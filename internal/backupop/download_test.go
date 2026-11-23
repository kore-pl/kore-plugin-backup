package backupop

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// createBackupFixture 造一个备份并返回 create 结果（id/sha256/path）。
func createBackupFixture(t *testing.T, workspace string) (Options, string, string) {
	t.Helper()
	source := prepareSource(t, workspace)
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	raw, perr := Call("backup.create", json.RawMessage(`{"name":"snap","paths":[`+quote(source)+`]}`), opts, nil)
	if perr != nil {
		t.Fatalf("create: %v", perr)
	}
	var create struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(raw, &create); err != nil {
		t.Fatalf("解析 create: %v", err)
	}
	return opts, create.ID, create.SHA256
}

// TestDownloadMetadata 校验不 inline 时只返回可下载引用（路径 + 大小 + sha），不读内容。
func TestDownloadMetadata(t *testing.T) {
	workspace := t.TempDir()
	opts, id, sha := createBackupFixture(t, workspace)
	raw, perr := Call("backup.download", json.RawMessage(`{"id":`+quote(id)+`}`), opts, nil)
	if perr != nil {
		t.Fatalf("download: %v", perr)
	}
	var data DownloadData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if data.ID != id || data.SHA256 != sha || data.SizeBytes == 0 || data.Path == "" ||
		data.Inline || data.DataBase64 != "" || !data.EOF {
		t.Fatalf("元信息结果不对: %+v", data)
	}
	if filepath.Dir(data.Path) != filepath.Clean(opts.Dir) || data.Filename != filepath.Base(data.Path) {
		t.Fatalf("下载引用不在备份目录内: %+v", data)
	}
	if _, err := os.Stat(data.Path); err != nil {
		t.Fatalf("下载引用不存在: %v", err)
	}
}

// TestDownloadChunkedRoundTrip 校验分块下载拼回来的字节与原包逐字节一致。
func TestDownloadChunkedRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	opts, id, sha := createBackupFixture(t, workspace)
	// 造一个 > 首块的小包不现实，改成小 limit 多块读取，证明 offset/next_offset 闭环。
	const chunk = 97
	var content []byte
	offset := int64(0)
	for round := 0; round < 10000; round++ {
		params := `{"id":` + quote(id) + `,"inline":true,"offset":` + itoa(offset) + `,"limit":` + itoa(chunk) + `}`
		raw, perr := Call("backup.download", json.RawMessage(params), opts, nil)
		if perr != nil {
			t.Fatalf("download offset=%d: %v", offset, perr)
		}
		var data DownloadData
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("解析: %v", err)
		}
		piece, err := base64.StdEncoding.DecodeString(data.DataBase64)
		if err != nil {
			t.Fatalf("base64: %v", err)
		}
		if data.ChunkBytes != len(piece) || data.NextOffset != offset+int64(len(piece)) {
			t.Fatalf("分块元信息不一致: %+v", data)
		}
		content = append(content, piece...)
		if data.EOF {
			if data.NextOffset != data.SizeBytes {
				t.Fatalf("EOF 时 next_offset=%d != size=%d", data.NextOffset, data.SizeBytes)
			}
			break
		}
		if data.NextOffset <= offset {
			t.Fatalf("next_offset 未前进: %d → %d", offset, data.NextOffset)
		}
		offset = data.NextOffset
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != sha {
		t.Fatalf("分块下载拼回的 sha 与原包不一致: %s != %s", hex.EncodeToString(sum[:]), sha)
	}
}

// TestDownloadRejects 校验 id/offset/limit 与符号链接越界。
func TestDownloadRejects(t *testing.T) {
	workspace := t.TempDir()
	opts, id, _ := createBackupFixture(t, workspace)
	bad := []string{
		`{"id":"../etc"}`,
		`{"id":""}`,
		`{"id":` + quote(id) + `,"inline":true,"limit":8388608}`,
		`{"id":` + quote(id) + `,"inline":true,"offset":-1}`,
		`{"id":` + quote(id) + `,"inline":true,"offset":999999999}`,
	}
	for _, params := range bad {
		if _, perr := Call("backup.download", json.RawMessage(params), opts, nil); perr == nil ||
			perr.Code != pluginapi.E_INVALID {
			t.Fatalf("params=%s 期望 E_INVALID，得到 %v", params, perr)
		}
	}
	if _, perr := Call("backup.download", json.RawMessage(`{"id":"absent-20260101T000000Z"}`), opts, nil); perr == nil ||
		perr.Code != pluginapi.E_NOTFOUND {
		t.Fatalf("不存在 id 期望 E_NOTFOUND，得到 %v", perr)
	}
	// 备份目录内放一个指向目录外的符号链接，不能当备份包读。
	outside := filepath.Join(t.TempDir(), "secret.tar.gz")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("造外部文件: %v", err)
	}
	link := filepath.Join(opts.Dir, "evil-20260101T000000Z.tar.gz")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("造符号链接: %v", err)
	}
	if _, perr := Call("backup.download", json.RawMessage(`{"id":"evil-20260101T000000Z"}`), opts, nil); perr == nil ||
		perr.Code != pluginapi.E_INVALID {
		t.Fatalf("符号链接越界期望 E_INVALID，得到 %v", perr)
	}
}

// itoa 是测试内的十进制转换，避免引入 strconv 影响可读性。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}
