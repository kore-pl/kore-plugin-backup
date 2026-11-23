package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
	"github.com/pengtang1024/kore-plugin-backup/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// embeddedInfo 是随二进制发布的内嵌清单：只含握手字段（manifest.json），不含会随打包回填的 size_bytes。
//
//go:embed manifest.json
var embeddedInfo []byte

// manifest 是 info.json 中插件运行期真正需要的字段，其余字段归内核校验。
type manifest struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Protocol     string   `json:"protocol"`
	RequiresKore string   `json:"requires_kore"`
	Capabilities []string `json:"capabilities"`
}

// backupValues 是 <state_dir>/plugins/backup/config.json 的显式形状（G-24）。
//
// db_* 是 db 维度的连接配置：db_password 属敏感字段（0600 落盘、config.get 掩码），
// 只经 MYSQL_PWD 传给 mysqldump，不进 argv / 日志 / 方法结果。
type backupValues struct {
	Dir        string   `json:"dir"`
	AllowRoots []string `json:"allow_roots"`
	DBHost     string   `json:"db_host"`
	DBPort     int      `json:"db_port"`
	DBUser     string   `json:"db_user"`
	DBPassword string   `json:"db_password"`
	DBDumpBin  string   `json:"db_dump_bin"`
}

// db 维度连接配置的代码默认值（环境变量可覆盖，config.json 优先）。
const (
	defaultDBHost    = "127.0.0.1"
	defaultDBPort    = 3306
	defaultDBUser    = "root"
	defaultDBDumpBin = "mysqldump"
)

// mysqlConfig 把运行期配置转成 backupop 的 db 连接形状。
func (c config) mysqlConfig() backupop.MySQLConfig {
	return backupop.MySQLConfig{
		Host:     c.values.DBHost,
		Port:     c.values.DBPort,
		User:     c.values.DBUser,
		Password: c.values.DBPassword,
		DumpBin:  c.values.DBDumpBin,
	}
}

// config 是插件运行期配置：配置契约（config.json）+ 环境变量 + 内嵌清单。
type config struct {
	socket       string
	id           string
	token        string
	version      string
	requiresKore string
	capabilities []string
	startedAt    time.Time
	stateDir     string
	store        *plugincfg.Store
	values       backupValues
	source       string
	backupDir    string
	allowRoots   []string
}

// configDefaultAllowRoots 是配置契约的默认白名单（与 docs/PLUGIN-CONFIG.md §7.2 一致）。
var configDefaultAllowRoots = []string{"/var/www", "/etc/kore"}

// loadConfig 读取配置契约文件与环境变量，并按 file > env > default 解析。
func loadConfig() (config, error) {
	var m manifest
	if err := json.Unmarshal(embeddedInfo, &m); err != nil {
		return config{}, fmt.Errorf("解析内嵌 manifest.json: %w", err)
	}
	if m.Protocol != pluginapi.Protocol {
		return config{}, fmt.Errorf("info.json.protocol=%q 与 SDK 协议 %q 不一致", m.Protocol, pluginapi.Protocol)
	}
	stateDir := strings.TrimSpace(os.Getenv("KORE_STATE_DIR"))
	store := plugincfg.NewStore(stateDir)
	values, source, err := resolveBackupValues(store)
	if err != nil {
		return config{}, err
	}
	if err := plugincfg.ValidateSchema(backupSchema(values.Dir)); err != nil {
		return config{}, fmt.Errorf("config schema 非法: %w", err)
	}
	cfg := config{
		socket:       os.Getenv("KORE_PLUGIN_SOCK"),
		id:           os.Getenv("KORE_PLUGIN_ID"),
		token:        os.Getenv("KORE_PLUGIN_TOKEN"),
		version:      m.Version,
		requiresKore: m.RequiresKore,
		capabilities: m.Capabilities,
		startedAt:    time.Now(),
		stateDir:     stateDir,
		store:        store,
		values:       values,
		source:       source,
		backupDir:    values.Dir,
		allowRoots:   values.AllowRoots,
	}
	if cfg.id == "" {
		cfg.id = m.ID
	}
	if cfg.socket == "" {
		return config{}, fmt.Errorf("缺少环境变量 KORE_PLUGIN_SOCK")
	}
	return cfg, nil
}

// resolveBackupValues 实现优先级 config.json > KORE_BACKUP_* > 代码默认值。
func resolveBackupValues(store *plugincfg.Store) (backupValues, string, error) {
	if store.Exists() {
		raw, _, err := store.Read()
		if err != nil {
			return backupValues{}, "", err
		}
		var values backupValues
		if err := strictDecode(raw, &values); err != nil {
			return backupValues{}, "", fmt.Errorf("解析插件配置 %s: %w", store.Path(), err)
		}
		normalized, err := validateBackupValues(values)
		if err != nil {
			return backupValues{}, "", fmt.Errorf("插件配置 %s 非法: %w", store.Path(), err)
		}
		return normalized, plugincfg.SourceFile, nil
	}
	dir := resolveBackupDir()
	roots := append([]string(nil), configDefaultAllowRoots...)
	dbValues := backupValues{
		DBHost:    envOrDefault("KORE_BACKUP_DB_HOST", defaultDBHost),
		DBPort:    defaultDBPort,
		DBUser:    envOrDefault("KORE_BACKUP_DB_USER", defaultDBUser),
		DBDumpBin: envOrDefault("KORE_BACKUP_MYSQLDUMP_BIN", defaultDBDumpBin),
	}
	if rawPort := strings.TrimSpace(os.Getenv("KORE_BACKUP_DB_PORT")); rawPort != "" {
		port, convErr := strconv.Atoi(rawPort)
		if convErr != nil {
			return backupValues{}, "", fmt.Errorf("KORE_BACKUP_DB_PORT 不是整数: %s", rawPort)
		}
		dbValues.DBPort = port
	}
	if password := os.Getenv("KORE_BACKUP_DB_PASSWORD"); password != "" {
		dbValues.DBPassword = password
	}
	source := plugincfg.SourceDefault
	if strings.TrimSpace(os.Getenv("KORE_BACKUP_DIR")) != "" {
		source = plugincfg.SourceEnv
	}
	if anyDBEnvSet() {
		source = plugincfg.SourceEnv
	}
	if raw := os.Getenv("KORE_BACKUP_ALLOW_ROOTS"); strings.TrimSpace(raw) != "" {
		parsed, err := parseAllowRoots(raw)
		if err != nil {
			return backupValues{}, "", err
		}
		roots = parsed
		source = plugincfg.SourceEnv
	}
	dbValues.Dir, dbValues.AllowRoots = dir, roots
	normalized, err := validateBackupValues(withDBDefaults(dbValues))
	if err != nil {
		return backupValues{}, "", err
	}
	return normalized, source, nil
}

// anyDBEnvSet 报告是否设置了任一 db 维度环境变量（用于 config.get 的 source 标注）。
func anyDBEnvSet() bool {
	for _, key := range []string{
		"KORE_BACKUP_DB_HOST", "KORE_BACKUP_DB_PORT", "KORE_BACKUP_DB_USER",
		"KORE_BACKUP_DB_PASSWORD", "KORE_BACKUP_MYSQLDUMP_BIN",
	} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

// envOrDefault 返回环境变量值（去空白），为空时用默认值。
func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// withDBDefaults 给空的 db 字段补默认值：兼容 0.3.0 及更早的配置文件（无 db_* 字段）。
func withDBDefaults(values backupValues) backupValues {
	if strings.TrimSpace(values.DBHost) == "" {
		values.DBHost = defaultDBHost
	}
	if values.DBPort == 0 {
		values.DBPort = defaultDBPort
	}
	if strings.TrimSpace(values.DBUser) == "" {
		values.DBUser = defaultDBUser
	}
	if strings.TrimSpace(values.DBDumpBin) == "" {
		values.DBDumpBin = envOrDefault("KORE_BACKUP_MYSQLDUMP_BIN", defaultDBDumpBin)
	}
	return values
}

// validateBackupValues 校验并归一化 dir / allow_roots；写盘前与启动加载复用同一套规则。
func validateBackupValues(values backupValues) (backupValues, error) {
	dir, err := validateDir(values.Dir)
	if err != nil {
		return backupValues{}, err
	}
	roots, err := normalizeAllowRoots(values.AllowRoots)
	if err != nil {
		return backupValues{}, err
	}
	values = withDBDefaults(values)
	if err := backupop.ValidateMySQLConfig(backupop.MySQLConfig{
		Host: values.DBHost, Port: values.DBPort, User: values.DBUser,
		Password: values.DBPassword, DumpBin: values.DBDumpBin,
	}); err != nil {
		return backupValues{}, fmt.Errorf("%s", err.Message)
	}
	return backupValues{
		Dir: dir, AllowRoots: roots,
		DBHost: values.DBHost, DBPort: values.DBPort, DBUser: values.DBUser,
		DBPassword: values.DBPassword, DBDumpBin: values.DBDumpBin,
	}, nil
}

// validateDir 校验备份目录：必须是绝对路径、Clean 后无变化、不含 NUL。
func validateDir(raw string) (string, error) {
	dir := strings.TrimSpace(raw)
	if dir == "" {
		return "", fmt.Errorf("备份目录不能为空")
	}
	if strings.ContainsRune(dir, 0) {
		return "", fmt.Errorf("备份目录含 NUL 字节")
	}
	if strings.Contains(dir, "\\") {
		return "", fmt.Errorf("备份目录含反斜杠")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("备份目录必须是绝对路径：%s", dir)
	}
	if filepath.Clean(dir) != dir {
		return "", fmt.Errorf("备份目录必须是 Clean 后的路径：%s", dir)
	}
	return dir, nil
}

// normalizeAllowRoots 校验白名单：非空、绝对、Clean、无重复。
func normalizeAllowRoots(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("允许的根路径不能为空")
	}
	roots := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for i, item := range raw {
		root := strings.TrimSpace(item)
		if root == "" {
			return nil, fmt.Errorf("第 %d 个允许根路径为空", i+1)
		}
		if strings.ContainsRune(root, 0) {
			return nil, fmt.Errorf("第 %d 个允许根路径含 NUL 字节", i+1)
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("第 %d 个允许根路径必须是绝对路径：%s", i+1, root)
		}
		if filepath.Clean(root) != root {
			return nil, fmt.Errorf("第 %d 个允许根路径必须是 Clean 后的路径：%s", i+1, root)
		}
		if seen[root] {
			return nil, fmt.Errorf("第 %d 个允许根路径重复：%s", i+1, root)
		}
		seen[root] = true
		roots = append(roots, root)
	}
	return roots, nil
}

// strictDecode 解码 JSON 并拒绝未知字段与尾随内容。
func strictDecode(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("JSON 对象之后存在多余内容")
	}
	return nil
}

// defaultBackupDir 返回代码默认备份目录：<state>/backups（state 为空时 /var/lib/kore）。
func defaultBackupDir(stateDir string) string {
	state := strings.TrimSpace(stateDir)
	if state == "" {
		state = "/var/lib/kore"
	}
	return filepath.Join(filepath.Clean(state), "backups")
}

// resolveBackupDir 解析环境变量阶段的备份目录：KORE_BACKUP_DIR 优先，否则默认目录。
func resolveBackupDir() string {
	if dir := strings.TrimSpace(os.Getenv("KORE_BACKUP_DIR")); dir != "" {
		return filepath.Clean(dir)
	}
	return defaultBackupDir(os.Getenv("KORE_STATE_DIR"))
}

// parseAllowRoots 解析 KORE_BACKUP_ALLOW_ROOTS：支持系统路径分隔符与逗号两种写法。
func parseAllowRoots(raw string) ([]string, error) {
	replaced := strings.ReplaceAll(raw, ",", string(os.PathListSeparator))
	return normalizeAllowRoots(filepath.SplitList(replaced))
}

// backupSchema 是 config.get 返回的表单描述。
func backupSchema(defaultDir string) plugincfg.Schema {
	dirDefault, _ := json.Marshal(defaultDir)
	rootsDefault, _ := json.Marshal(configDefaultAllowRoots)
	return plugincfg.Schema{
		Title: "备份设置",
		Fields: []plugincfg.Field{
			{
				Key:         "dir",
				Label:       "备份目录",
				Type:        plugincfg.TypeString,
				Default:     dirDefault,
				Required:    true,
				Help:        "备份包与 .sha256 / .meta.json 的落盘目录，必须是绝对路径；例：/var/lib/kore/backups",
				Placeholder: "/var/lib/kore/backups",
			},
			{
				Key:         "allow_roots",
				Label:       "允许的根路径",
				Type:        plugincfg.TypeList,
				Item:        plugincfg.TypeString,
				Default:     rootsDefault,
				Required:    true,
				Help:        "备份源路径必须位于这些目录之下；每项一个绝对路径，例：/var/www、/etc/kore",
				Placeholder: "/var/www",
			},
			{
				Key:         "db_host",
				Label:       "数据库主机",
				Type:        plugincfg.TypeString,
				Default:     json.RawMessage(`"` + defaultDBHost + `"`),
				Required:    true,
				Help:        "db 维度（MySQL 库备份）使用的主机；只在勾选 db 维度时生效",
				Placeholder: defaultDBHost,
			},
			{
				Key:         "db_port",
				Label:       "数据库端口",
				Type:        plugincfg.TypeInt,
				Default:     json.RawMessage(`3306`),
				Required:    true,
				Min:         intPtr(1),
				Max:         intPtr(65535),
				Help:        "MySQL 端口，默认 3306",
				Placeholder: "3306",
			},
			{
				Key:         "db_user",
				Label:       "数据库用户",
				Type:        plugincfg.TypeString,
				Default:     json.RawMessage(`"` + defaultDBUser + `"`),
				Required:    true,
				Help:        "只读权限即可导出（需 SELECT、LOCK TABLES、SHOW VIEW、TRIGGER、EVENT）",
				Placeholder: defaultDBUser,
			},
			{
				Key:       "db_password",
				Label:     "数据库密码",
				Type:      plugincfg.TypeString,
				Default:   json.RawMessage(`""`),
				Required:  false,
				Help:      "只写不回显；留空表示无密码。密码只经 MYSQL_PWD 传给 mysqldump，不进命令行参数",
				Sensitive: true,
			},
			{
				Key:         "db_dump_bin",
				Label:       "mysqldump 路径",
				Type:        plugincfg.TypeString,
				Default:     json.RawMessage(`"` + defaultDBDumpBin + `"`),
				Required:    true,
				Help:        "mysqldump 可执行文件路径或命令名；也可用环境变量 KORE_BACKUP_MYSQLDUMP_BIN 覆盖",
				Placeholder: defaultDBDumpBin,
			},
		},
	}
}

// intPtr 返回指向整数的指针，供 schema 的 min/max 使用。
func intPtr(v int) *int { return &v }
