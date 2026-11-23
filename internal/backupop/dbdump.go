package backupop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// db 维度的执行边界：
//   - 只导出显式声明的单个库（库名走标识符白名单，不接受任意 SQL / 多余参数）；
//   - 连接信息来自插件配置（config.json，0600），密码只经 MYSQL_PWD 环境变量传给子进程，
//     **绝不进 argv**，也不写日志 / 结果；
//   - 导出文件落在备份目录下的临时 staging 目录（0700），打成 db/<database>.sql 后删除；
//   - 子进程超时 30 分钟（与内核单任务硬上限同口径），不做后台常驻。
const (
	// dbDumpTimeout 是单库导出的硬超时。
	dbDumpTimeout = 30 * time.Minute
	// dbDumpStderrLimit 是保留的 stderr 上限，避免把大段输出写进错误与日志。
	dbDumpStderrLimit = 4096
	// dbStagingPrefix 是导出临时目录前缀（位于备份目录内，归档完成后删除）。
	dbStagingPrefix = ".stage-"
)

// dumpEnvAllowlist 是透传给 mysqldump 的环境变量白名单：只保留定位客户端与本地化所需项。
var dumpEnvAllowlist = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TZ": true,
}

// stagedFile 是已导出到 staging 的待归档文件。
type stagedFile struct {
	path string
	rel  string
	size int64
}

// validateDatabaseName 校验库名：字母数字下划线美元连字符，≤64，且不能以 - 开头（防参数注入）。
func validateDatabaseName(raw string) (string, *pluginapi.Error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", invalid("db 维度的 database 不能为空")
	}
	if len(name) > 64 {
		return "", invalid("database 超过 64 字符上限")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '$':
		case r == '-':
			if i == 0 {
				return "", invalid("database 不能以 - 开头")
			}
		default:
			return "", invalid("database 只允许字母、数字、下划线、美元符号与连字符")
		}
	}
	return name, nil
}

// ValidateMySQLConfig 校验 db 维度连接配置；错误信息不包含密码。
func ValidateMySQLConfig(cfg MySQLConfig) *pluginapi.Error {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return invalid("db 维度需要先配置数据库主机（db_host）")
	}
	if len(host) > 255 || strings.ContainsAny(host, " \t\r\n\x00") {
		return invalid("db_host 非法：不能含空白或控制字符")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return invalid("db_port 必须在 1–65535 之间")
	}
	user := strings.TrimSpace(cfg.User)
	if user == "" {
		return invalid("db 维度需要先配置数据库用户（db_user）")
	}
	if len(user) > 64 || strings.ContainsAny(user, " \t\r\n\x00") || strings.HasPrefix(user, "-") {
		return invalid("db_user 非法：不能含空白、控制字符或以 - 开头")
	}
	if strings.ContainsRune(cfg.Password, 0) || len(cfg.Password) > 1024 {
		return invalid("db_password 非法")
	}
	if strings.TrimSpace(cfg.DumpBin) == "" || strings.ContainsAny(cfg.DumpBin, "\x00\r\n") {
		return invalid("db_dump_bin 非法：不能为空或含控制字符")
	}
	return nil
}

// dumpDatabase 用 mysqldump 导出一个库到 staging，返回待归档文件与实测阶段。
func dumpDatabase(cfg MySQLConfig, db DBSpec, stagingRoot string, progress func(entries int, bytes int64)) (stagedFile, JobStage, error) {
	started := time.Now()
	stage := JobStage{Name: "db_dump", StartedAt: started.UTC().Format(time.RFC3339)}
	fail := func(err error) (stagedFile, JobStage, error) {
		stage.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		stage.DurationMS = time.Since(started).Milliseconds()
		return stagedFile{}, stage, err
	}
	database, perr := validateDatabaseName(db.Database)
	if perr != nil {
		return fail(newOpError(ErrInvalid, perr.Message))
	}
	if perr := ValidateMySQLConfig(cfg); perr != nil {
		return fail(newOpError(ErrInvalid, perr.Message))
	}
	outDir := filepath.Join(stagingRoot, "db")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fail(newOpError(ErrInternal, "创建导出临时目录失败: "+err.Error()))
	}
	outPath := filepath.Join(outDir, database+".sql")
	file, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(newOpError(ErrInternal, "创建导出文件失败: "+err.Error()))
	}
	timeoutCtx, cancel := context.WithTimeout(context.Background(), dbDumpTimeout)
	defer cancel()
	args := dumpArgs(cfg, database)
	command := exec.CommandContext(timeoutCtx, cfg.DumpBin, args...)
	command.Env = dumpEnv(cfg.Password)
	var stderr bytes.Buffer
	command.Stderr = &limitedWriter{buffer: &stderr, remaining: dbDumpStderrLimit}
	command.Stdout = file
	runErr := command.Run()
	closeErr := file.Close()
	if runErr != nil {
		return fail(dumpError(runErr, timeoutCtx, stderr.String()))
	}
	if closeErr != nil {
		return fail(newOpError(ErrInternal, "关闭导出文件失败: "+closeErr.Error()))
	}
	info, err := os.Stat(outPath)
	if err != nil {
		return fail(newOpError(ErrInternal, "读取导出文件大小失败: "+err.Error()))
	}
	stage.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	stage.DurationMS = time.Since(started).Milliseconds()
	stage.Entries = 1
	stage.Bytes = info.Size()
	if progress != nil {
		progress(1, info.Size())
	}
	return stagedFile{path: outPath, rel: "db/" + database + ".sql", size: info.Size()}, stage, nil
}

// dumpArgs 构造 mysqldump 参数；密码绝不出现，值都以独立 argv 传递。
func dumpArgs(cfg MySQLConfig, database string) []string {
	return []string{
		"--host", strings.TrimSpace(cfg.Host),
		"--port", strconv.Itoa(cfg.Port),
		"--user", strings.TrimSpace(cfg.User),
		"--single-transaction",
		"--quick",
		"--skip-lock-tables",
		"--no-tablespaces",
		"--default-character-set=utf8mb4",
		"--databases", database,
	}
}

// dumpEnv 只透传白名单环境变量，并把密码经 MYSQL_PWD 注入（旧值一律剔除）。
func dumpEnv(password string) []string {
	env := make([]string, 0, 16)
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if !ok || !dumpEnvAllowlist[key] {
			continue
		}
		env = append(env, item)
	}
	if password != "" {
		env = append(env, "MYSQL_PWD="+password)
	}
	return env
}

// dumpError 把子进程失败映射为可分类错误；stderr 截断并压成单行。
func dumpError(runErr error, ctx context.Context, stderr string) error {
	detail := strings.Join(strings.Fields(stderr), " ")
	if len(detail) > 300 {
		detail = detail[:300]
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return newOpError(ErrUpstream, fmt.Sprintf("mysqldump 超过 %s 未完成，已终止", dbDumpTimeout))
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		message := "mysqldump 退出码 " + strconv.Itoa(exitErr.ExitCode())
		if detail != "" {
			message += ": " + detail
		}
		return newOpError(ErrUpstream, message)
	}
	if errors.Is(runErr, exec.ErrNotFound) || errors.Is(runErr, os.ErrNotExist) {
		return newOpError(ErrUnsupported,
			"未找到 mysqldump 客户端：安装 mysql-client/mariadb-client，或把 db_dump_bin 指向可执行文件绝对路径")
	}
	return newOpError(ErrInternal, "启动 mysqldump 失败: "+runErr.Error())
}

// limitedWriter 是带上限的 stderr 收集器：超过上限后静默丢弃后续内容。
type limitedWriter struct {
	buffer    *bytes.Buffer
	remaining int
}

// Write 实现 io.Writer。
func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return len(p), nil
	}
	chunk := p
	if len(chunk) > w.remaining {
		chunk = chunk[:w.remaining]
	}
	w.buffer.Write(chunk)
	w.remaining -= len(chunk)
	return len(p), nil
}

// 编译期确认 limitedWriter 兼容 io.Writer（command.Stderr 的静态类型要求）。
var _ io.Writer = (*limitedWriter)(nil)
