package backupop

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCreateRestoreRoundTrip 打包目录树（子目录/空文件/大文件/中文名/符号链接）后恢复，
// 逐文件比对内容、权限、mtime 与符号链接目标。
func TestCreateRestoreRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o750); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	large := bytes.Repeat([]byte("kore-backup-roundtrip-"), 100_000)
	files := map[string]struct {
		content []byte
		mode    os.FileMode
	}{
		"a.txt":         {[]byte("hello backup\n"), 0o640},
		"empty.txt":     {nil, 0o600},
		"sub/b.txt":     {[]byte("nested\n"), 0o644},
		"中文.txt":        {[]byte("中文内容\n"), 0o644},
		"sub/large.bin": {large, 0o600},
	}
	for name, file := range files {
		path := filepath.Join(src, filepath.FromSlash(name))
		if err := os.WriteFile(path, file.content, file.mode); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
		if err := os.Chmod(path, file.mode); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Fatalf("建符号链接: %v", err)
	}
	fixed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	for name := range files {
		if err := os.Chtimes(filepath.Join(src, filepath.FromSlash(name)), fixed, fixed); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}

	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	created := createBackup(t, opts, "snap", []string{src}, nil, "gzip")
	if created.Entries < 8 {
		t.Fatalf("条目数=%d，期望至少 8（目录+文件+符号链接）", created.Entries)
	}
	info, err := os.Stat(created.Path)
	if err != nil {
		t.Fatalf("备份包不存在: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("备份包权限=%o，期望 0600", info.Mode().Perm())
	}
	for _, sidecar := range []string{created.Path + ".sha256", created.Path + ".meta.json"} {
		side, err := os.Stat(sidecar)
		if err != nil {
			t.Fatalf("sidecar 不存在: %v", err)
		}
		if side.Mode().Perm() != 0o600 {
			t.Fatalf("sidecar %s 权限=%o，期望 0600", filepath.Base(sidecar), side.Mode().Perm())
		}
	}

	target := filepath.Join(workspace, "dst")
	restored := restoreBackup(t, opts, created.ID, target, nil)
	if restored.RestoredEntries != created.Entries {
		t.Fatalf("恢复条目=%d，期望 %d", restored.RestoredEntries, created.Entries)
	}
	for name, file := range files {
		path := filepath.Join(target, "src", filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读恢复文件 %s: %v", name, err)
		}
		if !bytes.Equal(data, file.content) {
			t.Fatalf("%s 内容不一致：恢复 %d 字节，期望 %d 字节", name, len(data), len(file.content))
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != file.mode {
			t.Fatalf("%s 权限=%o，期望 %o", name, info.Mode().Perm(), file.mode)
		}
		if delta := info.ModTime().Unix() - fixed.Unix(); delta < -1 || delta > 1 {
			t.Fatalf("%s mtime=%v，期望约 %v", name, info.ModTime(), fixed)
		}
	}
	link, err := os.Readlink(filepath.Join(target, "src", "link.txt"))
	if err != nil || link != "a.txt" {
		t.Fatalf("符号链接目标=%q err=%v，期望 a.txt", link, err)
	}
}

// TestCreateSingleFile 单个文件路径也能打包并恢复（不过滤父目录）。
func TestCreateSingleFile(t *testing.T) {
	workspace := t.TempDir()
	file := filepath.Join(workspace, "only.txt")
	if err := os.WriteFile(file, []byte("single\n"), 0o600); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	created := createBackup(t, opts, "single", []string{file}, nil, "gzip")
	target := filepath.Join(workspace, "dst")
	restoreBackup(t, opts, created.ID, target, nil)
	data, err := os.ReadFile(filepath.Join(target, "only.txt"))
	if err != nil || string(data) != "single\n" {
		t.Fatalf("单文件恢复内容=%q err=%v", data, err)
	}
}

// TestExcludeGlob exclude glob（含 basename 匹配）生效且不误删其它文件。
func TestExcludeGlob(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	for _, name := range []string{"keep.txt", "drop.log", "sub/nested.log", "sub/keep2.txt"} {
		if err := os.WriteFile(filepath.Join(src, filepath.FromSlash(name)), []byte(name), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	created := createBackup(t, opts, "exclude", []string{src}, []string{"*.log"}, "gzip")
	target := filepath.Join(workspace, "dst")
	restoreBackup(t, opts, created.ID, target, nil)

	if _, err := os.Stat(filepath.Join(target, "src", "drop.log")); !os.IsNotExist(err) {
		t.Fatalf("drop.log 应被 exclude 排除: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "src", "sub", "nested.log")); !os.IsNotExist(err) {
		t.Fatalf("sub/nested.log 应被 exclude 排除: %v", err)
	}
	for _, name := range []string{"keep.txt", "sub/keep2.txt"} {
		if _, err := os.Stat(filepath.Join(target, "src", filepath.FromSlash(name))); err != nil {
			t.Fatalf("%s 应保留: %v", name, err)
		}
	}
}

// TestCompressionNone compression=none 生成 .tar 并能恢复。
func TestCompressionNone(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("plain\n"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	created := createBackup(t, opts, "plain", []string{src}, nil, "none")
	if filepath.Ext(created.Path) != ".tar" {
		t.Fatalf("compression=none 应生成 .tar，实际 %s", created.Path)
	}
	target := filepath.Join(workspace, "dst")
	restoreBackup(t, opts, created.ID, target, nil)
	if data, err := os.ReadFile(filepath.Join(target, "src", "a.txt")); err != nil || string(data) != "plain\n" {
		t.Fatalf("恢复内容=%q err=%v", data, err)
	}
}

// TestDeterministicSHA 相同源两次打包 sha256 一致（排除时间戳/顺序抖动）。
func TestDeterministicSHA(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("deterministic\n"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	first := createBackup(t, opts, "first", []string{src}, nil, "gzip")
	second := createBackup(t, opts, "second", []string{src}, nil, "gzip")
	t.Logf("first sha256=%s", first.SHA256)
	t.Logf("second sha256=%s", second.SHA256)
	if first.SHA256 != second.SHA256 {
		t.Fatalf("两次打包 sha256 不一致: %s != %s", first.SHA256, second.SHA256)
	}
}

// TestCreateLimitCleansPartial 超过字节上限时中止并删除半成品。
func TestCreateLimitCleansPartial(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	backups := filepath.Join(workspace, "backups")
	opts := Options{Dir: backups, AllowRoots: []string{workspace}, MaxBytes: 1024}
	_, perr := Call("backup.create", json.RawMessage(`{"name":"limit","paths":[`+quote(src)+`]}`), opts, nil)
	if perr == nil || perr.Code != "E_INVALID" {
		t.Fatalf("超限错误=%v，期望 E_INVALID", perr)
	}
	assertDirEmpty(t, backups)

	opts.MaxBytes = 0
	opts.MaxEntries = 1
	_, perr = Call("backup.create", json.RawMessage(`{"name":"limit","paths":[`+quote(src)+`]}`), opts, nil)
	if perr == nil || perr.Code != "E_INVALID" {
		t.Fatalf("条目超限错误=%v，期望 E_INVALID", perr)
	}
	assertDirEmpty(t, backups)
}

// TestVerifyTampered 篡改包后 sha_match=false，且 verify 仍返回结果。
func TestVerifyTampered(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("verify me\n"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	created := createBackup(t, opts, "verify", []string{src}, nil, "gzip")

	good := verifyBackup(t, opts, created.ID)
	if !good.SHAMatch || good.Corrupted || good.Entries == 0 {
		t.Fatalf("原始包校验结果不对: %+v", good)
	}

	file, err := os.OpenFile(created.Path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("打开包: %v", err)
	}
	if _, err := file.WriteAt([]byte{0x00}, 12); err != nil {
		_ = file.Close()
		t.Fatalf("篡改包: %v", err)
	}
	_ = file.Close()

	tampered := verifyBackup(t, opts, created.ID)
	if tampered.SHAMatch {
		t.Fatalf("篡改后 sha_match 仍为 true: %+v", tampered)
	}
}

// TestVerifyMissingSidecar 缺 sidecar 时 sha_match=false 但内容可解。
func TestVerifyMissingSidecar(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	created := createBackup(t, opts, "noside", []string{src}, nil, "gzip")
	if err := os.Remove(created.Path + ".sha256"); err != nil {
		t.Fatalf("删 sidecar: %v", err)
	}
	result := verifyBackup(t, opts, created.ID)
	if result.SHAMatch || result.Corrupted || result.Entries == 0 {
		t.Fatalf("缺 sidecar 校验结果不对: %+v", result)
	}
}

// TestListCorrupted 非 tar 内容标记 corrupted=true。
func TestListCorrupted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad-20260101T000000Z.tar.gz"), []byte("not a gzip"), 0o600); err != nil {
		t.Fatalf("写坏包: %v", err)
	}
	items, err := realFS{}.List(dir)
	if err != nil || len(items) != 1 {
		t.Fatalf("List=%v err=%v", items, err)
	}
	if !items[0].Corrupted {
		t.Fatalf("坏包未标记 corrupted: %+v", items[0])
	}
}

// TestDeleteNotFound 删除不存在备份返回 E_NOTFOUND，删除后 sidecar 一并清理。
func TestDeleteNotFound(t *testing.T) {
	workspace := t.TempDir()
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	_, perr := Call("backup.delete", json.RawMessage(`{"id":"nope-20260101T000000Z"}`), opts, nil)
	if perr == nil || perr.Code != "E_NOTFOUND" {
		t.Fatalf("删除不存在备份错误=%v，期望 E_NOTFOUND", perr)
	}

	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	created := createBackup(t, opts, "gone", []string{src}, nil, "gzip")
	if _, perr := Call("backup.delete", json.RawMessage(`{"id":`+quote(created.ID)+`}`), opts, nil); perr != nil {
		t.Fatalf("删除失败: %v", perr)
	}
	for _, path := range []string{created.Path, created.Path + ".sha256", created.Path + ".meta.json"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s 应已删除: %v", path, err)
		}
	}
}

// TestRestorePathFilter 只恢复指定条目，过滤项未命中时进入 failed。
func TestRestorePathFilter(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	for _, name := range []string{"a.txt", "sub/b.txt"} {
		if err := os.WriteFile(filepath.Join(src, filepath.FromSlash(name)), []byte(name), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	created := createBackup(t, opts, "filter", []string{src}, nil, "gzip")
	target := filepath.Join(workspace, "dst")
	restored := restoreBackup(t, opts, created.ID, target, []string{"src/sub/b.txt", "src/missing.txt"})
	if restored.RestoredEntries != 1 {
		t.Fatalf("过滤恢复条目=%d，期望 1", restored.RestoredEntries)
	}
	if _, err := os.Stat(filepath.Join(target, "src", "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("a.txt 不应被恢复: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "src", "sub", "b.txt")); err != nil || string(data) != "sub/b.txt" {
		t.Fatalf("b.txt 恢复内容=%q err=%v", data, err)
	}
	if len(restored.Failed) != 1 || restored.Failed[0] != "src/missing.txt" {
		t.Fatalf("failed=%v，期望 [src/missing.txt]", restored.Failed)
	}
}

// createBackup 经 Call 创建备份并断言成功。
func createBackup(t *testing.T, opts Options, name string, paths, exclude []string, compression string) CreateData {
	t.Helper()
	params, _ := json.Marshal(map[string]any{
		"name": name, "paths": paths, "exclude": exclude, "compression": compression,
	})
	result, perr := Call("backup.create", params, opts, nil)
	if perr != nil {
		t.Fatalf("create 失败: %s/%s", perr.Code, perr.Message)
	}
	var data CreateData
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("解析 create: %v", err)
	}
	return data
}

// restoreBackup 经 Call 恢复并断言成功。
func restoreBackup(t *testing.T, opts Options, id, target string, paths []string) RestoreData {
	t.Helper()
	payload := map[string]any{"id": id, "target_dir": target}
	if len(paths) > 0 {
		payload["paths"] = paths
	}
	params, _ := json.Marshal(payload)
	result, perr := Call("backup.restore", params, opts, nil)
	if perr != nil {
		t.Fatalf("restore 失败: %s/%s", perr.Code, perr.Message)
	}
	var data RestoreData
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("解析 restore: %v", err)
	}
	return data
}

// verifyBackup 经 Call 校验并断言成功。
func verifyBackup(t *testing.T, opts Options, id string) VerifyData {
	t.Helper()
	result, perr := Call("backup.verify", json.RawMessage(`{"id":`+quote(id)+`}`), opts, nil)
	if perr != nil {
		t.Fatalf("verify 失败: %s/%s", perr.Code, perr.Message)
	}
	var data VerifyData
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("解析 verify: %v", err)
	}
	return data
}

// assertDirEmpty 断言目录不存在或为空。
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("读目录: %v", err)
	}
	// 作业记录（.jobs/）是 create 的持久化状态，不属于"半成品归档"，不在此断言范围。
	residue := 0
	for _, entry := range entries {
		if entry.Name() == jobsDirName {
			continue
		}
		residue++
	}
	if residue != 0 {
		t.Fatalf("目录 %s 应无残留，实际 %d 项", dir, residue)
	}
}

// quote 把字符串编码为 JSON 字面量。
func quote(raw string) string {
	data, _ := json.Marshal(raw)
	return string(data)
}
