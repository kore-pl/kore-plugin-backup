package backupop

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// retentionFixture 是一份手工落盘的备份包及其字节数。
type retentionFixture struct {
	ID        string
	Name      string
	CreatedAt string
	SizeBytes int64
}

// writeTestBackup 在 dir 落一个 List 可识别的备份包：gzip tar + .sha256 + .meta.json。
// 时间戳由调用方给定，便于构造"保留最近 N 份"的确定性场景。
func writeTestBackup(t *testing.T, dir, id, name, createdAt string, payload int) retentionFixture {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建备份目录: %v", err)
	}
	archivePath := filepath.Join(dir, id+".tar.gz")
	file, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("建备份包: %v", err)
	}
	gz := gzip.NewWriter(file)
	writer := tar.NewWriter(gz)
	content := bytes.Repeat([]byte("k"), payload)
	if err := writer.WriteHeader(&tar.Header{
		Name: "payload.txt", Mode: 0o600, Size: int64(len(content)), ModTime: time.Unix(0, 0),
	}); err != nil {
		t.Fatalf("写 tar 头: %v", err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatalf("写 tar 内容: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("收尾 tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("收尾 gzip: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("关闭备份包: %v", err)
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("读备份包: %v", err)
	}
	sum := sha256.Sum256(data)
	meta := metaFile{
		ID: id, Name: name, CreatedAt: createdAt, Entries: 1,
		SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Compression: "gzip",
	}
	if err := writeSidecars(archivePath, meta); err != nil {
		t.Fatalf("写 sidecar: %v", err)
	}
	return retentionFixture{ID: id, Name: name, CreatedAt: createdAt, SizeBytes: int64(len(data))}
}

// writeForeignBackup 落一个无 sidecar 的包，模拟"非本插件创建"的归档。
func writeForeignBackup(t *testing.T, dir, id string) {
	t.Helper()
	path := filepath.Join(dir, id+".tar.gz")
	if err := os.WriteFile(path, []byte("not-created-by-plugin"), 0o600); err != nil {
		t.Fatalf("写外来包: %v", err)
	}
}

// retentionResult 是测试侧解析用的与 RetentionData 同形结构。
type retentionResult struct {
	Kept       int                  `json:"kept"`
	Deleted    []RetentionItem      `json:"deleted"`
	FreedBytes int64                `json:"freed_bytes"`
	DryRun     bool                 `json:"dry_run"`
	Protected  []RetentionProtected `json:"protected"`
}

func callRetentionResult(t *testing.T, opts Options, params string) retentionResult {
	t.Helper()
	raw, perr := Call("backup.retention", json.RawMessage(params), opts, nil)
	if perr != nil {
		t.Fatalf("backup.retention(%s) 失败: %v", params, perr)
	}
	var result retentionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析 retention 结果: %v (%s)", err, raw)
	}
	return result
}

// TestRetentionDryRunThenDelete 预演不删盘，真删只留最近 keep 份并报告释放字节数。
func TestRetentionDryRunThenDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backups")
	fixtures := []retentionFixture{
		writeTestBackup(t, dir, "auto-20260101T000000Z", "auto", "2026-01-01T00:00:00Z", 8),
		writeTestBackup(t, dir, "auto-20260102T000000Z", "auto", "2026-01-02T00:00:00Z", 16),
		writeTestBackup(t, dir, "auto-20260103T000000Z", "auto", "2026-01-03T00:00:00Z", 32),
		writeTestBackup(t, dir, "auto-20260104T000000Z", "auto", "2026-01-04T00:00:00Z", 64),
	}
	opts := Options{Dir: dir}

	preview := callRetentionResult(t, opts, `{"keep":2,"dry_run":true}`)
	if !preview.DryRun || len(preview.Deleted) != 2 || preview.Kept != 2 {
		t.Fatalf("dry_run 结果=%+v，期望 deleted=2 kept=2", preview)
	}
	if preview.FreedBytes != fixtures[0].SizeBytes+fixtures[1].SizeBytes {
		t.Fatalf("dry_run freed_bytes=%d，期望 %d", preview.FreedBytes, fixtures[0].SizeBytes+fixtures[1].SizeBytes)
	}
	for _, fixture := range fixtures {
		if _, err := os.Stat(filepath.Join(dir, fixture.ID+".tar.gz")); err != nil {
			t.Fatalf("dry_run 不应删盘，%s 已消失: %v", fixture.ID, err)
		}
	}

	deleted := callRetentionResult(t, opts, `{"keep":2}`)
	if deleted.DryRun || len(deleted.Deleted) != 2 || deleted.Kept != 2 {
		t.Fatalf("真删结果=%+v，期望 deleted=2 kept=2", deleted)
	}
	if deleted.FreedBytes != fixtures[0].SizeBytes+fixtures[1].SizeBytes {
		t.Fatalf("freed_bytes=%d，期望 %d", deleted.FreedBytes, fixtures[0].SizeBytes+fixtures[1].SizeBytes)
	}
	wantGone := map[string]bool{fixtures[0].ID: true, fixtures[1].ID: true}
	for _, item := range deleted.Deleted {
		if !wantGone[item.ID] {
			t.Fatalf("被删的不是最旧两份: %s", item.ID)
		}
		delete(wantGone, item.ID)
	}
	if len(wantGone) != 0 {
		t.Fatalf("应删除 %v，实际 deleted=%+v", wantGone, deleted.Deleted)
	}
	for _, id := range []string{fixtures[2].ID, fixtures[3].ID} {
		if _, err := os.Stat(filepath.Join(dir, id+".tar.gz")); err != nil {
			t.Fatalf("最近两份必须保留，%s 不存在: %v", id, err)
		}
	}
}

// TestRetentionProtectsForeignAndNewest 外来包与最新一份都不删，并给出 protected 原因。
func TestRetentionProtectsForeignAndNewest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backups")
	older := writeTestBackup(t, dir, "auto-20260101T000000Z", "auto", "2026-01-01T00:00:00Z", 8)
	newest := writeTestBackup(t, dir, "auto-20260103T000000Z", "auto", "2026-01-03T00:00:00Z", 8)
	foreignID := "manual-20251231T000000Z"
	writeForeignBackup(t, dir, foreignID)

	result := callRetentionResult(t, Options{Dir: dir}, `{"keep":1}`)
	if result.Kept != 2 {
		t.Fatalf("kept=%d，期望 2（最新 + 外来包）", result.Kept)
	}
	if len(result.Deleted) != 1 || result.Deleted[0].ID != older.ID {
		t.Fatalf("只应删除较旧的插件包 %s，实际 %+v", older.ID, result.Deleted)
	}
	if len(result.Protected) != 1 || result.Protected[0].ID != foreignID {
		t.Fatalf("外来包应出现在 protected 且带原因，实际 %+v", result.Protected)
	}
	if !strings.Contains(result.Protected[0].Reason, "非本插件创建") {
		t.Fatalf("protected 原因不明确: %q", result.Protected[0].Reason)
	}
	for _, path := range []string{newest.ID + ".tar.gz", foreignID + ".tar.gz"} {
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Fatalf("%s 必须保留: %v", path, err)
		}
	}
}

// TestRetentionKeepBounds keep 必须是 1..100 的整数。
func TestRetentionKeepBounds(t *testing.T) {
	opts := testOptions(t)
	for _, params := range []string{`{"keep":0}`, `{"keep":101}`, `{}`, `{"keep":-1}`} {
		_, perr := Call("backup.retention", json.RawMessage(params), opts, nil)
		if perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("retention(%s) 错误=%v，期望 E_INVALID", params, perr)
		}
	}
}

// retentionFailDelete 在删除第 failOn 次时返回故障，用于验证"已删除 N 份"上下文。
type retentionFailDelete struct {
	items  []BackupInfo
	calls  int
	failOn int
}

func (f *retentionFailDelete) Create(CreateSpec) (CreateData, error) { return CreateData{}, nil }

func (f *retentionFailDelete) List(string) ([]BackupInfo, error) { return f.items, nil }

func (f *retentionFailDelete) Restore(RestoreSpec) (RestoreData, error) { return RestoreData{}, nil }

func (f *retentionFailDelete) Delete(string, string) error {
	f.calls++
	if f.calls == f.failOn {
		return NewFaultError(FaultDisk, "删除时磁盘满")
	}
	return nil
}

func (f *retentionFailDelete) Verify(string, string) (VerifyData, error) { return VerifyData{}, nil }

// TestRetentionDeleteFailureReportsProgress 中途失败要带已删除数量，不假装零删除。
func TestRetentionDeleteFailureReportsProgress(t *testing.T) {
	exec := &retentionFailDelete{failOn: 2, items: []BackupInfo{
		{ID: "b4-20260104T000000Z", Name: "b4", CreatedAt: "2026-01-04T00:00:00Z", Managed: true},
		{ID: "b3-20260103T000000Z", Name: "b3", CreatedAt: "2026-01-03T00:00:00Z", Managed: true},
		{ID: "b2-20260102T000000Z", Name: "b2", CreatedAt: "2026-01-02T00:00:00Z", Managed: true},
		{ID: "b1-20260101T000000Z", Name: "b1", CreatedAt: "2026-01-01T00:00:00Z", Managed: true},
	}}
	_, perr := Call("backup.retention", json.RawMessage(`{"keep":2}`), testOptions(t), exec)
	if perr == nil || perr.Code != pluginapi.E_INTERNAL {
		t.Fatalf("错误=%v，期望 E_INTERNAL", perr)
	}
	if !strings.Contains(perr.Message, "已删除 1 份") {
		t.Fatalf("错误信息缺少已删除数量: %q", perr.Message)
	}
}

// statsResult 是测试侧解析用的与 StatsData 同形结构。
type statsResult struct {
	TotalCount int               `json:"total_count"`
	TotalBytes int64             `json:"total_bytes"`
	Oldest     *string           `json:"oldest"`
	Newest     *string           `json:"newest"`
	ByName     []StatsNameBucket `json:"by_name"`
}

// TestStatsSummary 汇总总数/字节/最早/最晚与按 name 分组。
func TestStatsSummary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backups")
	first := writeTestBackup(t, dir, "auto-20260101T000000Z", "auto", "2026-01-01T00:00:00Z", 8)
	manual := writeTestBackup(t, dir, "manual-20260102T000000Z", "manual", "2026-01-02T00:00:00Z", 16)
	last := writeTestBackup(t, dir, "auto-20260103T000000Z", "auto", "2026-01-03T00:00:00Z", 8)
	raw, perr := Call("backup.stats", json.RawMessage(`{}`), Options{Dir: dir}, nil)
	if perr != nil {
		t.Fatalf("backup.stats 失败: %v", perr)
	}
	var result statsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析 stats: %v (%s)", err, raw)
	}
	wantBytes := first.SizeBytes + manual.SizeBytes + last.SizeBytes
	if result.TotalCount != 3 || result.TotalBytes != wantBytes {
		t.Fatalf("统计=%+v，期望 count=3 bytes=%d", result, wantBytes)
	}
	if result.Oldest == nil || *result.Oldest != first.CreatedAt {
		t.Fatalf("oldest=%v，期望 %s", result.Oldest, first.CreatedAt)
	}
	if result.Newest == nil || *result.Newest != last.CreatedAt {
		t.Fatalf("newest=%v，期望 %s", result.Newest, last.CreatedAt)
	}
	if len(result.ByName) != 2 || result.ByName[0].Name != "auto" || result.ByName[0].Count != 2 {
		t.Fatalf("by_name 应按数量倒序: %+v", result.ByName)
	}
	if result.ByName[0].Bytes != first.SizeBytes+last.SizeBytes {
		t.Fatalf("auto 分组字节=%d，期望 %d", result.ByName[0].Bytes, first.SizeBytes+last.SizeBytes)
	}
}

// TestStatsEmpty 空目录返回零值，oldest/newest 为 null 且 by_name 是空数组。
func TestStatsEmpty(t *testing.T) {
	raw, perr := Call("backup.stats", json.RawMessage(`{}`), testOptions(t), nil)
	if perr != nil {
		t.Fatalf("backup.stats 失败: %v", perr)
	}
	var result statsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析 stats: %v (%s)", err, raw)
	}
	if result.TotalCount != 0 || result.TotalBytes != 0 || result.Oldest != nil || result.Newest != nil {
		t.Fatalf("空目录统计=%+v", result)
	}
	if result.ByName == nil || len(result.ByName) != 0 {
		t.Fatalf("by_name 必须是空数组而不是 null: %s", raw)
	}
}
