package backupop

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

type uploadTestEntry struct {
	name      string
	data      string
	kind      byte
	link      string
	mode      int64
	bigSize   int64
	skipClose bool
}

// makeUploadArchive 造一份 tar/gzip 归档字节；bigSize>0 时只写超大头不写内容（解压炸弹探针）。
func makeUploadArchive(t *testing.T, gzipFormat bool, entries ...uploadTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var gz *gzip.Writer
	if gzipFormat {
		gz = gzip.NewWriter(&buf)
	}
	var writer *tar.Writer
	if gzipFormat {
		writer = tar.NewWriter(gz)
	} else {
		writer = tar.NewWriter(&buf)
	}
	for _, e := range entries {
		size := int64(len(e.data))
		if e.bigSize > 0 {
			size = e.bigSize
		}
		kind := e.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{
			Name: e.name, Mode: mode, Size: size, Typeflag: kind, Linkname: e.link,
			Uid: os.Getuid(), Gid: os.Getgid(), Format: tar.FormatPAX,
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("写 tar 头: %v", err)
		}
		if e.bigSize == 0 && (kind == tar.TypeReg || kind == tar.TypeRegA) && e.data != "" {
			if _, err := writer.Write([]byte(e.data)); err != nil {
				t.Fatalf("写 tar 内容: %v", err)
			}
		}
	}
	if !eSkipClose(entries) {
		if err := writer.Close(); err != nil {
			t.Fatalf("收尾 tar: %v", err)
		}
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			t.Fatalf("收尾 gzip: %v", err)
		}
	}
	return buf.Bytes()
}

// runUpload 走完整四阶段：begin → 4 MiB 分块 chunk → commit。
func runUpload(t *testing.T, opts Options, uploadID, filename string, payload []byte, declaredSHA string) (json.RawMessage, *pluginapi.Error) {
	t.Helper()
	begin, _ := json.Marshal(map[string]any{
		"phase": "begin", "upload_id": uploadID, "filename": filename,
		"size_bytes": len(payload), "sha256": declaredSHA,
	})
	if _, perr := Call("backup.upload", begin, opts, nil); perr != nil {
		return nil, perr
	}
	const chunkSize = int(4 << 20)
	for offset := 0; offset < len(payload); offset += chunkSize {
		end := offset + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		chunk, _ := json.Marshal(map[string]any{
			"phase": "chunk", "upload_id": uploadID, "offset": offset,
			"data_base64": base64.StdEncoding.EncodeToString(payload[offset:end]),
		})
		if _, perr := Call("backup.upload", chunk, opts, nil); perr != nil {
			return nil, perr
		}
	}
	commit, _ := json.Marshal(map[string]any{"phase": "commit", "upload_id": uploadID})
	return Call("backup.upload", commit, opts, nil)
}

func uploadTestOptions(t *testing.T) Options {
	t.Helper()
	workspace := t.TempDir()
	return Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
}

// TestUploadRoundTripReusesRestore 合法包导入后 list/verify 可读且 restore 仍走既有路径；不写 .meta.json。
func TestUploadRoundTripReusesRestore(t *testing.T) {
	opts := uploadTestOptions(t)
	payload := makeUploadArchive(t, true,
		uploadTestEntry{name: "site/index.html", data: "<h1>hello</h1>"},
		uploadTestEntry{name: "site/css/a.css", data: "body{color:red}"},
	)
	sum := sha256.Sum256(payload)
	raw, perr := runUpload(t, opts, "u-roundtrip", "site.tar.gz", payload, hex.EncodeToString(sum[:]))
	if perr != nil {
		t.Fatalf("上传四阶段失败: %s %s", perr.Code, perr.Message)
	}
	var result struct {
		ID          string `json:"id"`
		Filename    string `json:"filename"`
		SHA256      string `json:"sha256"`
		Entries     int    `json:"entries"`
		Managed     bool   `json:"managed"`
		Compression string `json:"compression"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析 commit: %v (%s)", err, raw)
	}
	if result.ID == "" || result.Entries != 2 || result.Managed || result.Compression != "gzip" {
		t.Fatalf("commit 结果不对: %+v", result)
	}
	if result.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 不是对实际字节自算: %s", result.SHA256)
	}
	// 只写 .sha256，绝不写 .meta.json ⇒ Managed=false，retention 不会误删。
	if _, err := os.Stat(filepath.Join(opts.Dir, result.ID+".tar.gz.sha256")); err != nil {
		t.Fatalf("缺 .sha256 sidecar: %v", err)
	}
	if _, err := os.Stat(filepath.Join(opts.Dir, result.ID+".meta.json")); !os.IsNotExist(err) {
		t.Fatalf("导入包不应写 .meta.json: %v", err)
	}
	listed, perr := Call("backup.list", nil, opts, nil)
	if perr != nil || !strings.Contains(string(listed), `"managed":false`) {
		t.Fatalf("list 应显示外部包: perr=%v raw=%s", perr, listed)
	}
	verified, perr := Call("backup.verify", json.RawMessage(`{"id":`+uploadJSONString(result.ID)+`}`), opts, nil)
	if perr != nil || !strings.Contains(string(verified), `"sha_match":true`) {
		t.Fatalf("verify 应 sha_match=true: perr=%v raw=%s", perr, verified)
	}
	target := filepath.Join(filepath.Dir(opts.Dir), "restored")
	restored, perr := Call("backup.restore", json.RawMessage(`{"id":`+uploadJSONString(result.ID)+`,"target_dir":`+uploadJSONString(target)+`}`), opts, nil)
	if perr != nil || !strings.Contains(string(restored), `"restored_entries":2`) {
		t.Fatalf("导入包应能被既有 restore 恢复: perr=%v raw=%s", perr, restored)
	}
	data, err := os.ReadFile(filepath.Join(target, "site", "index.html"))
	if err != nil || string(data) != "<h1>hello</h1>" {
		t.Fatalf("恢复内容不对: %q err=%v", data, err)
	}
}

// eSkipClose 让解压炸弹探针只落超大 tar 头、不写满声明字节。
func eSkipClose(entries []uploadTestEntry) bool {
	for _, e := range entries {
		if e.skipClose {
			return true
		}
	}
	return false
}

func uploadJSONString(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// TestUploadRejectsMaliciousArchives 恶意归档在 commit 被拒，备份目录零落地、暂存零残留。
func TestUploadRejectsMaliciousArchives(t *testing.T) {
	cases := []struct {
		name   string
		format bool
		entry  uploadTestEntry
	}{
		{"parent-traversal", true, uploadTestEntry{name: "../evil.txt", data: "x"}},
		{"absolute-path", true, uploadTestEntry{name: "/etc/evil", data: "x"}},
		{"device-char", true, uploadTestEntry{name: "dev", kind: tar.TypeChar}},
		{"device-block", true, uploadTestEntry{name: "dev", kind: tar.TypeBlock}},
		{"fifo", true, uploadTestEntry{name: "pipe", kind: tar.TypeFifo}},
		{"hardlink", true, uploadTestEntry{name: "link", kind: tar.TypeLink, link: "site/index.html"}},
		{"symlink-escape", true, uploadTestEntry{name: "s", kind: tar.TypeSymlink, link: "../../outside"}},
		{"symlink-absolute", true, uploadTestEntry{name: "s", kind: tar.TypeSymlink, link: "/etc/passwd"}},
		{"long-name", true, uploadTestEntry{name: strings.Repeat("a", MaxEntryNameLen+1), data: "x"}},
		{"zip-bomb-header", false, uploadTestEntry{name: "bomb", data: "x", bigSize: int64(3) << 30, skipClose: true}},
		{"plain-tar-traversal", false, uploadTestEntry{name: "../evil.txt", data: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := uploadTestOptions(t)
			payload := makeUploadArchive(t, tc.format, tc.entry)
			_, perr := runUpload(t, opts, "u-bad", "bad.tar.gz", payload, "")
			if perr == nil {
				t.Fatal("恶意归档应被拒绝")
			}
			if perr.Code != pluginapi.E_INVALID {
				t.Fatalf("错误码=%q，期望 E_INVALID（message=%q）", perr.Code, perr.Message)
			}
			assertUploadClean(t, opts.Dir)
		})
	}
}

func assertUploadClean(t *testing.T, dir string) {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("读备份目录: %v", err)
	}
	for _, item := range items {
		if strings.HasPrefix(item.Name(), uploadDirPrefix) || strings.HasSuffix(item.Name(), ".tar.gz") || strings.HasSuffix(item.Name(), ".tar") {
			t.Fatalf("备份目录残留 %s", item.Name())
		}
	}
}

// TestUploadSizeAndChunkLimits 声明总长 512 MiB 上限、单块 4 MiB 上限与 sha256 自校验。
func TestUploadSizeAndChunkLimits(t *testing.T) {
	opts := uploadTestOptions(t)
	cases := []struct {
		name   string
		params string
	}{
		{"oversize-declared", `{"phase":"begin","upload_id":"u1","filename":"a.tar.gz","size_bytes":100000000000}`},
		{"hard-cap", `{"phase":"begin","upload_id":"u1","filename":"a.tar.gz","size_bytes":2147483648}`},
		{"bad-sha", `{"phase":"begin","upload_id":"u1","filename":"a.tar.gz","size_bytes":10,"sha256":"zz"}`},
		{"bad-upload-id", `{"phase":"begin","upload_id":"../evil","filename":"a.tar.gz","size_bytes":10}`},
		{"path-filename", `{"phase":"begin","upload_id":"u1","filename":"/tmp/a.tar.gz","size_bytes":10}`},
		{"backslash-filename", `{"phase":"begin","upload_id":"u1","filename":"..\\a.tar.gz","size_bytes":10}`},
		{"wrong-extension", `{"phase":"begin","upload_id":"u1","filename":"a.zip","size_bytes":10}`},
		{"bad-phase", `{"phase":"explode","upload_id":"u1"}`},
		{"negative-offset", `{"phase":"chunk","upload_id":"u1","offset":-1,"data_base64":"YQ=="}`},
		{"bad-base64", `{"phase":"chunk","upload_id":"u1","offset":0,"data_base64":"!!!"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := Call("backup.upload", json.RawMessage(tc.params), opts, nil)
			if perr == nil || perr.Code != pluginapi.E_INVALID {
				t.Fatalf("错误=%v，期望 E_INVALID", perr)
			}
		})
	}
	// 超过 4 MiB 的单块（base64 后 >5.3 MiB）必须拒绝且不落暂存。
	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), int(MaxUploadChunk)+1))
	_, perr := Call("backup.upload", json.RawMessage(`{"phase":"chunk","upload_id":"u1","offset":0,"data_base64":"`+big+`"}`), opts, nil)
	if perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("超大分块错误=%v，期望 E_INVALID", perr)
	}
	assertUploadClean(t, opts.Dir)
}

// TestUploadAbortTTLAndChunkOrder abort 清理、乱序分块冲突、30 分钟 TTL 半包清理。
func TestUploadAbortTTLAndChunkOrder(t *testing.T) {
	opts := uploadTestOptions(t)
	payload := makeUploadArchive(t, true, uploadTestEntry{name: "a.txt", data: "a"})
	begin, _ := json.Marshal(map[string]any{
		"phase": "begin", "upload_id": "u-order", "filename": "a.tar.gz", "size_bytes": len(payload),
	})
	if _, perr := Call("backup.upload", begin, opts, nil); perr != nil {
		t.Fatalf("begin: %v", perr)
	}
	// offset 必须为已接收字节数：从 7 开始应 E_CONFLICT。
	gap, _ := json.Marshal(map[string]any{
		"phase": "chunk", "upload_id": "u-order", "offset": 7,
		"data_base64": base64.StdEncoding.EncodeToString(payload[:7]),
	})
	if _, perr := Call("backup.upload", gap, opts, nil); perr == nil || perr.Code != pluginapi.E_CONFLICT {
		t.Fatalf("乱序分块错误=%v，期望 E_CONFLICT", perr)
	}
	abort, _ := json.Marshal(map[string]any{"phase": "abort", "upload_id": "u-order"})
	if raw, perr := Call("backup.upload", abort, opts, nil); perr != nil || !strings.Contains(string(raw), `"aborted":true`) {
		t.Fatalf("abort: perr=%v raw=%s", perr, raw)
	}
	assertUploadClean(t, opts.Dir)
	// 造一个超过 30 分钟的半包，下一次 begin 的 sweep 必须清掉它。
	stale := filepath.Join(opts.Dir, uploadDirPrefix+"u-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("建过期暂存: %v", err)
	}
	old := time.Now().Add(-UploadTTL - time.Minute).UTC().Format(time.RFC3339)
	state := uploadState{UploadID: "u-stale", Filename: "a.tar.gz", SizeBytes: 10, CreatedAt: old, Compression: "gzip"}
	if err := writeUploadState(stale, state); err != nil {
		t.Fatalf("写过期状态: %v", err)
	}
	if err := SweepStaleUploads(opts.Dir); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("过期半包未被清理: %v", err)
	}
	// 提交阶段遇到过期会话也要拒绝并清理。
	begin2, _ := json.Marshal(map[string]any{
		"phase": "begin", "upload_id": "u-expired", "filename": "a.tar.gz", "size_bytes": len(payload),
	})
	if _, perr := Call("backup.upload", begin2, opts, nil); perr != nil {
		t.Fatalf("begin2: %v", perr)
	}
	expiredState := uploadState{UploadID: "u-expired", Filename: "a.tar.gz", SizeBytes: int64(len(payload)), Compression: "gzip", CreatedAt: old}
	if err := writeUploadState(filepath.Join(opts.Dir, uploadDirPrefix+"u-expired"), expiredState); err != nil {
		t.Fatalf("改写过期状态: %v", err)
	}
	commit, _ := json.Marshal(map[string]any{"phase": "commit", "upload_id": "u-expired"})
	if _, perr := Call("backup.upload", commit, opts, nil); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("过期 commit 错误=%v，期望 E_INVALID", perr)
	}
	assertUploadClean(t, opts.Dir)
}

// TestUploadSHA Mismatch 声明 sha256 与实际字节不符时整包拒绝。
func TestUploadSHAMismatch(t *testing.T) {
	opts := uploadTestOptions(t)
	payload := makeUploadArchive(t, false, uploadTestEntry{name: "a.txt", data: "a"})
	_, perr := runUpload(t, opts, "u-sha", "a.tar", payload, strings.Repeat("0", 64))
	if perr == nil || perr.Code != pluginapi.E_INVALID || !strings.Contains(perr.Message, "sha256") {
		t.Fatalf("sha 不符错误=%v，期望 E_INVALID 含 sha256", perr)
	}
	assertUploadClean(t, opts.Dir)
}
