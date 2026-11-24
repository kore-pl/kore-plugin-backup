// 边界类别：并发。
// 三类真实并发：并发创建备份（同目录、同样例来源、独立 name）、并发轮转清理（同一批包
// 同时被多个 retention 调用判定）、备份与恢复并发（同一份包被多路读取，同时新包在写入）。
// 断言口径：不丢作业记录、不产生重复 id、不误删最新包、恢复内容逐字节一致；
// 数据竞争由 `go test -race` 判定（-race 下本文件必须 PASS）。
package boundary

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestConcurrentCreateDistinctNames 8 路真并发创建：每份包都必须独立落盘、sha256 可校验，
// 且 .jobs/ 作业记录一份不少（并发写作业状态不得互相覆盖）。
func TestConcurrentCreateDistinctNames(t *testing.T) {
	t.Parallel()
	workspace, opts := newWorkspace(t)
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 16, 32*1024)

	const workers = 8
	params := make([]json.RawMessage, workers)
	for index := range params {
		params[index] = mustJSON(t, map[string]any{
			"name":  fmt.Sprintf("snap-%d", index),
			"paths": []string{src},
		})
	}

	ids := make([]string, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			raw, perr := backupop.Call("backup.create", params[index], opts, nil)
			if perr != nil {
				t.Errorf("并发 backup.create[%d] 失败: %s %s", index, perr.Code, perr.Message)
				return
			}
			var data backupop.CreateData
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Errorf("解析并发 create[%d] 结果: %v", index, err)
				return
			}
			ids[index] = data.ID
		}(index)
	}
	close(start)
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	seen := make(map[string]bool, workers)
	for index, id := range ids {
		if id == "" {
			t.Fatalf("并发 create[%d] 没有返回 id", index)
		}
		if seen[id] {
			t.Fatalf("并发 create 产生重复 id: %s", id)
		}
		seen[id] = true
	}

	list := listBackups(t, opts)
	if list.Count != workers {
		t.Fatalf("并发创建后备份数=%d，期望 %d（有包被覆盖或丢失）", list.Count, workers)
	}
	for _, id := range ids {
		report := verifyBackup(t, opts, id)
		if !report.OK {
			t.Fatalf("并发创建的备份 %s 校验失败: %+v", id, report.Checks)
		}
	}

	jobs := jobsResult(t, opts)
	if jobs.Count != workers {
		t.Fatalf("作业记录数=%d，期望 %d（并发写 .jobs 覆盖了记录）", jobs.Count, workers)
	}
	for _, job := range jobs.Jobs {
		if job.Status != backupop.JobStatusSucceeded {
			t.Fatalf("作业 %s 状态=%s，并发创建成功后应为 %s", job.ID, job.Status, backupop.JobStatusSucceeded)
		}
	}
}

// TestConcurrentRetentionCleanup 6 路并发轮转（keep=3）：同一批 8 份包被多个调用同时判定，
// 最新 3 份不得被任何一路删除，其余删除只允许因「已被别的调用删掉」而返回 E_NOTFOUND。
func TestConcurrentRetentionCleanup(t *testing.T) {
	t.Parallel()
	workspace, opts := newWorkspace(t)
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 4, 16*1024)

	const seeded = 8
	ids := make([]string, seeded)
	for index := 0; index < seeded; index++ {
		ids[index] = createBackup(t, opts, fmt.Sprintf("seed-%d", index), []string{src}).ID
	}
	// 保留窗口 = 创建顺序最后 3 份；并发调用只允许删窗口外的包。
	expectedKept := make(map[string]bool, 3)
	for _, id := range ids[seeded-3:] {
		expectedKept[id] = true
	}

	const workers = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	unexpected := make([]string, 0, workers)
	start := make(chan struct{})
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, perr := backupop.Call("backup.retention", json.RawMessage(`{"keep":3}`), opts, nil)
			// 并发删除天然存在「别人先删掉了」的窗口，E_NOTFOUND 是合法结果；
			// 其他错误（E_INTERNAL / E_CONFLICT）都说明并发清理不安全。
			if perr != nil && perr.Code != pluginapi.E_NOTFOUND {
				mu.Lock()
				unexpected = append(unexpected, fmt.Sprintf("%s: %s", perr.Code, perr.Message))
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(unexpected) != 0 {
		t.Fatalf("并发轮转出现非预期错误: %v", unexpected)
	}

	list := listBackups(t, opts)
	if list.Count != 3 {
		t.Fatalf("并发轮转后剩余=%d 份，期望 3 份", list.Count)
	}
	for _, backup := range list.Backups {
		if !expectedKept[backup.ID] {
			t.Fatalf("并发轮转删除了保留窗口内的最新备份 %s", backup.ID)
		}
		if report := verifyBackup(t, opts, backup.ID); !report.OK {
			t.Fatalf("并发轮转后备份 %s 损坏: %+v", backup.ID, report.Checks)
		}
	}
}

// TestConcurrentBackupAndRestore 备份与恢复并发：同一份包被 4 路同时恢复到不同目标，
// 同时 4 路在写新包；恢复内容必须与源逐字节一致，新包必须全部成功。
func TestConcurrentBackupAndRestore(t *testing.T) {
	t.Parallel()
	workspace, opts := newWorkspace(t)
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 12, 16*1024)
	seed := createBackup(t, opts, "seed", []string{src})

	const workers = 4
	restoreParams := make([]json.RawMessage, workers)
	targets := make([]string, workers)
	createParams := make([]json.RawMessage, workers)
	for index := 0; index < workers; index++ {
		target := filepath.Join(workspace, fmt.Sprintf("restore-%d", index))
		targets[index] = target
		restoreParams[index] = mustJSON(t, map[string]string{"id": seed.ID, "target_dir": target})
		createParams[index] = mustJSON(t, map[string]any{
			"name":  fmt.Sprintf("parallel-%d", index),
			"paths": []string{src},
		})
	}

	createIDs := make([]string, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for index := 0; index < workers; index++ {
		wg.Add(2)
		go func(index int) {
			defer wg.Done()
			<-start
			if _, perr := backupop.Call("backup.restore", restoreParams[index], opts, nil); perr != nil {
				t.Errorf("并发 restore[%d] 失败: %s %s", index, perr.Code, perr.Message)
			}
		}(index)
		go func(index int) {
			defer wg.Done()
			<-start
			raw, perr := backupop.Call("backup.create", createParams[index], opts, nil)
			if perr != nil {
				t.Errorf("并发 create[%d] 失败: %s %s", index, perr.Code, perr.Message)
				return
			}
			var data backupop.CreateData
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Errorf("解析并发 create[%d] 结果: %v", index, err)
				return
			}
			createIDs[index] = data.ID
		}(index)
	}
	close(start)
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	for _, target := range targets {
		// 归档里的相对根是源目录名（src/），逐字节比对恢复内容。
		compareTrees(t, src, filepath.Join(target, "src"))
	}
	list := listBackups(t, opts)
	if list.Count != workers+1 {
		t.Fatalf("备份与恢复并发后备份数=%d，期望 %d", list.Count, workers+1)
	}
	for _, id := range createIDs {
		if report := verifyBackup(t, opts, id); !report.OK {
			t.Fatalf("并发写入的备份 %s 校验失败: %+v", id, report.Checks)
		}
	}
}
