// Package fault 对 backup 插件做四类故障注入，验证数据面与控制面方法的错误返回。
// 每个用例向 backupop.Call 的执行面注入一种故障，断言结构化错误码。
package fault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// faultExecutor 按注入类别让每个方法都失败，模拟四类运行时故障。
type faultExecutor struct {
	kind backupop.FaultKind
}

func (f faultExecutor) Create(backupop.CreateSpec) (backupop.CreateData, error) {
	return backupop.CreateData{}, backupop.NewFaultError(f.kind, "create 执行面故障")
}

func (f faultExecutor) List(string) ([]backupop.BackupInfo, error) {
	return nil, backupop.NewFaultError(f.kind, "list 执行面故障")
}

func (f faultExecutor) Restore(backupop.RestoreSpec) (backupop.RestoreData, error) {
	return backupop.RestoreData{}, backupop.NewFaultError(f.kind, "restore 执行面故障")
}

func (f faultExecutor) Delete(string, string) error {
	return backupop.NewFaultError(f.kind, "delete 执行面故障")
}

func (f faultExecutor) Verify(string, string) (backupop.VerifyData, error) {
	return backupop.VerifyData{}, backupop.NewFaultError(f.kind, "verify 执行面故障")
}

func (f faultExecutor) Download(backupop.DownloadSpec) (backupop.DownloadData, error) {
	return backupop.DownloadData{}, backupop.NewFaultError(f.kind, "download 执行面故障")
}

func (f faultExecutor) Upload(backupop.UploadSpec) (backupop.UploadData, error) {
	return backupop.UploadData{}, backupop.NewFaultError(f.kind, "upload 执行面故障")
}

// StateStore 让 jobs / schedule.* 也进四类故障矩阵。
func (f faultExecutor) SaveJob(string, backupop.JobRecord) error {
	return backupop.NewFaultError(f.kind, "jobs 执行面故障")
}

func (f faultExecutor) ListJobs(string, string) ([]backupop.JobRecord, error) {
	return nil, backupop.NewFaultError(f.kind, "jobs 执行面故障")
}

func (f faultExecutor) SaveSchedules(string, []backupop.ScheduleRecord) error {
	return backupop.NewFaultError(f.kind, "schedule 执行面故障")
}

func (f faultExecutor) LoadSchedules(string) ([]backupop.ScheduleRecord, error) {
	return nil, backupop.NewFaultError(f.kind, "schedule 执行面故障")
}

// okExecutor 是 sanity 用的空执行面：只证明方法分派与编码路径正常，不落盘。
type okExecutor struct{}

func (okExecutor) Create(backupop.CreateSpec) (backupop.CreateData, error) {
	return backupop.CreateData{Skipped: []string{}, Warnings: []string{}}, nil
}

func (okExecutor) List(string) ([]backupop.BackupInfo, error) { return []backupop.BackupInfo{}, nil }

func (okExecutor) Restore(backupop.RestoreSpec) (backupop.RestoreData, error) {
	return backupop.RestoreData{Failed: []string{}}, nil
}

func (okExecutor) Delete(string, string) error { return nil }

func (okExecutor) Verify(string, string) (backupop.VerifyData, error) {
	return backupop.VerifyData{}, nil
}

func (okExecutor) Download(spec backupop.DownloadSpec) (backupop.DownloadData, error) {
	return backupop.DownloadData{ID: spec.ID, EOF: true}, nil
}

func (okExecutor) Upload(spec backupop.UploadSpec) (backupop.UploadData, error) {
	return backupop.UploadData{UploadID: spec.UploadID}, nil
}

// faultMethod 是一个方法的合法参数。
type faultMethod struct {
	name   string
	method string
	params string
}

// assertFault 对每个方法注入故障，断言 nil 结果 + 预期错误码与类别关键词。
func assertFault(t *testing.T, kind backupop.FaultKind, wantCode, wantHint string) {
	t.Helper()
	exec := faultExecutor{kind: kind}
	workspace := t.TempDir()
	opts := backupop.Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	for _, m := range methodsFor(t, workspace) {
		t.Run(m.name, func(t *testing.T) {
			result, perr := backupop.Call(m.method, json.RawMessage(m.params), opts, exec)
			if perr == nil {
				t.Fatalf("%s 注入 %s 后未返回错误（结果=%s）", m.method, kind, result)
			}
			if result != nil {
				t.Fatalf("%s 出错时不应带结果: %s", m.method, result)
			}
			if perr.Code != wantCode {
				t.Fatalf("%s 错误码=%q，期望 %q（message=%q）", m.method, perr.Code, wantCode, perr.Message)
			}
			if !strings.Contains(perr.Message, wantHint) {
				t.Fatalf("%s 错误信息 %q 未含类别关键词 %q", m.method, perr.Message, wantHint)
			}
		})
	}
}

// methodsFor 构造全部方法的合法参数，源目录位于给定工作区。
func methodsFor(t *testing.T, workspace string) []faultMethod {
	t.Helper()
	source := filepath.Join(workspace, "src")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("建源目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写源文件: %v", err)
	}
	sourceJSON, _ := json.Marshal(source)
	targetJSON, _ := json.Marshal(filepath.Join(workspace, "dst"))
	id := "snap-20260101T000000Z"
	idJSON, _ := json.Marshal(id)
	return []faultMethod{
		{"create", "backup.create", `{"name":"snap","paths":[` + string(sourceJSON) + `]}`},
		{"list", "backup.list", `{}`},
		{"restore", "backup.restore", `{"id":` + string(idJSON) + `,"target_dir":` + string(targetJSON) + `}`},
		{"delete", "backup.delete", `{"id":` + string(idJSON) + `}`},
		{"verify", "backup.verify", `{"id":` + string(idJSON) + `}`},
		{"retention", "backup.retention", `{"keep":2}`},
		{"stats", "backup.stats", `{}`},
		{"download", "backup.download", `{"id":` + string(idJSON) + `,"inline":true,"limit":1024}`},
		{"upload-begin", "backup.upload", `{"phase":"begin","upload_id":"u1","filename":"snap.tar.gz","size_bytes":1024}`},
		{"jobs", "backup.jobs", `{}`},
		{"schedule-set", "backup.schedule.set", `{"name":"snap","schedule":"0 3 * * *","paths":[` + string(sourceJSON) + `]}`},
		{"schedule-get", "backup.schedule.get", `{}`},
	}
}

// sanityNoFault 验证无故障时全部方法正常返回，证明故障断言不是"永远失败"的假通过。
func sanityNoFault(t *testing.T) {
	t.Helper()
	workspace := t.TempDir()
	opts := backupop.Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	for _, m := range methodsFor(t, workspace) {
		result, perr := backupop.Call(m.method, json.RawMessage(m.params), opts, okExecutor{})
		if perr != nil {
			t.Fatalf("无故障时 %s 意外返回错误: %v", m.method, perr)
		}
		if len(result) == 0 {
			t.Fatalf("无故障时 %s 返回空结果", m.method)
		}
	}
}

// verifyErrorContract 断言错误体是标准协议结构（含 code 字段）。
func verifyErrorContract(t *testing.T, perr *pluginapi.Error) {
	t.Helper()
	if perr == nil {
		t.Fatal("错误体为 nil")
	}
	if perr.Code == "" || perr.Message == "" {
		t.Fatalf("错误体字段不完整: %+v", perr)
	}
}
