package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore-plugin-backup/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// newRuntimeConfig 构造带配置存储的运行期配置（不读环境变量）。
func newRuntimeConfig(t *testing.T) *config {
	t.Helper()
	base := t.TempDir()
	dir := defaultBackupDir(base)
	cfg := &config{
		id:       "backup",
		stateDir: base,
		store:    plugincfg.NewStore(base),
		values: backupValues{
			Dir: dir, AllowRoots: []string{base},
			DBHost: defaultDBHost, DBPort: defaultDBPort, DBUser: defaultDBUser, DBDumpBin: defaultDBDumpBin,
		},
		source:     plugincfg.SourceDefault,
		backupDir:  dir,
		allowRoots: []string{base},
	}
	return cfg
}

// TestConfigGetDefault 校验未落盘时 schema/values/source 形状。
func TestConfigGetDefault(t *testing.T) {
	cfg := newRuntimeConfig(t)
	resp, _ := dispatch(&pluginapi.Request{Method: "backup.config.get", Params: json.RawMessage(`{}`)}, cfg, testLogger())
	if resp.Error != nil {
		t.Fatalf("config.get 失败: %v", resp.Error)
	}
	var got plugincfg.GetResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("解析结果: %v", err)
	}
	if got.Source != plugincfg.SourceDefault || got.Schema.Title != "备份设置" || len(got.Schema.Fields) != 7 {
		t.Fatalf("结果不符: %+v", got)
	}
	var values backupValues
	if err := json.Unmarshal(got.Values, &values); err != nil {
		t.Fatalf("values: %v", err)
	}
	if values.Dir != cfg.backupDir || len(values.AllowRoots) != 1 {
		t.Fatalf("values=%+v", values)
	}
	if values.DBHost != defaultDBHost || values.DBPort != defaultDBPort || values.DBDumpBin != defaultDBDumpBin {
		t.Fatalf("db 默认值未回填: %+v", values)
	}
	if values.DBPassword != plugincfg.SensitivePlaceholder {
		t.Fatalf("db_password 应被掩码为占位值，实际 %q", values.DBPassword)
	}
}

// TestConfigSetHotApply 校验保存后下一次 backup.create 立即使用新目录与白名单，无需重启。
func TestConfigSetHotApply(t *testing.T) {
	cfg := newRuntimeConfig(t)
	newDir := filepath.Join(t.TempDir(), "backups")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte("kore"), 0o600); err != nil {
		t.Fatalf("造数: %v", err)
	}
	body, _ := json.Marshal(map[string]any{"values": map[string]any{"dir": newDir, "allow_roots": []string{root}}})
	resp, _ := dispatch(&pluginapi.Request{Method: "backup.config.set", Params: body}, cfg, testLogger())
	if resp.Error != nil {
		t.Fatalf("config.set 失败: %v", resp.Error)
	}
	var set plugincfg.SetResult
	if err := json.Unmarshal(resp.Result, &set); err != nil {
		t.Fatalf("解析 set: %v", err)
	}
	if !set.Saved || !set.Applied || set.RestartRequired {
		t.Fatalf("set 结果=%+v", set)
	}
	info, err := os.Stat(cfg.store.Path())
	if err != nil {
		t.Fatalf("配置文件未落盘: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("配置权限=%o，期望 600", info.Mode().Perm())
	}
	// 直接调用业务方法（不重建 config），确认新目录已生效。
	call(t, *cfg, "backup.create", `{"name":"cfg-hot","paths":[`+jsonQuote(root)+`]}`)
	entries, err := os.ReadDir(newDir)
	if err != nil {
		t.Fatalf("新备份目录不可读: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("备份未写入新目录")
	}
}

// TestConfigPriorityFileOverEnv 校验 config.json > KORE_BACKUP_*。
func TestConfigPriorityFileOverEnv(t *testing.T) {
	base := t.TempDir()
	store := plugincfg.NewStore(base)
	fileDir := filepath.Join(base, "from-file")
	if err := store.Write(mustJSON(t, backupValues{Dir: fileDir, AllowRoots: []string{base}})); err != nil {
		t.Fatalf("预置配置: %v", err)
	}
	t.Setenv("KORE_BACKUP_DIR", filepath.Join(base, "from-env"))
	t.Setenv("KORE_BACKUP_ALLOW_ROOTS", "/tmp")
	values, source, err := resolveBackupValues(store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if source != plugincfg.SourceFile || values.Dir != fileDir {
		t.Fatalf("source=%q dir=%q", source, values.Dir)
	}
	if len(values.AllowRoots) != 1 || values.AllowRoots[0] != base {
		t.Fatalf("roots=%v", values.AllowRoots)
	}
}

// TestConfigEnvCompat 校验老用户环境变量仍生效（source=env，默认白名单两项）。
func TestConfigEnvCompat(t *testing.T) {
	store := plugincfg.NewStore(t.TempDir())
	t.Setenv("KORE_BACKUP_DIR", "/tmp/kore-env-backups")
	t.Setenv("KORE_BACKUP_ALLOW_ROOTS", "/tmp:/etc/kore")
	values, source, err := resolveBackupValues(store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if source != plugincfg.SourceEnv || values.Dir != "/tmp/kore-env-backups" {
		t.Fatalf("source=%q values=%+v", source, values)
	}
	if len(values.AllowRoots) != 2 || values.AllowRoots[1] != "/etc/kore" {
		t.Fatalf("roots=%v", values.AllowRoots)
	}
}

// TestConfigSetRejectsInvalid 校验非法值返回 E_INVALID 且不写盘、不改内存。
func TestConfigSetRejectsInvalid(t *testing.T) {
	cfg := newRuntimeConfig(t)
	oldDir := cfg.backupDir
	body := json.RawMessage(`{"values":{"allow_roots":["relative/path"]}}`)
	resp, _ := dispatch(&pluginapi.Request{Method: "backup.config.set", Params: body}, cfg, testLogger())
	if resp.Error == nil || resp.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("期望 E_INVALID，得到 %+v", resp.Error)
	}
	if cfg.backupDir != oldDir || cfg.source != plugincfg.SourceDefault {
		t.Fatalf("失败后内存被改动: %+v", cfg.values)
	}
	if _, err := os.Stat(cfg.store.Path()); !os.IsNotExist(err) {
		t.Fatalf("失败后不应写配置文件，err=%v", err)
	}
}

// mustJSON 把测试值序列化为 JSON；失败直接终止测试。
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	return data
}

// TestConfigSetSensitivePlaceholderKeepsPassword 校验掩码占位值不会把密码写坏。
func TestConfigSetSensitivePlaceholderKeepsPassword(t *testing.T) {
	cfg := newRuntimeConfig(t)
	cfg.values.DBPassword = "old-secret"
	body := json.RawMessage(`{"values":{"db_password":"***","db_user":"kore"}}`)
	resp, _ := dispatch(&pluginapi.Request{Method: "backup.config.set", Params: body}, cfg, testLogger())
	if resp.Error != nil {
		t.Fatalf("config.set 失败: %+v", resp.Error)
	}
	raw, ok, err := cfg.store.Read()
	if err != nil || !ok {
		t.Fatalf("读配置: ok=%v err=%v", ok, err)
	}
	var stored backupValues
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("解析落盘配置: %v", err)
	}
	if stored.DBPassword != "old-secret" || stored.DBUser != "kore" {
		t.Fatalf("占位值把密码/用户写坏: %+v", stored)
	}
	if cfg.values.DBPassword != "old-secret" {
		t.Fatalf("运行期密码被占位值覆盖: %q", cfg.values.DBPassword)
	}
}

// TestConfigGetNeverLeaksPassword 校验 config.get 永远不回显 db_password 原值。
func TestConfigGetNeverLeaksPassword(t *testing.T) {
	cfg := newRuntimeConfig(t)
	cfg.values.DBPassword = "top-secret"
	resp, _ := dispatch(&pluginapi.Request{Method: "backup.config.get", Params: json.RawMessage(`{}`)}, cfg, testLogger())
	if resp.Error != nil {
		t.Fatalf("config.get: %+v", resp.Error)
	}
	if strings.Contains(string(resp.Result), "top-secret") {
		t.Fatalf("config.get 泄露密码: %s", resp.Result)
	}
	if !strings.Contains(string(resp.Result), `"db_password":"***"`) {
		t.Fatalf("db_password 未掩码: %s", resp.Result)
	}
}

// TestConfigFileCompatOldShape 校验 0.3.0 的旧配置文件（无 db_* 字段）仍能加载并补默认值。
func TestConfigFileCompatOldShape(t *testing.T) {
	base := t.TempDir()
	store := plugincfg.NewStore(base)
	dir := filepath.Join(base, "backups")
	if err := store.Write(json.RawMessage(`{"dir":"` + dir + `","allow_roots":["` + base + `"]}`)); err != nil {
		t.Fatalf("预置旧配置: %v", err)
	}
	values, source, err := resolveBackupValues(store)
	if err != nil {
		t.Fatalf("加载旧配置: %v", err)
	}
	if source != plugincfg.SourceFile || values.Dir != dir {
		t.Fatalf("source=%q dir=%q", source, values.Dir)
	}
	if values.DBHost != defaultDBHost || values.DBPort != defaultDBPort ||
		values.DBUser != defaultDBUser || values.DBDumpBin != defaultDBDumpBin {
		t.Fatalf("旧配置未补 db 默认值: %+v", values)
	}
}

// TestConfigEnvDBFields 校验 db 连接可用环境变量配置（无配置文件时）。
func TestConfigEnvDBFields(t *testing.T) {
	store := plugincfg.NewStore(t.TempDir())
	t.Setenv("KORE_BACKUP_DB_HOST", "db.internal")
	t.Setenv("KORE_BACKUP_DB_PORT", "3307")
	t.Setenv("KORE_BACKUP_DB_USER", "kore")
	t.Setenv("KORE_BACKUP_DB_PASSWORD", "env-secret")
	t.Setenv("KORE_BACKUP_MYSQLDUMP_BIN", "/usr/bin/mysqldump-custom")
	values, source, err := resolveBackupValues(store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if source != plugincfg.SourceEnv {
		t.Fatalf("source=%q，期望 env", source)
	}
	if values.DBHost != "db.internal" || values.DBPort != 3307 || values.DBUser != "kore" ||
		values.DBPassword != "env-secret" || values.DBDumpBin != "/usr/bin/mysqldump-custom" {
		t.Fatalf("env db 字段未生效: %+v", values)
	}
}

// TestConfigRejectsBadDBPort 校验 db_port 越界返回 E_INVALID 且不写盘。
func TestConfigRejectsBadDBPort(t *testing.T) {
	cfg := newRuntimeConfig(t)
	body := json.RawMessage(`{"values":{"db_port":70000}}`)
	resp, _ := dispatch(&pluginapi.Request{Method: "backup.config.set", Params: body}, cfg, testLogger())
	if resp.Error == nil || resp.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("期望 E_INVALID，得到 %+v", resp.Error)
	}
	if _, err := os.Stat(cfg.store.Path()); !os.IsNotExist(err) {
		t.Fatalf("非法配置不应写盘，err=%v", err)
	}
}
