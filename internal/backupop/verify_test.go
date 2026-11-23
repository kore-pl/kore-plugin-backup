package backupop

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildVerifyBackup 造 id.tar.gz + 可选 .meta.json / .sha256，返回包路径。
func buildVerifyBackup(t *testing.T, dir, id, payload string, withMeta bool) string {
	t.Helper()
	archivePath := filepath.Join(dir, id+".tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("建包: %v", err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	body := []byte(payload)
	if err := tw.WriteHeader(&tar.Header{Name: "test.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("写头: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("写内容: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("关 tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("关 gzip: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("关文件: %v", err)
	}
	if !withMeta {
		return archivePath
	}
	payloadBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("读包: %v", err)
	}
	sum := sha256.Sum256(payloadBytes)
	hexSum := hex.EncodeToString(sum[:])
	info, _ := os.Stat(archivePath)
	meta := metaFile{
		ID: id, Name: id, CreatedAt: "2026-11-23T21:00:00+08:00",
		Entries: 1, SizeBytes: info.Size(), SHA256: hexSum, Compression: "gzip",
	}
	raw, _ := json.Marshal(meta)
	if err := os.WriteFile(archivePath+".meta.json", raw, 0o600); err != nil {
		t.Fatalf("写 meta: %v", err)
	}
	if err := os.WriteFile(archivePath+".sha256", []byte(hexSum+"  "+filepath.Base(archivePath)+"\n"), 0o600); err != nil {
		t.Fatalf("写 sidecar: %v", err)
	}
	return archivePath
}

func callVerifyForTest(t *testing.T, dir, id string) VerifyData {
	t.Helper()
	raw, perr := Call("backup.verify", json.RawMessage(`{"id":"`+id+`"}`), Options{Dir: dir}, nil)
	if perr != nil {
		t.Fatalf("verify 失败: %+v", perr)
	}
	var got VerifyData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析: %v（%s）", err, raw)
	}
	return got
}

func findCheck(data VerifyData, name string) (VerifyCheck, bool) {
	for _, check := range data.Checks {
		if check.Name == name {
			return check, true
		}
	}
	return VerifyCheck{}, false
}

// TestVerifyOK 正常包：全部检查通过、ok=true、记录的 sha/size/entries 都被核对。
func TestVerifyOK(t *testing.T) {
	dir := t.TempDir()
	buildVerifyBackup(t, dir, "good", "hello verify", true)
	got := callVerifyForTest(t, dir, "good")
	if !got.OK || got.Legacy || !got.SHAMatch || got.Corrupted || got.Entries != 1 {
		t.Fatalf("正常包校验结果不对: %+v", got)
	}
	for _, name := range []string{"exists", "size", "sha256", "size_recorded", "readable", "entries"} {
		if check, ok := findCheck(got, name); !ok || !check.OK {
			t.Fatalf("缺少通过的检查项 %s: %+v", name, got.Checks)
		}
	}
	if got.CreatedAt == "" || got.SHA256 == "" || got.SizeBytes == 0 {
		t.Fatalf("元信息缺失: %+v", got)
	}
}

// TestVerifyTruncated 截断包：size_recorded 失败，detail 明确说“文件不完整”。
func TestVerifyTruncated(t *testing.T) {
	dir := t.TempDir()
	archivePath := buildVerifyBackup(t, dir, "cut", strings.Repeat("x", 4096), true)
	raw, _ := os.ReadFile(archivePath)
	if err := os.WriteFile(archivePath, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	got := callVerifyForTest(t, dir, "cut")
	if got.OK {
		t.Fatalf("截断包不应通过: %+v", got)
	}
	check, ok := findCheck(got, "size_recorded")
	if !ok || check.OK || !strings.Contains(check.Detail, "文件不完整") {
		t.Fatalf("截断应给出「文件不完整」: %+v", got.Checks)
	}
}

// TestVerifyTampered 篡改一字节（大小不变）：sha256 失败，detail 说“内容被改动或写入不完整”。
func TestVerifyTamperedByte(t *testing.T) {
	dir := t.TempDir()
	archivePath := buildVerifyBackup(t, dir, "tamper", strings.Repeat("y", 2048), true)
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(archivePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := callVerifyForTest(t, dir, "tamper")
	if got.OK || got.SHAMatch {
		t.Fatalf("篡改包不应通过: %+v", got)
	}
	check, ok := findCheck(got, "sha256")
	if !ok || check.OK || !strings.Contains(check.Detail, "内容被改动或写入不完整") {
		t.Fatalf("篡改应给出「内容被改动或写入不完整」: %+v", got.Checks)
	}
}

// TestVerifyEmpty 空文件：size 检查失败并明确报“文件为空”。
func TestVerifyEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.tar.gz"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := callVerifyForTest(t, dir, "empty")
	if got.OK {
		t.Fatalf("空文件不应通过: %+v", got)
	}
	check, ok := findCheck(got, "size")
	if !ok || check.OK || !strings.Contains(check.Detail, "文件为空") {
		t.Fatalf("空文件应明确报「文件为空」: %+v", got.Checks)
	}
}

// TestVerifyLegacyNoSHA 旧备份无 sha/元数据：降级为仅检查可读性并说明。
func TestVerifyLegacyNoSHA(t *testing.T) {
	dir := t.TempDir()
	buildVerifyBackup(t, dir, "legacy", "old backup", false)
	got := callVerifyForTest(t, dir, "legacy")
	if !got.OK || !got.Legacy {
		t.Fatalf("旧备份应降级但可读: %+v", got)
	}
	check, ok := findCheck(got, "sha256")
	if !ok || !check.OK || !strings.Contains(check.Detail, "仅检查可读性") {
		t.Fatalf("旧备份应说明降级: %+v", got.Checks)
	}
}

// TestVerifyMissing 不存在的 id：ok=false + exists 检查给出“备份不存在”。
func TestVerifyMissing(t *testing.T) {
	got := callVerifyForTest(t, t.TempDir(), "absent")
	if got.OK || len(got.Checks) != 1 || got.Checks[0].OK {
		t.Fatalf("不存在备份的检查结果不对: %+v", got)
	}
	if !strings.Contains(got.Checks[0].Detail, "备份不存在") {
		t.Fatalf("应明确报备份不存在: %+v", got.Checks)
	}
}
