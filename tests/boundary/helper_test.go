// Package boundary 覆盖 backup 插件的四类边界条件，全部走 backupop.Call 的真实执行面
// （exec 传 nil，即 archive/tar + compress/gzip + 真实文件系统）：
//
//	异常输入：非法备份路径、格式错误的参数与插件配置；
//	并发：并发创建备份、并发轮转清理、备份与恢复并发（-race 下断言无数据竞争）；
//	权限：备份目录不可写 / 不可读、恢复源不可读、恢复目标父目录不可写、root-only 源文件；
//	资源耗尽：真实 ulimit -f 文件大小限制、真实 tmpfs 写满（ENOSPC）。
//
// 这里不使用替身伪造错误：只有真实系统调用真的失败，断言才可能通过。
package boundary

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// helperEnv / helperMode* 是资源耗尽用例的子进程协议：父用例在注入限制的
// shell 里重新执行同一个测试二进制，只跑 helper 用例。
const (
	helperEnv        = "KORE_BOUNDARY_MODE"
	helperModeUlimit = "ulimit-f"
	helperModeTmpfs  = "tmpfs-full"
)

// mustJSON 把参数结构编码为方法载荷；编码失败属于用例自身问题。
func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化方法参数: %v", err)
	}
	return data
}

// call 走真实执行面调用一个 backup 方法（exec=nil ⇒ realFS）。
func call(t *testing.T, method string, params json.RawMessage, opts backupop.Options) (json.RawMessage, *pluginapi.Error) {
	t.Helper()
	return backupop.Call(method, params, opts, nil)
}

// newWorkspace 建一个独立工作区，并把备份目录与白名单都收敛在其中，
// 避免用例碰到宿主真实的 /var/www、/etc/kore 或 /home。
func newWorkspace(t *testing.T) (string, backupop.Options) {
	t.Helper()
	workspace := t.TempDir()
	return workspace, backupop.Options{
		Dir:        filepath.Join(workspace, "backups"),
		AllowRoots: []string{workspace},
	}
}

// createBackup 真打包一份备份并返回结构化结果。
func createBackup(t *testing.T, opts backupop.Options, name string, paths []string) backupop.CreateData {
	t.Helper()
	raw, perr := call(t, "backup.create", mustJSON(t, map[string]any{
		"name":  name,
		"paths": paths,
	}), opts)
	if perr != nil {
		t.Fatalf("backup.create(%s) 失败: %s %s", name, perr.Code, perr.Message)
	}
	var data backupop.CreateData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("解析 create 结果: %v", err)
	}
	return data
}

// listBackups 读取备份目录索引。
func listBackups(t *testing.T, opts backupop.Options) backupop.ListResult {
	t.Helper()
	raw, perr := call(t, "backup.list", nil, opts)
	if perr != nil {
		t.Fatalf("backup.list 失败: %s %s", perr.Code, perr.Message)
	}
	var data backupop.ListResult
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("解析 list 结果: %v", err)
	}
	return data
}

// verifyBackup 逐项校验一份备份（sha256 / 条目 / 可读性）。
func verifyBackup(t *testing.T, opts backupop.Options, id string) backupop.VerifyData {
	t.Helper()
	raw, perr := call(t, "backup.verify", mustJSON(t, map[string]string{"id": id}), opts)
	if perr != nil {
		t.Fatalf("backup.verify(%s) 失败: %s %s", id, perr.Code, perr.Message)
	}
	var data backupop.VerifyData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("解析 verify 结果: %v", err)
	}
	return data
}

// jobsResult 读取持久化作业记录（.jobs/ 下的真实状态机）。
func jobsResult(t *testing.T, opts backupop.Options) backupop.JobsResult {
	t.Helper()
	raw, perr := call(t, "backup.jobs", mustJSON(t, map[string]int{"limit": backupop.MaxJobsLimit}), opts)
	if perr != nil {
		t.Fatalf("backup.jobs 失败: %s %s", perr.Code, perr.Message)
	}
	var data backupop.JobsResult
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("解析 jobs 结果: %v", err)
	}
	return data
}

// fileName 返回 writeTree 生成的第 index 个文件名。
func fileName(index int) string {
	return "f-" + pad2(index) + ".bin"
}

func pad2(value int) string {
	if value < 10 {
		return "0" + string(rune('0'+value))
	}
	return string(rune('0'+value/10)) + string(rune('0'+value%10))
}

// fileContent 生成确定性的不可压缩字节：固定种子，便于恢复后逐字节比对，
// 同时保证 gzip 无法把它压到注入的文件大小限制以下。
func fileContent(index, size int) []byte {
	buf := make([]byte, size)
	random := rand.New(rand.NewSource(int64(index) + 1))
	if _, err := random.Read(buf); err != nil {
		panic("生成测试内容: " + err.Error())
	}
	return buf
}

// writeTree 在 root 下落 files 个 size 字节的源文件，模拟真实站点目录。
func writeTree(t *testing.T, root string, files, size int) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("创建源目录: %v", err)
	}
	for index := 0; index < files; index++ {
		path := filepath.Join(root, fileName(index))
		if err := os.WriteFile(path, fileContent(index, size), 0o644); err != nil {
			t.Fatalf("写入源文件 %s: %v", path, err)
		}
	}
}

// compareTrees 逐文件比对两棵目录树的内容（恢复结果必须与源一致）。
func compareTrees(t *testing.T, wantRoot, gotRoot string) {
	t.Helper()
	checked := 0
	err := filepath.WalkDir(wantRoot, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(wantRoot, current)
		if err != nil {
			return err
		}
		want, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(gotRoot, rel))
		if err != nil {
			t.Fatalf("恢复结果缺文件 %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Fatalf("恢复内容不一致: %s（源 %d 字节，恢复 %d 字节）", rel, len(want), len(got))
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("比对恢复结果: %v", err)
	}
	if checked == 0 {
		t.Fatal("源目录为空，比对没有意义")
	}
}
