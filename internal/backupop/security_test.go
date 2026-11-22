package backupop

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestRestoreRejectsTraversal 手工构造含 ../evil 的 tar：整包 E_INVALID，
// 目标目录不落地任何文件，临时目录也不残留。
func TestRestoreRejectsTraversal(t *testing.T) {
	workspace := t.TempDir()
	backups := filepath.Join(workspace, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatalf("建备份目录: %v", err)
	}
	id := "evil-20260101T000000Z"
	writeTarGz(t, filepath.Join(backups, id+".tar.gz"), []tar.Header{
		{Name: "good.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
		{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
	}, []string{"good", "evil"})

	target := filepath.Join(workspace, "dst")
	opts := Options{Dir: backups, AllowRoots: []string{workspace}}
	params, _ := json.Marshal(map[string]string{"id": id, "target_dir": target})
	_, perr := Call("backup.restore", params, opts, nil)
	if perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("路径穿越恢复错误=%v，期望 E_INVALID", perr)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("目标目录不应存在: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, "evil")); !os.IsNotExist(err) {
		t.Fatalf("evil 不应落地: %v", err)
	}
	assertNoStaging(t, workspace)
}

// TestRestoreRejectsAbsolutePath 归档条目使用绝对路径时整包拒绝。
func TestRestoreRejectsAbsolutePath(t *testing.T) {
	workspace, opts, id := prepareMaliciousArchive(t, "abs", []tar.Header{
		{Name: "/etc/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
	}, []string{"evil"})
	_, perr := Call("backup.restore", restoreParamsJSON(t, id, filepath.Join(workspace, "dst")), opts, nil)
	if perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("绝对路径恢复错误=%v，期望 E_INVALID", perr)
	}
	assertNoStaging(t, workspace)
}

// TestRestoreRejectsDeviceAndFifo 拒绝字符/块设备与 FIFO 条目。
func TestRestoreRejectsDeviceAndFifo(t *testing.T) {
	for name, typeflag := range map[string]byte{
		"char":  tar.TypeChar,
		"block": tar.TypeBlock,
		"fifo":  tar.TypeFifo,
	} {
		t.Run(name, func(t *testing.T) {
			workspace, opts, id := prepareMaliciousArchive(t, name, []tar.Header{
				{Name: "dev0", Typeflag: typeflag, Mode: 0o600},
			}, []string{""})
			_, perr := Call("backup.restore", restoreParamsJSON(t, id, filepath.Join(workspace, "dst")), opts, nil)
			if perr == nil || perr.Code != pluginapi.E_INVALID {
				t.Fatalf("%s 恢复错误=%v，期望 E_INVALID", name, perr)
			}
		})
	}
}

// TestRestoreRejectsSymlinkEscape 符号链接目标越出恢复根时整包拒绝。
func TestRestoreRejectsSymlinkEscape(t *testing.T) {
	workspace, opts, id := prepareMaliciousArchive(t, "link", []tar.Header{
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../evil", Mode: 0o777},
	}, []string{""})
	_, perr := Call("backup.restore", restoreParamsJSON(t, id, filepath.Join(workspace, "dst")), opts, nil)
	if perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("符号链接逃逸恢复错误=%v，期望 E_INVALID", perr)
	}
}

// TestValidateRestoreHeaderLimits 超长路径与超 2 GiB 单条目在头校验阶段被拒。
func TestValidateRestoreHeaderLimits(t *testing.T) {
	long := &tar.Header{Name: strings.Repeat("a", MaxEntryNameLen+1), Typeflag: tar.TypeReg, Size: 1}
	if _, perr := validateRestoreHeader(long); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("超长路径错误=%v，期望 E_INVALID", perr)
	}
	oversize := &tar.Header{Name: "big.bin", Typeflag: tar.TypeReg, Size: MaxEntryBytes + 1}
	if _, perr := validateRestoreHeader(oversize); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("超 2GiB 条目错误=%v，期望 E_INVALID", perr)
	}
	ok := &tar.Header{Name: "中文/文件.txt", Typeflag: tar.TypeReg, Size: 1}
	if name, perr := validateRestoreHeader(ok); perr != nil || name != "中文/文件.txt" {
		t.Fatalf("合法中文路径被拒: name=%q err=%v", name, perr)
	}
}

// TestPathWhitelist 白名单外路径、/proc、相对路径与目标越界全部 E_INVALID。
func TestPathWhitelist(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	roots := []string{workspace}
	if _, perr := ResolveBackupPath(outside, roots); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("白名单外路径错误=%v，期望 E_INVALID", perr)
	}
	if _, perr := ResolveBackupPath("relative/path", roots); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("相对路径错误=%v，期望 E_INVALID", perr)
	}
	if _, perr := ResolveBackupPath("/proc/self", []string{"/proc", workspace}); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("/proc 错误=%v，期望 E_INVALID（即使白名单误配）", perr)
	}
	if _, perr := ResolveTargetDir(outside, roots); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("目标白名单外错误=%v，期望 E_INVALID", perr)
	}
	if _, perr := ResolveTargetDir("/proc/restore", []string{"/proc", workspace}); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("/proc 目标错误=%v，期望 E_INVALID", perr)
	}
}

// TestCreateRejectsCriticalMisconfigured 白名单误配为 /proc 时仍拒绝关键目录。
func TestCreateRejectsCriticalMisconfigured(t *testing.T) {
	opts := Options{Dir: filepath.Join(t.TempDir(), "backups"), AllowRoots: []string{"/proc", "/sys"}}
	_, perr := Call("backup.create", json.RawMessage(`{"name":"bad","paths":["/proc/self"]}`), opts, nil)
	if perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("关键目录错误=%v，期望 E_INVALID", perr)
	}
}

// TestCreateRejectsSymlinkEscape 祖先目录符号链接解析后越出白名单时拒绝。
func TestCreateRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatalf("建符号链接: %v", err)
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	params, _ := json.Marshal(map[string]any{
		"name": "escape", "paths": []string{filepath.Join(workspace, "escape", "src")},
	})
	_, perr := Call("backup.create", params, opts, nil)
	if perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("符号链接逃逸错误=%v，期望 E_INVALID", perr)
	}
}

// TestRestoreTargetConflict 非空目标目录 E_CONFLICT 且原文件不被覆盖。
func TestRestoreTargetConflict(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	created := createBackup(t, opts, "conflict", []string{src}, nil, "gzip")

	target := filepath.Join(workspace, "dst")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("建目标: %v", err)
	}
	existing := filepath.Join(target, "keep.txt")
	if err := os.WriteFile(existing, []byte("original"), 0o644); err != nil {
		t.Fatalf("写目标文件: %v", err)
	}
	_, perr := Call("backup.restore", restoreParamsJSON(t, created.ID, target), opts, nil)
	if perr == nil || perr.Code != pluginapi.E_CONFLICT {
		t.Fatalf("非空目标错误=%v，期望 E_CONFLICT", perr)
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "original" {
		t.Fatalf("目标原文件被改动: %q err=%v", data, err)
	}
}

// TestRestoreTargetEmptyDir 已存在的空目录可被成功替换。
func TestRestoreTargetEmptyDir(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	created := createBackup(t, opts, "empty", []string{src}, nil, "gzip")
	target := filepath.Join(workspace, "dst")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("建空目标: %v", err)
	}
	restoreBackup(t, opts, created.ID, target, nil)
	if _, err := os.Stat(filepath.Join(target, "src", "a.txt")); err != nil {
		t.Fatalf("空目标恢复失败: %v", err)
	}
}

// TestRestoreCorruptedArchive 损坏包返回 E_INVALID 且目标不落地。
func TestRestoreCorruptedArchive(t *testing.T) {
	workspace := t.TempDir()
	backups := filepath.Join(workspace, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatalf("建备份目录: %v", err)
	}
	id := "broken-20260101T000000Z"
	if err := os.WriteFile(filepath.Join(backups, id+".tar.gz"), []byte("not gzip"), 0o600); err != nil {
		t.Fatalf("写坏包: %v", err)
	}
	opts := Options{Dir: backups, AllowRoots: []string{workspace}}
	target := filepath.Join(workspace, "dst")
	_, perr := Call("backup.restore", restoreParamsJSON(t, id, target), opts, nil)
	if perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("损坏包恢复错误=%v，期望 E_INVALID", perr)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("目标不应存在: %v", err)
	}
}

// prepareMaliciousArchive 在临时工作区写入一个手工 tar.gz，返回工作区、配置与 id。
func prepareMaliciousArchive(t *testing.T, name string, headers []tar.Header, contents []string) (string, Options, string) {
	t.Helper()
	workspace := t.TempDir()
	backups := filepath.Join(workspace, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatalf("建备份目录: %v", err)
	}
	id := name + "-20260101T000000Z"
	writeTarGz(t, filepath.Join(backups, id+".tar.gz"), headers, contents)
	return workspace, Options{Dir: backups, AllowRoots: []string{workspace}}, id
}

// writeTarGz 手工写 tar.gz；contents 与 headers 等长，空串表示无内容。
func writeTarGz(t *testing.T, archivePath string, headers []tar.Header, contents []string) {
	t.Helper()
	file, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("创建恶意包: %v", err)
	}
	gz := gzip.NewWriter(file)
	writer := tar.NewWriter(gz)
	for index := range headers {
		header := headers[index]
		if err := writer.WriteHeader(&header); err != nil {
			_ = file.Close()
			t.Fatalf("写恶意头: %v", err)
		}
		if index < len(contents) && contents[index] != "" {
			if _, err := writer.Write([]byte(contents[index])); err != nil {
				_ = file.Close()
				t.Fatalf("写恶意内容: %v", err)
			}
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

// assertNoStaging 断言工作区没有遗留 .kore-restore-* 临时目录。
func assertNoStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读工作区: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".kore-restore-") {
			t.Fatalf("遗留恢复临时目录: %s", entry.Name())
		}
	}
}

// restoreParamsJSON 构造 restore 请求参数。
func restoreParamsJSON(t *testing.T, id, target string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]string{"id": id, "target_dir": target})
	if err != nil {
		t.Fatalf("序列化 restore 参数: %v", err)
	}
	return data
}
