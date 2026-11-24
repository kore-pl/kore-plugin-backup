//go:build linux

// 边界类别：资源耗尽——真实注入，不使用替身伪造 ENOSPC/EFBIG。
//
//  1. ulimit -f：父用例在 `bash -c 'ulimit -f N; exec <测试二进制> -test.run=helper'`
//     子进程里真打包，内核在文件超过 RLIMIT_FSIZE 时返回 EFBIG（file too large）。
//  2. tmpfs 满：父用例用 `unshare -rm` 起非特权 mount namespace，挂 1 MiB tmpfs，
//     在真写满的 tmpfs 上打包，内核返回 ENOSPC（no space left on device）。
//     内核不允许非特权 user namespace 时该用例显式 skip（CI 若受限则跳过，不假绿）；
//     ulimit -f 用例无此依赖，始终覆盖资源耗尽。
//  3. 插件自身单次上限（MaxBytes）：真超限时中止并删除半成品。
//
// 三个用例都断言「失败后目录里不留 .tar.gz/.sha256/.meta.json 半成品」
// ——半成品是恢复时最危险的输入，清理逻辑必须被真实失败路径覆盖。
package boundary

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// helper 子进程环境变量：源目录、备份目录、白名单根。
const (
	helperSrcEnv  = "KORE_BOUNDARY_SRC"
	helperDirEnv  = "KORE_BOUNDARY_DIR"
	helperRootEnv = "KORE_BOUNDARY_ROOT"
)

// TestResourceExhaustionFileSizeLimit 真实 ulimit -f 注入：文件大小限制让落盘写失败，
// create 必须返回 E_INTERNAL 并清理半成品。
func TestResourceExhaustionFileSizeLimit(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("缺少 bash，无法注入 ulimit -f: %v", err)
	}
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 1, 4<<20) // 4 MiB 不可压缩内容，必然超过 128 KiB 限制
	dir := filepath.Join(workspace, "backups")

	out := runHelperUnder(t, bash, []string{"-c",
		fmt.Sprintf("ulimit -f 128 && exec %q -test.run=^TestResourceExhaustionHelper$ -test.v", os.Args[0]),
	}, map[string]string{
		helperEnv:     helperModeUlimit,
		helperSrcEnv:  src,
		helperDirEnv:  dir,
		helperRootEnv: workspace,
	})
	if !bytes.Contains(out, []byte("HELPER_OK")) {
		t.Fatalf("ulimit -f 注入下用例未通过:\n%s", out)
	}
}

// TestResourceExhaustionTmpfsFull 真实 tmpfs 写满注入（`unshare -rm` + 1 MiB tmpfs），
// create 必须返回 E_INTERNAL（ENOSPC）并清理半成品。
func TestResourceExhaustionTmpfsFull(t *testing.T) {
	t.Parallel()
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skipf("缺少 unshare，无法构造 tmpfs 满场景: %v", err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("缺少 bash: %v", err)
	}
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 1, 4<<20)
	mountpoint := filepath.Join(workspace, "mnt")
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatalf("建挂载点: %v", err)
	}
	script := fmt.Sprintf("mount -t tmpfs -o size=1m tmpfs %q && exec %q -test.run=^TestResourceExhaustionHelper$ -test.v",
		mountpoint, os.Args[0])
	cmd := exec.Command(unshare, "-rm", bash, "-c", script)
	cmd.Env = helperEnvVars(map[string]string{
		helperEnv:     helperModeTmpfs,
		helperSrcEnv:  src,
		helperDirEnv:  filepath.Join(mountpoint, "backups"),
		helperRootEnv: workspace,
	})
	out, runErr := cmd.CombinedOutput()
	if bytes.Contains(out, []byte("HELPER_OK")) && runErr == nil {
		return
	}
	if bytes.Contains(out, []byte("--- FAIL")) {
		t.Fatalf("tmpfs 满注入下用例失败:\n%s", out)
	}
	t.Skipf("本机不允许非特权 mount namespace，tmpfs 满场景跳过（ulimit -f 用例仍覆盖资源耗尽）: %v\n%s", runErr, out)
}

// TestResourceExhaustionMaxBytesCap 插件自身单次上限：超限必须 E_INVALID 中止并清理半成品。
func TestResourceExhaustionMaxBytesCap(t *testing.T) {
	t.Parallel()
	workspace, opts := newWorkspace(t)
	opts.MaxBytes = 4096
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 2, 8192) // 单文件 8 KiB > 4 KiB 上限

	_, perr := call(t, "backup.create", mustJSON(t, map[string]any{
		"name":  "overcap",
		"paths": []string{src},
	}), opts)
	if perr == nil {
		t.Fatal("超过 MaxBytes 上限的备份成功了")
	}
	if perr.Code != pluginapi.E_INVALID {
		t.Fatalf("错误码=%s，期望 E_INVALID（message=%s）", perr.Code, perr.Message)
	}
	if !strings.Contains(perr.Message, "上限") {
		t.Fatalf("错误信息=%q，期望包含「上限」", perr.Message)
	}
	assertNoPartialArchive(t, opts.Dir)
}

// TestResourceExhaustionHelper 只在父用例构造的受限子进程里执行；普通 go test 直接 skip。
//
// 断言：注入先被探测证明生效 → create 返回 E_INTERNAL 且错误信息命中真实 errno
// → 目录里没有半成品（这条是变异自证目标：去掉失败清理后这里必须 FAIL）。
func TestResourceExhaustionHelper(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("helper 进程专用：由 ulimit -f / tmpfs 满父用例在受限环境下启动")
	}
	if mode == helperModeUlimit {
		// 写超限时内核会发 SIGXFSZ；忽略后表现为 EFBIG，便于断言而不是直接杀进程。
		signal.Ignore(syscall.SIGXFSZ)
	}
	src := os.Getenv(helperSrcEnv)
	dir := os.Getenv(helperDirEnv)
	root := os.Getenv(helperRootEnv)
	if src == "" || dir == "" || root == "" {
		t.Fatalf("helper 环境变量不完整: src=%q dir=%q root=%q", src, dir, root)
	}

	// 第一步：证明注入真实生效（探测写必须失败）。
	probeSize := 512 << 10
	if mode == helperModeTmpfs {
		probeSize = 2 << 20 // tmpfs 只有 1 MiB，2 MiB 必然失败
	}
	probe := filepath.Join(dir, "injection-probe.bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建备份目录: %v", err)
	}
	if err := os.WriteFile(probe, make([]byte, probeSize), 0o600); err == nil {
		t.Fatalf("[%s] 注入未生效：%d 字节探测写成功", mode, probeSize)
	}
	_ = os.Remove(probe)

	// 第二步：真实 backup.create 必须失败，且错误码/信息可判定。
	opts := backupop.Options{Dir: dir, AllowRoots: []string{root}}
	params, err := json.Marshal(map[string]any{"name": "exhaust", "paths": []string{src}})
	if err != nil {
		t.Fatalf("构造参数: %v", err)
	}
	_, perr := backupop.Call("backup.create", params, opts, nil)
	if perr == nil {
		t.Fatalf("[%s] 资源耗尽下 create 成功了", mode)
	}
	if perr.Code != pluginapi.E_INTERNAL {
		t.Fatalf("[%s] 错误码=%s，期望 E_INTERNAL（message=%s）", mode, perr.Code, perr.Message)
	}
	// 发现（不改业务代码）：archive.make 的读侧错误只保留「已写 N 字节」，把底层
	// EFBIG/ENOSPC 的 errno 文案吞掉了，排障只能靠注入前探针。断言只要求
	// 真实失败被归为 E_INTERNAL 且错误来自打包写入路径；注入真实性由上面的探针证明。
	if message := strings.ToLower(perr.Message); !strings.Contains(message, "读取文件内容失败") &&
		!strings.Contains(message, "too large") && !strings.Contains(message, "no space left") {
		t.Fatalf("[%s] 错误信息=%q，期望来自打包写入路径的真实失败", mode, perr.Message)
	}

	// 第三步：失败后不得留半成品（恢复半包 = 数据损坏）。
	assertNoPartialArchive(t, dir)
	fmt.Printf("HELPER_OK mode=%s code=%s message=%s\n", mode, perr.Code, perr.Message)
}

// runHelperUnder 在给定命令下启动 helper 子进程并合并输出。
func runHelperUnder(t *testing.T, name string, args []string, env map[string]string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = helperEnvVars(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("受限子进程退出失败: %v\n%s", err, out)
	}
	return out
}

// helperEnvVars 在父进程环境上追加 helper 协议变量，保留 PATH/GOCACHE 等。
func helperEnvVars(extra map[string]string) []string {
	env := append(os.Environ(), helperEnv+"="+extra[helperEnv],
		helperSrcEnv+"="+extra[helperSrcEnv],
		helperDirEnv+"="+extra[helperDirEnv],
		helperRootEnv+"="+extra[helperRootEnv],
	)
	return env
}

// assertNoPartialArchive 断言目录里没有 .tar.gz / .sha256 / .meta.json 残留。
func assertNoPartialArchive(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("读取备份目录: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tar") ||
			strings.HasSuffix(name, ".sha256") || strings.HasSuffix(name, ".meta.json") {
			t.Fatalf("资源耗尽失败后残留半成品: %s", name)
		}
	}
}
