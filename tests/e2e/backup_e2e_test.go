//go:build e2e

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBackupLifecycleE2E 七步真跑：create → list → verify → restore → diff →
// delete → 恶意 ../evil 包被拒，且目标目录无残留。
func TestBackupLifecycleE2E(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("建源目录: %v", err)
	}
	sources := map[string]string{
		"a.txt":     "alpha\n",
		"sub/b.txt": "beta\n",
		"中文.txt":    "中文内容\n",
	}
	for name, content := range sources {
		path := filepath.Join(src, filepath.FromSlash(name))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
	backups := filepath.Join(workspace, "backups")
	socket := filepath.Join(workspace, "kernel.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("监听 UDS: %v", err)
	}
	defer func() { _ = listener.Close() }()
	if unixListener, ok := listener.(*net.UnixListener); ok {
		_ = unixListener.SetDeadline(time.Now().Add(10 * time.Second))
	}

	binary := buildPlugin(t, workspace)
	process, stderr := startPlugin(t, binary, socket, map[string]string{
		"KORE_BACKUP_DIR":         backups,
		"KORE_BACKUP_ALLOW_ROOTS": workspace,
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

	// --- 步骤 1：backup.create 真打包 ---
	create := request(t, conn, "create", "backup.create", mustJSON(t, map[string]any{
		"name": "snap", "paths": []string{src}, "compression": "gzip",
	}))
	if create.Error != nil {
		t.Fatalf("步骤1 create 失败: %s/%s", create.Error.Code, create.Error.Message)
	}
	var created struct {
		ID         string   `json:"id"`
		Path       string   `json:"path"`
		SizeBytes  int64    `json:"size_bytes"`
		Entries    int      `json:"entries"`
		SHA256     string   `json:"sha256"`
		DurationMS int64    `json:"duration_ms"`
		Skipped    []string `json:"skipped"`
		Warnings   []string `json:"warnings"`
	}
	if err := json.Unmarshal(create.Result, &created); err != nil {
		t.Fatalf("解析 create: %v (%s)", err, create.Result)
	}
	if created.ID == "" || created.SHA256 == "" || created.Entries < 5 || created.SizeBytes <= 0 {
		t.Fatalf("步骤1 create 结果不完整: %+v", created)
	}
	if _, err := os.Stat(created.Path); err != nil {
		t.Fatalf("步骤1 备份包未落盘: %v", err)
	}
	t.Logf("步骤1 create: id=%s entries=%d size=%d sha=%s duration_ms=%d",
		created.ID, created.Entries, created.SizeBytes, created.SHA256, created.DurationMS)

	// --- 步骤 2：backup.list 出现该备份 ---
	list := request(t, conn, "list", "backup.list", `{}`)
	if list.Error != nil {
		t.Fatalf("步骤2 list 失败: %s/%s", list.Error.Code, list.Error.Message)
	}
	var listed struct {
		Backups []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			SizeBytes int64  `json:"size_bytes"`
			SHA256    string `json:"sha256"`
			Entries   int    `json:"entries"`
			Corrupted bool   `json:"corrupted"`
		} `json:"backups"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(list.Result, &listed); err != nil {
		t.Fatalf("解析 list: %v (%s)", err, list.Result)
	}
	if listed.Count != 1 || listed.Backups[0].ID != created.ID || listed.Backups[0].Corrupted {
		t.Fatalf("步骤2 list 结果不对: %+v", listed)
	}
	t.Logf("步骤2 list: count=%d id=%s name=%s size=%d entries=%d sha=%s",
		listed.Count, listed.Backups[0].ID, listed.Backups[0].Name,
		listed.Backups[0].SizeBytes, listed.Backups[0].Entries, listed.Backups[0].SHA256)

	// --- 步骤 3：backup.verify sha_match=true ---
	verify := request(t, conn, "verify", "backup.verify", mustJSON(t, map[string]string{"id": created.ID}))
	if verify.Error != nil {
		t.Fatalf("步骤3 verify 失败: %s/%s", verify.Error.Code, verify.Error.Message)
	}
	var verified struct {
		Entries   int    `json:"entries"`
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
		SHAMatch  bool   `json:"sha_match"`
		Corrupted bool   `json:"corrupted"`
	}
	if err := json.Unmarshal(verify.Result, &verified); err != nil {
		t.Fatalf("解析 verify: %v (%s)", err, verify.Result)
	}
	if !verified.SHAMatch || verified.Corrupted || verified.Entries != created.Entries {
		t.Fatalf("步骤3 verify 结果不对: %+v", verified)
	}
	t.Logf("步骤3 verify: entries=%d size=%d sha_match=%v corrupted=%v",
		verified.Entries, verified.SizeBytes, verified.SHAMatch, verified.Corrupted)

	// --- 步骤 4：backup.restore 到 dst/ ---
	target := filepath.Join(workspace, "dst")
	restore := request(t, conn, "restore", "backup.restore", mustJSON(t, map[string]string{
		"id": created.ID, "target_dir": target,
	}))
	if restore.Error != nil {
		t.Fatalf("步骤4 restore 失败: %s/%s", restore.Error.Code, restore.Error.Message)
	}
	var restored struct {
		RestoredEntries int      `json:"restored_entries"`
		TargetDir       string   `json:"target_dir"`
		Failed          []string `json:"failed"`
		DurationMS      int64    `json:"duration_ms"`
	}
	if err := json.Unmarshal(restore.Result, &restored); err != nil {
		t.Fatalf("解析 restore: %v (%s)", err, restore.Result)
	}
	if restored.RestoredEntries != created.Entries || len(restored.Failed) != 0 {
		t.Fatalf("步骤4 restore 结果不对: %+v", restored)
	}
	t.Logf("步骤4 restore: restored_entries=%d target_dir=%s duration_ms=%d",
		restored.RestoredEntries, restored.TargetDir, restored.DurationMS)

	// --- 步骤 5：diff -r 源目录与恢复目录，退出码必须 0 ---
	diff := exec.Command("diff", "-r", src, filepath.Join(target, "src"))
	diffOutput, diffErr := diff.CombinedOutput()
	diffExit := 0
	if diffErr != nil {
		if exitErr, ok := diffErr.(*exec.ExitError); ok {
			diffExit = exitErr.ExitCode()
		} else {
			t.Fatalf("步骤5 diff 执行失败: %v", diffErr)
		}
	}
	t.Logf("步骤5 diff -r %s %s => exit=%d output=%q", src, filepath.Join(target, "src"), diffExit, strings.TrimSpace(string(diffOutput)))
	if diffExit != 0 {
		t.Fatalf("步骤5 diff 不一致，退出码 %d", diffExit)
	}

	// --- 步骤 6：backup.delete 后列表为空 ---
	deleted := request(t, conn, "delete", "backup.delete", mustJSON(t, map[string]string{"id": created.ID}))
	if deleted.Error != nil {
		t.Fatalf("步骤6 delete 失败: %s/%s", deleted.Error.Code, deleted.Error.Message)
	}
	empty := request(t, conn, "list-empty", "backup.list", `{}`)
	var emptyList struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(empty.Result, &emptyList); err != nil || empty.Error != nil {
		t.Fatalf("步骤6 list 失败: err=%v result=%s", empty.Error, empty.Result)
	}
	if emptyList.Count != 0 {
		t.Fatalf("步骤6 删除后列表应为空，实际 count=%d", emptyList.Count)
	}
	t.Logf("步骤6 delete: deleted id=%s，list count=0", created.ID)

	// --- 步骤 7：手工构造含 ../evil 的 tar，restore 整包拒绝且目标无残留 ---
	evilID := "evil-20260101T000000Z"
	writeTraversalArchive(t, filepath.Join(backups, evilID+".tar.gz"))
	evilTarget := filepath.Join(workspace, "dst-evil")
	evil := request(t, conn, "evil", "backup.restore", mustJSON(t, map[string]string{
		"id": evilID, "target_dir": evilTarget,
	}))
	if evil.Error == nil || evil.Error.Code != "E_INVALID" {
		t.Fatalf("步骤7 恶意包应返回 E_INVALID，实际 error=%+v result=%s", evil.Error, evil.Result)
	}
	t.Logf("步骤7 恶意包 restore 被拒: code=%s message=%q", evil.Error.Code, evil.Error.Message)
	if _, err := os.Lstat(evilTarget); !os.IsNotExist(err) {
		t.Fatalf("步骤7 目标目录不应存在: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, "evil")); !os.IsNotExist(err) {
		t.Fatalf("步骤7 evil 文件不应落地: %v", err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatalf("步骤7 读工作区: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".kore-restore-") {
			t.Fatalf("步骤7 遗留恢复临时目录: %s", entry.Name())
		}
	}

	// --- 收尾：kore.shutdown 优雅退出 ---
	shutdown := request(t, conn, "shutdown", "kore.shutdown", `{}`)
	if shutdown.Error != nil {
		t.Fatalf("shutdown 失败: %s/%s", shutdown.Error.Code, shutdown.Error.Message)
	}
	waitExit(t, process)
}

// writeTraversalArchive 手工写一个含 ../evil 的 tar.gz，模拟恶意备份包。
func writeTraversalArchive(t *testing.T, archivePath string) {
	t.Helper()
	file, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("创建恶意包: %v", err)
	}
	gz := gzip.NewWriter(file)
	writer := tar.NewWriter(gz)
	for _, item := range []struct {
		name    string
		content string
	}{
		{"good.txt", "good"},
		{"../evil", "evil"},
	} {
		header := &tar.Header{
			Name: item.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(item.content)),
		}
		if err := writer.WriteHeader(header); err != nil {
			_ = file.Close()
			t.Fatalf("写恶意头: %v", err)
		}
		if _, err := writer.Write([]byte(item.content)); err != nil {
			_ = file.Close()
			t.Fatalf("写恶意内容: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("关闭 gzip: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("关闭文件: %v", err)
	}
}
