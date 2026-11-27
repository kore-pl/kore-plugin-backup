//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// e2eArchive 造一份真 tar.gz；name 可控以构造恶意归档。
func e2eArchive(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Uid: os.Getuid(), Gid: os.Getgid()}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("写 tar 头: %v", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("写 tar 内容: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// TestUploadRestoreE2E 真插件 + UDS：上传合法包→list/verify→复用既有 restore；恶意包 commit 被拒。
func TestUploadRestoreE2E(t *testing.T) {
	workspace := t.TempDir()
	backups := filepath.Join(workspace, "backups")
	socket := filepath.Join(workspace, "kernel.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("监听 UDS: %v", err)
	}
	defer func() { _ = listener.Close() }()
	if unixListener, ok := listener.(*net.UnixListener); ok {
		_ = unixListener.SetDeadline(time.Now().Add(30 * time.Second))
	}
	binary := buildPlugin(t, workspace)
	process, stderr := startPlugin(t, binary, socket, map[string]string{
		"KORE_BACKUP_DIR": backups, "KORE_BACKUP_ALLOW_ROOTS": workspace,
	})
	defer func() {
		if t.Failed() {
			t.Logf("插件 stderr:\n%s", stderr.String())
		}
	}()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("接受插件连接: %v", err)
	}
	defer func() { _ = conn.Close() }()
	handshake(t, conn)

	upload := func(id, filename string, payload []byte) *struct {
		ID      string `json:"id"`
		Entries int    `json:"entries"`
		Managed bool   `json:"managed"`
	} {
		begin := request(t, conn, "ub-"+id, "backup.upload", mustJSON(t, map[string]any{
			"phase": "begin", "upload_id": id, "filename": filename, "size_bytes": len(payload),
		}))
		if begin.Error != nil {
			t.Fatalf("begin 失败: %s/%s", begin.Error.Code, begin.Error.Message)
		}
		chunk := request(t, conn, "uc-"+id, "backup.upload", mustJSON(t, map[string]any{
			"phase": "chunk", "upload_id": id, "offset": 0,
			"data_base64": base64.StdEncoding.EncodeToString(payload),
		}))
		if chunk.Error != nil {
			t.Fatalf("chunk 失败: %s/%s", chunk.Error.Code, chunk.Error.Message)
		}
		commit := request(t, conn, "uk-"+id, "backup.upload", mustJSON(t, map[string]any{
			"phase": "commit", "upload_id": id,
		}))
		if commit.Error != nil {
			return nil
		}
		var result struct {
			ID      string `json:"id"`
			Entries int    `json:"entries"`
			Managed bool   `json:"managed"`
		}
		if err := json.Unmarshal(commit.Result, &result); err != nil {
			t.Fatalf("解析 commit: %v (%s)", err, commit.Result)
		}
		return &result
	}

	good := upload("u-good", "imported.tar.gz", e2eArchive(t, "site/a.txt", "alpha\n"))
	if good == nil || good.ID == "" || good.Entries != 1 || good.Managed {
		t.Fatalf("合法包导入结果不对: %+v (stderr=%s)", good, stderr.String())
	}
	list := request(t, conn, "l", "backup.list", `{}`)
	if list.Error != nil || !strings.Contains(string(list.Result), `"managed":false`) {
		t.Fatalf("list 应显示外部包: %s raw=%s", list.Error, list.Result)
	}
	verify := request(t, conn, "v", "backup.verify", mustJSON(t, map[string]any{"id": good.ID}))
	if verify.Error != nil || !strings.Contains(string(verify.Result), `"sha_match":true`) {
		t.Fatalf("verify 应通过: %s raw=%s", verify.Error, verify.Result)
	}
	target := filepath.Join(workspace, "restored")
	restore := request(t, conn, "r", "backup.restore", mustJSON(t, map[string]any{"id": good.ID, "target_dir": target}))
	if restore.Error != nil {
		t.Fatalf("导入包复用 restore 失败: %s/%s", restore.Error.Code, restore.Error.Message)
	}
	data, err := os.ReadFile(filepath.Join(target, "site", "a.txt"))
	if err != nil || string(data) != "alpha\n" {
		t.Fatalf("恢复内容不对: %q err=%v", data, err)
	}

	bad := upload("u-bad", "bad.tar.gz", e2eArchive(t, "../evil.txt", "evil"))
	if bad != nil {
		t.Fatalf("恶意归档应被 commit 拒绝: %+v", bad)
	}
	if _, err := os.Stat(filepath.Join(backups, ".upload-u-bad")); !os.IsNotExist(err) {
		t.Fatalf("恶意归档暂存未清理: %v", err)
	}
	shutdown := request(t, conn, "s", "kore.shutdown", `{}`)
	if shutdown.Error != nil {
		t.Fatalf("shutdown: %s", shutdown.Error)
	}
	waitExit(t, process)
}
