package boundary

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// makeArchive 造一份小 tar.gz 归档；kind 非 0 时写特殊条目类型。
func makeArchive(t *testing.T, name, link string, kind byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	header := &tar.Header{
		Name: name, Linkname: link, Mode: 0o644, Typeflag: kind,
		Uid: os.Getuid(), Gid: os.Getgid(), Format: tar.FormatPAX,
	}
	if kind == 0 || kind == tar.TypeReg {
		header.Typeflag = tar.TypeReg
		header.Size = 5
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("写 tar 头: %v", err)
	}
	if header.Typeflag == tar.TypeReg {
		if _, err := tw.Write([]byte("hello")); err != nil {
			t.Fatalf("写 tar 内容: %v", err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func uploadViaCall(t *testing.T, opts backupop.Options, id, filename string, payload []byte) *pluginapi.Error {
	t.Helper()
	begin, _ := json.Marshal(map[string]any{
		"phase": "begin", "upload_id": id, "filename": filename, "size_bytes": len(payload),
	})
	if _, perr := call(t, "backup.upload", begin, opts); perr != nil {
		return perr
	}
	chunk, _ := json.Marshal(map[string]any{
		"phase": "chunk", "upload_id": id, "offset": 0,
		"data_base64": base64.StdEncoding.EncodeToString(payload),
	})
	if _, perr := call(t, "backup.upload", chunk, opts); perr != nil {
		return perr
	}
	commit, _ := json.Marshal(map[string]any{"phase": "commit", "upload_id": id})
	_, perr := call(t, "backup.upload", commit, opts)
	return perr
}

func assertNoUploadResidue(t *testing.T, dir string) {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("读备份目录: %v", err)
	}
	for _, item := range items {
		if strings.HasPrefix(item.Name(), ".upload-") || strings.HasSuffix(item.Name(), ".tar.gz") || strings.HasSuffix(item.Name(), ".tar") {
			t.Fatalf("恶意/超大上传后目录残留 %s", item.Name())
		}
	}
}

// TestUploadMaliciousArchivesBoundary 真实执行面：恶意归档 commit 被拒且备份目录与暂存零落地。
func TestUploadMaliciousArchivesBoundary(t *testing.T) {
	cases := []struct {
		name string
		make func() []byte
	}{
		{"parent-traversal", func() []byte { return makeArchive(t, "../evil.txt", "", 0) }},
		{"absolute-path", func() []byte { return makeArchive(t, "/etc/evil", "", 0) }},
		{"char-device", func() []byte { return makeArchive(t, "dev", "", tar.TypeChar) }},
		{"fifo", func() []byte { return makeArchive(t, "pipe", "", tar.TypeFifo) }},
		{"symlink-escape", func() []byte { return makeArchive(t, "s", "../../outside", tar.TypeSymlink) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, opts := newWorkspace(t)
			if perr := uploadViaCall(t, opts, "u-boundary", "bad.tar.gz", tc.make()); perr == nil || perr.Code != pluginapi.E_INVALID {
				t.Fatalf("错误=%v，期望 E_INVALID", perr)
			}
			assertNoUploadResidue(t, opts.Dir)
		})
	}
}

// TestUploadOversizeBoundary 声明总长 >512 MiB 与单块 >4 MiB 均在真实执行面前被拒。
func TestUploadOversizeBoundary(t *testing.T) {
	_, opts := newWorkspace(t)
	begin, _ := json.Marshal(map[string]any{
		"phase": "begin", "upload_id": "u-big", "filename": "big.tar.gz",
		"size_bytes": backupop.MaxUploadBytes + 1,
	})
	if _, perr := call(t, "backup.upload", begin, opts); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("超大声明错误=%v，期望 E_INVALID", perr)
	}
	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), int(backupop.MaxUploadChunk)+1))
	chunk, _ := json.Marshal(map[string]any{
		"phase": "chunk", "upload_id": "u-big", "offset": 0, "data_base64": big,
	})
	if _, perr := call(t, "backup.upload", chunk, opts); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("超大分块错误=%v，期望 E_INVALID", perr)
	}
	assertNoUploadResidue(t, opts.Dir)
}

// TestUploadTTLBoundary 超过 30 分钟的暂存半包被启动扫描清理，不能长期占用备份目录。
func TestUploadTTLBoundary(t *testing.T) {
	_, opts := newWorkspace(t)
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		t.Fatalf("建备份目录: %v", err)
	}
	stale := filepath.Join(opts.Dir, ".upload-u-old")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("建暂存: %v", err)
	}
	state := map[string]any{
		"upload_id": "u-old", "filename": "a.tar.gz", "size_bytes": 10,
		"created_at":  time.Now().Add(-backupop.UploadTTL - time.Minute).UTC().Format(time.RFC3339),
		"compression": "gzip",
	}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(stale, ".upload.json"), data, 0o600); err != nil {
		t.Fatalf("写状态: %v", err)
	}
	if err := backupop.SweepStaleUploads(opts.Dir); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("过期半包未清理: %v", err)
	}
}

// TestUploadCapabilityBoundary 入口必须是 system:admin 写方法（未授权走内核 capability.denied）。
func TestUploadCapabilityBoundary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "info.json"))
	if err != nil {
		t.Fatalf("读 info.json: %v", err)
	}
	var info struct {
		MethodCapabilities map[string]string `json:"method_capabilities"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("解析 info.json: %v", err)
	}
	if got := info.MethodCapabilities["backup.upload"]; got != "system:admin" {
		t.Fatalf("backup.upload 能力=%q，期望 system:admin", got)
	}
}
