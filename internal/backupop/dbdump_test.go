package backupop

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// writeDumpStub 在临时目录里造一个假 mysqldump：把 argv 与 MYSQL_PWD 落盘，再输出假 SQL。
// 路径直接写进脚本，避免依赖被白名单过滤掉的环境变量。
func writeDumpStub(t *testing.T, dir, body string, exitCode int) (bin, argsPath, pwdPath string) {
	t.Helper()
	bin = filepath.Join(dir, "mysqldump")
	argsPath = filepath.Join(dir, "args.txt")
	pwdPath = filepath.Join(dir, "pwd.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\n" +
		"printf '%s' \"${MYSQL_PWD:-}\" > " + shellQuote(pwdPath) + "\n" +
		body + "\n" +
		"exit " + itoa(int64(exitCode)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("写假 mysqldump: %v", err)
	}
	return bin, argsPath, pwdPath
}

// shellQuote 做最小单引号转义，测试路径都是临时目录里的普通路径。
func shellQuote(raw string) string {
	return "'" + strings.ReplaceAll(raw, "'", `'\''`) + "'"
}

// readTarEntries 读出归档内全部常规文件内容（测试专用）。
func readTarEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开归档: %v", err)
	}
	defer func() { _ = file.Close() }()
	var reader io.Reader = file
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(file)
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	entries := map[string]string{}
	tr := tar.NewReader(reader)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读 tar: %v", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("读条目 %s: %v", header.Name, err)
		}
		entries[header.Name] = string(data)
	}
	return entries
}

// TestCreateDBDimension 校验 db 维度：密码只经 MYSQL_PWD、不进 argv，.sql 打进 db/<database>.sql。
func TestCreateDBDimension(t *testing.T) {
	workspace := t.TempDir()
	stubDir := filepath.Join(workspace, "stub")
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		t.Fatalf("建 stub 目录: %v", err)
	}
	bin, argsPath, pwdPath := writeDumpStub(t, stubDir, `printf 'CREATE TABLE t (id int);\n'`, 0)
	opts := Options{
		Dir:        filepath.Join(workspace, "backups"),
		AllowRoots: []string{workspace},
		DB:         MySQLConfig{Host: "127.0.0.1", Port: 3306, User: "backup", Password: "s3cr3t", DumpBin: bin},
	}
	raw, perr := Call("backup.create",
		json.RawMessage(`{"name":"dbonly","dimensions":["db"],"db":{"database":"shop"}}`), opts, nil)
	if perr != nil {
		t.Fatalf("create db: %v", perr)
	}
	var create struct {
		Path   string     `json:"path"`
		Job    *JobRecord `json:"job"`
		Stages []JobStage `json:"stages"`
	}
	if err := json.Unmarshal(raw, &create); err != nil {
		t.Fatalf("解析 create: %v", err)
	}
	entries := readTarEntries(t, create.Path)
	if got := entries["db/shop.sql"]; !strings.Contains(got, "CREATE TABLE") {
		t.Fatalf("归档内缺少 db/shop.sql 或内容不对: %q", got)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("读 argv: %v", err)
	}
	argText := string(args)
	if strings.Contains(argText, "s3cr3t") {
		t.Fatalf("密码出现在 argv 中: %q", argText)
	}
	for _, want := range []string{"--databases", "shop", "--single-transaction", "--default-character-set=utf8mb4"} {
		if !strings.Contains(argText, want) {
			t.Fatalf("argv 缺少 %q: %q", want, argText)
		}
	}
	pwd, err := os.ReadFile(pwdPath)
	if err != nil || string(pwd) != "s3cr3t" {
		t.Fatalf("MYSQL_PWD 注入不对: %q err=%v", pwd, err)
	}
	if len(create.Stages) != 2 || create.Stages[0].Name != "db_dump" || create.Stages[1].Name != "pack" {
		t.Fatalf("阶段不对: %+v", create.Stages)
	}
}

// TestCreateSiteAndDBCombined 校验 site + db 组合：两类条目进同一个归档。
func TestCreateSiteAndDBCombined(t *testing.T) {
	workspace := t.TempDir()
	source := prepareSource(t, workspace)
	stubDir := filepath.Join(workspace, "stub")
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		t.Fatalf("建 stub 目录: %v", err)
	}
	bin, _, _ := writeDumpStub(t, stubDir, `printf 'SELECT 1;\n'`, 0)
	opts := Options{
		Dir:        filepath.Join(workspace, "backups"),
		AllowRoots: []string{workspace},
		DB:         MySQLConfig{Host: "localhost", Port: 3307, User: "u", Password: "", DumpBin: bin},
	}
	params := `{"name":"both","dimensions":["site","db"],"paths":[` + quote(source) + `],"db":{"database":"shop"}}`
	raw, perr := Call("backup.create", json.RawMessage(params), opts, nil)
	if perr != nil {
		t.Fatalf("create combined: %v", perr)
	}
	var create struct {
		Path       string   `json:"path"`
		Dimensions []string `json:"dimensions"`
	}
	if err := json.Unmarshal(raw, &create); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(create.Dimensions) != 2 || create.Dimensions[0] != DimensionSite || create.Dimensions[1] != DimensionDB {
		t.Fatalf("维度回显不对: %+v", create.Dimensions)
	}
	entries := readTarEntries(t, create.Path)
	if _, ok := entries["src/a.txt"]; !ok {
		t.Fatalf("组合备份缺少站点条目: %v", entries)
	}
	if _, ok := entries["db/shop.sql"]; !ok {
		t.Fatalf("组合备份缺少 db 条目: %v", entries)
	}
}

// TestCreateDBRejects 校验 db 维度参数与连接配置边界，非法值不落到子进程。
func TestCreateDBRejects(t *testing.T) {
	workspace := t.TempDir()
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	cases := []string{
		`{"name":"db","dimensions":["db"]}`,
		`{"name":"db","dimensions":["db"],"db":{"database":""}}`,
		`{"name":"db","dimensions":["db"],"db":{"database":"shop; DROP TABLE t"}}`,
		`{"name":"db","dimensions":["db"],"db":{"database":"-evil"}}`,
		`{"name":"db","dimensions":["db"],"db":{"database":"shop"}}`,
		`{"name":"db","dimensions":["site"],"db":{"database":"shop"},"paths":[` + quote(workspace) + `]}`,
	}
	for _, params := range cases {
		if _, perr := Call("backup.create", json.RawMessage(params), opts, nil); perr == nil ||
			perr.Code != pluginapi.E_INVALID {
			t.Fatalf("params=%s 期望 E_INVALID，得到 %v", params, perr)
		}
	}
}

// TestCreateDBDumpFailure 校验 mysqldump 失败映射为 E_UPSTREAM 且不残留归档。
func TestCreateDBDumpFailure(t *testing.T) {
	workspace := t.TempDir()
	stubDir := filepath.Join(workspace, "stub")
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		t.Fatalf("建 stub 目录: %v", err)
	}
	bin, _, _ := writeDumpStub(t, stubDir, `printf 'Access denied for user backup\n' >&2`, 3)
	opts := Options{
		Dir:        filepath.Join(workspace, "backups"),
		AllowRoots: []string{workspace},
		DB:         MySQLConfig{Host: "127.0.0.1", Port: 3306, User: "backup", Password: "s3cr3t", DumpBin: bin},
	}
	_, perr := Call("backup.create",
		json.RawMessage(`{"name":"db","dimensions":["db"],"db":{"database":"shop"}}`), opts, nil)
	if perr == nil || perr.Code != pluginapi.E_UPSTREAM {
		t.Fatalf("期望 E_UPSTREAM，得到 %v", perr)
	}
	if !strings.Contains(perr.Message, "退出码 3") || !strings.Contains(perr.Message, "Access denied") {
		t.Fatalf("错误信息缺少退出码/stderr: %q", perr.Message)
	}
	if strings.Contains(perr.Message, "s3cr3t") {
		t.Fatalf("错误信息泄露密码: %q", perr.Message)
	}
	assertDirEmpty(t, opts.Dir)
}

// TestCreateDBMissingClient 校验找不到 mysqldump 时明确 E_UNSUPPORTED（不静默降级）。
func TestCreateDBMissingClient(t *testing.T) {
	workspace := t.TempDir()
	opts := Options{
		Dir:        filepath.Join(workspace, "backups"),
		AllowRoots: []string{workspace},
		DB: MySQLConfig{Host: "127.0.0.1", Port: 3306, User: "backup",
			DumpBin: filepath.Join(workspace, "no-such-mysqldump")},
	}
	_, perr := Call("backup.create",
		json.RawMessage(`{"name":"db","dimensions":["db"],"db":{"database":"shop"}}`), opts, nil)
	if perr == nil || perr.Code != pluginapi.E_UNSUPPORTED {
		t.Fatalf("期望 E_UNSUPPORTED，得到 %v", perr)
	}
}

// TestDumpArgsNeverContainPassword 直接断言参数构造不含密码（防止后续改动回退）。
func TestDumpArgsNeverContainPassword(t *testing.T) {
	cfg := MySQLConfig{Host: "127.0.0.1", Port: 3306, User: "backup", Password: "topsecret", DumpBin: "mysqldump"}
	for _, arg := range dumpArgs(cfg, "shop") {
		if strings.Contains(arg, cfg.Password) {
			t.Fatalf("argv 出现密码: %q", arg)
		}
	}
	env := strings.Join(dumpEnv(cfg.Password), "\n")
	if !strings.Contains(env, "MYSQL_PWD=topsecret") {
		t.Fatalf("MYSQL_PWD 未注入: %s", env)
	}
}
