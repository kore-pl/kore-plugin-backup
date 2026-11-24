//go:build linux

// 边界类别：权限。
// 非 root 下的真实权限场景：备份目录不可写 / 不可读、恢复源不可读、恢复目标父目录不可写、
// 只读源目录仍可备份、root-only 源文件被跳过。断言明确错误码，不把 EACCES 说成成功。
// CI（ubuntu-latest）以非 root 运行；root 下权限位失效，用例显式 skip 而不是假绿。
package boundary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// skipIfRoot 说明：root 绕过 DAC 权限位，chmod 000 依然可写；这类用例在 root 下没有意义。
func skipIfRoot(t *testing.T, scene string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skipf("%s：以 root 运行会绕过权限位（CI 以非 root 运行，本地以普通用户运行）", scene)
	}
}

// TestPermissionDeniedBackupDirWrite 备份目录不可写（0500）时 create 必须失败，
// 错误码 E_INTERNAL 且信息包含 permission denied——不能静默产出空备份。
func TestPermissionDeniedBackupDirWrite(t *testing.T) {
	t.Parallel()
	skipIfRoot(t, "备份目录写权限")
	workspace, opts := newWorkspace(t)
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 2, 1024)

	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		t.Fatalf("建备份目录: %v", err)
	}
	if err := os.Chmod(opts.Dir, 0o500); err != nil {
		t.Fatalf("收紧备份目录权限: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(opts.Dir, 0o700) })

	_, perr := call(t, "backup.create", mustJSON(t, map[string]any{
		"name":  "denied",
		"paths": []string{src},
	}), opts)
	if perr == nil {
		t.Fatal("备份目录不可写时 create 成功了")
	}
	if perr.Code != pluginapi.E_INTERNAL {
		t.Fatalf("错误码=%s，期望 E_INTERNAL（message=%s）", perr.Code, perr.Message)
	}
	if !strings.Contains(strings.ToLower(perr.Message), "permission denied") {
		t.Fatalf("错误信息=%q，期望包含 permission denied（权限不足必须暴露真实原因）", perr.Message)
	}
}

// TestPermissionDeniedBackupDirRead 备份目录不可读（0000）时 list 必须返回 E_INTERNAL，
// 不能伪装成「没有备份」。
func TestPermissionDeniedBackupDirRead(t *testing.T) {
	t.Parallel()
	skipIfRoot(t, "备份目录读权限")
	workspace, opts := newWorkspace(t)
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 2, 1024)
	createBackup(t, opts, "seeded", []string{src})

	if err := os.Chmod(opts.Dir, 0o000); err != nil {
		t.Fatalf("关闭备份目录权限: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(opts.Dir, 0o700) })

	_, perr := call(t, "backup.list", nil, opts)
	if perr == nil {
		t.Fatal("备份目录不可读时 list 成功了")
	}
	if perr.Code != pluginapi.E_INTERNAL {
		t.Fatalf("错误码=%s，期望 E_INTERNAL（message=%s）", perr.Code, perr.Message)
	}
	if !strings.Contains(perr.Message, "读取备份目录失败") {
		t.Fatalf("错误信息=%q，期望包含「读取备份目录失败」", perr.Message)
	}
}

// TestPermissionDeniedRestoreSource 恢复源不可读（归档 chmod 0000）时 restore 必须 E_INTERNAL。
func TestPermissionDeniedRestoreSource(t *testing.T) {
	t.Parallel()
	skipIfRoot(t, "恢复源读权限")
	workspace, opts := newWorkspace(t)
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 2, 1024)
	created := createBackup(t, opts, "locked", []string{src})

	if err := os.Chmod(created.Path, 0o000); err != nil {
		t.Fatalf("收紧归档权限: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(created.Path, 0o600) })

	target := filepath.Join(workspace, "restore-target")
	_, perr := call(t, "backup.restore", mustJSON(t, map[string]string{
		"id": created.ID, "target_dir": target,
	}), opts)
	if perr == nil {
		t.Fatal("恢复源不可读时 restore 成功了")
	}
	if perr.Code != pluginapi.E_INTERNAL {
		t.Fatalf("错误码=%s，期望 E_INTERNAL（message=%s）", perr.Code, perr.Message)
	}
	if !strings.Contains(strings.ToLower(perr.Message), "permission denied") {
		t.Fatalf("错误信息=%q，期望包含 permission denied", perr.Message)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("恢复失败后目标目录不应存在: %v", err)
	}
}

// TestPermissionDeniedRestoreTargetParent 恢复目标父目录不可写（0500）时，
// 临时目录无法创建，restore 必须 E_INTERNAL 且不留半包。
func TestPermissionDeniedRestoreTargetParent(t *testing.T) {
	t.Parallel()
	skipIfRoot(t, "恢复目标写权限")
	workspace, opts := newWorkspace(t)
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 2, 1024)
	created := createBackup(t, opts, "readonly-target", []string{src})

	parent := filepath.Join(workspace, "readonly")
	if err := os.MkdirAll(parent, 0o500); err != nil {
		t.Fatalf("建只读父目录: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	_, perr := call(t, "backup.restore", mustJSON(t, map[string]string{
		"id": created.ID, "target_dir": filepath.Join(parent, "dst"),
	}), opts)
	if perr == nil {
		t.Fatal("目标父目录不可写时 restore 成功了")
	}
	if perr.Code != pluginapi.E_INTERNAL {
		t.Fatalf("错误码=%s，期望 E_INTERNAL（message=%s）", perr.Code, perr.Message)
	}
	if !strings.Contains(perr.Message, "创建恢复临时目录失败") {
		t.Fatalf("错误信息=%q，期望包含「创建恢复临时目录失败」", perr.Message)
	}
}

// TestPermissionReadOnlySourceStillBacksUp 只读源目录（0555）不是权限不足：
// 读操作必须成功，且恢复内容与源一致。这条防止把「只读」误判成失败。
func TestPermissionReadOnlySourceStillBacksUp(t *testing.T) {
	t.Parallel()
	skipIfRoot(t, "只读源目录权限语义")
	workspace, opts := newWorkspace(t)
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 3, 2048)
	if err := os.Chmod(src, 0o555); err != nil {
		t.Fatalf("设置只读源目录: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(src, 0o755) })

	created := createBackup(t, opts, "readonly-src", []string{src})
	if report := verifyBackup(t, opts, created.ID); !report.OK {
		t.Fatalf("只读源目录备份校验失败: %+v", report.Checks)
	}
	target := filepath.Join(workspace, "restored")
	// 归档保留源目录 0555，恢复出的目录同样只读；先登记清理再恢复，
	// 否则 t.TempDir 的 RemoveAll 会因为只读目录失败（这是正确的模式保留，不是 bug）。
	t.Cleanup(func() {
		_ = filepath.WalkDir(target, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && entry.IsDir() {
				_ = os.Chmod(current, 0o755)
			}
			return nil
		})
	})
	if _, perr := call(t, "backup.restore", mustJSON(t, map[string]string{
		"id": created.ID, "target_dir": target,
	}), opts); perr != nil {
		t.Fatalf("只读源目录恢复失败: %s %s", perr.Code, perr.Message)
	}
	compareTrees(t, src, filepath.Join(target, "src"))
}

// TestPermissionDeniedRootOnlySource 非 root 读取 root-only 文件（/etc/shadow）：
// 内容读不到，插件按「跳过」处理，最终因没有可备份条目返回 E_INVALID。
// 这是真实行为（不是伪造），同时暴露一处诊断缺口：错误信息未指出权限原因，已记录为发现。
func TestPermissionDeniedRootOnlySource(t *testing.T) {
	t.Parallel()
	skipIfRoot(t, "root-only 源文件")

	const rootOnly = "/etc/shadow"
	file, err := os.Open(rootOnly)
	if err != nil {
		// 预期路径：非 root 且不在 shadow 组，open 必然 EACCES。
		if !os.IsPermission(err) {
			t.Skipf("无法确认 %s 的权限语义: %v", rootOnly, err)
		}
	} else {
		_ = file.Close()
		t.Skipf("本机 %s 可读，无法构造 root-only 场景", rootOnly)
	}
	_, opts := newWorkspace(t)
	opts.AllowRoots = []string{"/etc"}

	_, perr := call(t, "backup.create", mustJSON(t, map[string]any{
		"name":  "rootonly",
		"paths": []string{rootOnly},
	}), opts)
	if perr == nil {
		t.Fatalf("root-only 源 %s 被成功备份（不应读到内容）", rootOnly)
	}
	if perr.Code != pluginapi.E_INVALID {
		t.Fatalf("错误码=%s，期望 E_INVALID（message=%s）", perr.Code, perr.Message)
	}
	if !strings.Contains(perr.Message, "没有可备份的条目") {
		t.Fatalf("错误信息=%q，期望包含「没有可备份的条目」", perr.Message)
	}
}
