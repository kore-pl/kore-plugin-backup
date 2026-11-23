package backupop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestBusinessVerb 方法名归一化：backup.<verb> 与裸 <verb> 都接受。
func TestBusinessVerb(t *testing.T) {
	cases := map[string]string{
		"backup.create": "create", "backup.list": "list", "backup.restore": "restore",
		"backup.delete": "delete", "backup.verify": "verify",
		"backup.retention": "retention", "backup.stats": "stats",
		"create": "create", "list": "list", "restore": "restore",
		"delete": "delete", "verify": "verify", "retention": "retention", "stats": "stats",
	}
	for method, want := range cases {
		got, ok := businessVerb(method)
		if !ok || got != want {
			t.Fatalf("businessVerb(%q)=(%q,%v)，期望 (%q,true)", method, got, ok, want)
		}
	}
	if _, ok := businessVerb("backup.exec"); ok {
		t.Fatal("backup.exec 不应被接受")
	}
}

// TestCallUnknownMethod 未知方法返回 E_NOTFOUND。
func TestCallUnknownMethod(t *testing.T) {
	_, perr := Call("backup.exec", json.RawMessage(`{}`), testOptions(t), nil)
	if perr == nil || perr.Code != pluginapi.E_NOTFOUND {
		t.Fatalf("未知方法错误=%v，期望 E_NOTFOUND", perr)
	}
}

// TestCallParamValidation 非法参数形状与 id 返回 E_INVALID。
func TestCallParamValidation(t *testing.T) {
	workspace := t.TempDir()
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	bad := []struct {
		method string
		params string
	}{
		{"backup.list", `[]`},
		{"backup.create", `not-json`},
		{"backup.create", `{"name":"snap","paths":[]}`},
		{"backup.create", `{"name":"../evil","paths":["` + workspace + `"]}`},
		{"backup.restore", `{"id":"../etc","target_dir":"` + workspace + `/dst"}`},
		{"backup.delete", `{"id":""}`},
		{"backup.verify", `{"id":"a/b"}`},
		{"backup.retention", `{"keep":0}`},
		{"backup.retention", `{"keep":101}`},
		{"backup.retention", `{"keep":"2"}`},
	}
	for _, c := range bad {
		_, perr := Call(c.method, json.RawMessage(c.params), opts, nil)
		if perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("Call(%s,%s) 错误=%v，期望 E_INVALID", c.method, c.params, perr)
		}
	}
}

// TestCallWithFaultExecutor 执行面故障仍按四类映射，证明 Call 分派到新接口。
func TestCallWithFaultExecutor(t *testing.T) {
	opts := testOptions(t)
	_, perr := Call("backup.list", json.RawMessage(`{}`), opts, faultExecutor{kind: FaultDisk})
	if perr == nil || perr.Code != pluginapi.E_INTERNAL || !strings.Contains(perr.Message, "磁盘满") {
		t.Fatalf("磁盘满映射=%v", perr)
	}
}

// testOptions 返回以临时目录为白名单与备份目录的运行配置。
func testOptions(t *testing.T) Options {
	t.Helper()
	workspace := t.TempDir()
	return Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
}

// faultExecutor 让所有执行面方法返回指定故障，供 Call 错误映射测试复用。
type faultExecutor struct{ kind FaultKind }

func (f faultExecutor) Create(CreateSpec) (CreateData, error) {
	return CreateData{}, NewFaultError(f.kind, "create 执行面故障")
}

func (f faultExecutor) List(string) ([]BackupInfo, error) {
	return nil, NewFaultError(f.kind, "list 执行面故障")
}

func (f faultExecutor) Restore(RestoreSpec) (RestoreData, error) {
	return RestoreData{}, NewFaultError(f.kind, "restore 执行面故障")
}

func (f faultExecutor) Delete(string, string) error {
	return NewFaultError(f.kind, "delete 执行面故障")
}

func (f faultExecutor) Verify(string, string) (VerifyData, error) {
	return VerifyData{}, NewFaultError(f.kind, "verify 执行面故障")
}

// TestOptionsDefaultAllowRoots 未配置白名单时回落到默认三根目录。
func TestOptionsDefaultAllowRoots(t *testing.T) {
	opts := Options{Dir: filepath.Join(t.TempDir(), "backups")}
	applyOptionDefaults(&opts)
	if len(opts.AllowRoots) != len(DefaultAllowRoots) {
		t.Fatalf("默认白名单=%v", opts.AllowRoots)
	}
	if opts.MaxBytes != DefaultMaxBytes || opts.MaxEntries != DefaultMaxEntries {
		t.Fatalf("默认上限=%d/%d", opts.MaxBytes, opts.MaxEntries)
	}
}

// TestListEmptyDir backup.list 在目录不存在时返回空列表。
func TestListEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	items, err := realFS{}.List(dir)
	if err != nil || len(items) != 0 {
		t.Fatalf("List(%q)=%v err=%v", dir, items, err)
	}
}

// TestResolveArchiveRejectsSymlink 备份包是符号链接时拒绝当作常规包。
func TestResolveArchiveRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.tar.gz")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link-20260101T000000Z.tar.gz")); err != nil {
		t.Fatalf("建符号链接: %v", err)
	}
	_, _, err := resolveArchive(dir, "link-20260101T000000Z")
	if err == nil {
		t.Fatal("符号链接备份包应被拒绝")
	}
}
