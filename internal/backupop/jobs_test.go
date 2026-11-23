package backupop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestJobsRecordsSucceededCreate 校验 create 成功时落一条 succeeded 作业记录，
// 含真实条目/字节、归档 id、pack 阶段与维度。
func TestJobsRecordsSucceededCreate(t *testing.T) {
	workspace := t.TempDir()
	source := prepareSource(t, workspace)
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	created, perr := Call("backup.create", json.RawMessage(`{"name":"snap","paths":[`+quote(source)+`]}`), opts, nil)
	if perr != nil {
		t.Fatalf("create: %v", perr)
	}
	var create struct {
		ID   string     `json:"id"`
		Job  *JobRecord `json:"job"`
		Stgs []JobStage `json:"stages"`
	}
	if err := json.Unmarshal(created, &create); err != nil {
		t.Fatalf("解析 create: %v", err)
	}
	if create.Job == nil || create.Job.Status != JobStatusSucceeded {
		t.Fatalf("create 结果应带 succeeded 作业: %s", created)
	}
	if create.Job.ArchiveID != create.ID || create.Job.Entries == 0 || create.Job.Bytes == 0 ||
		create.Job.ArchiveBytes == 0 || create.Job.SHA256 == "" {
		t.Fatalf("作业计数/关联字段不完整: %+v", create.Job)
	}
	if len(create.Job.Stages) == 0 || create.Job.Stages[0].Name != "pack" {
		t.Fatalf("作业阶段不对: %+v", create.Job.Stages)
	}
	if len(create.Job.Dimensions) != 1 || create.Job.Dimensions[0] != DimensionSite {
		t.Fatalf("作业维度不对: %+v", create.Job.Dimensions)
	}

	raw, perr := Call("backup.jobs", json.RawMessage(`{}`), opts, nil)
	if perr != nil {
		t.Fatalf("jobs: %v", perr)
	}
	var result JobsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析 jobs: %v", err)
	}
	if result.Count != 1 || result.Jobs[0].ID != create.Job.ID || result.Hint == "" {
		t.Fatalf("jobs 结果不对: %+v", result)
	}
}

// TestJobsRecordsFailedCreate 校验 create 失败时记录 failed 与错误原因，不假装成功。
func TestJobsRecordsFailedCreate(t *testing.T) {
	workspace := t.TempDir()
	source := prepareSource(t, workspace)
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}, MaxBytes: 1}
	if _, perr := Call("backup.create", json.RawMessage(`{"name":"snap","paths":[`+quote(source)+`]}`), opts, nil); perr == nil {
		t.Fatal("超限 create 应失败")
	}
	raw, perr := Call("backup.jobs", json.RawMessage(`{}`), opts, nil)
	if perr != nil {
		t.Fatalf("jobs: %v", perr)
	}
	var result JobsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析 jobs: %v", err)
	}
	if result.Count != 1 || result.Jobs[0].Status != JobStatusFailed {
		t.Fatalf("应有 1 条 failed 记录: %+v", result)
	}
	if !strings.Contains(result.Jobs[0].Error, "上限") {
		t.Fatalf("失败原因未记录: %q", result.Jobs[0].Error)
	}
}

// stateProbeExecutor 在 Create 期间读作业文件，验证 pending → running 状态推进。
type stateProbeExecutor struct {
	realFS
	sawRunning bool
	probeErr   error
}

func (p *stateProbeExecutor) Create(spec CreateSpec) (CreateData, error) {
	entries, err := os.ReadDir(filepath.Join(spec.Dir, jobsDirName))
	if err != nil {
		p.probeErr = err
	} else {
		for _, entry := range entries {
			data, readErr := os.ReadFile(filepath.Join(spec.Dir, jobsDirName, entry.Name()))
			if readErr != nil {
				continue
			}
			var record JobRecord
			if json.Unmarshal(data, &record) == nil && record.Status == JobStatusRunning {
				p.sawRunning = true
			}
		}
	}
	return p.realFS.Create(spec)
}

// TestJobsStateMachineRunning 校验执行面运行期间作业文件状态已推进到 running（真实可轮询）。
func TestJobsStateMachineRunning(t *testing.T) {
	workspace := t.TempDir()
	source := prepareSource(t, workspace)
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	probe := &stateProbeExecutor{}
	if _, perr := Call("backup.create", json.RawMessage(`{"name":"snap","paths":[`+quote(source)+`]}`), opts, probe); perr != nil {
		t.Fatalf("create: %v", perr)
	}
	if probe.probeErr != nil {
		t.Fatalf("读作业目录: %v", probe.probeErr)
	}
	if !probe.sawRunning {
		t.Fatal("执行期间作业状态未推进到 running")
	}
}

// TestJobsLimitAndFilter 校验 limit 边界与按 id 过滤。
func TestJobsLimitAndFilter(t *testing.T) {
	workspace := t.TempDir()
	source := prepareSource(t, workspace)
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	for i := 0; i < 3; i++ {
		// 同名同秒会命中 id 冲突（既有语义），这里用不同 name 造多条作业。
		name := "snap" + itoa(int64(i))
		if _, perr := Call("backup.create", json.RawMessage(`{"name":"`+name+`","paths":[`+quote(source)+`]}`), opts, nil); perr != nil {
			t.Fatalf("create %d: %v", i, perr)
		}
	}
	raw, perr := Call("backup.jobs", json.RawMessage(`{"limit":2}`), opts, nil)
	if perr != nil {
		t.Fatalf("jobs: %v", perr)
	}
	var result JobsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if result.Count != 2 || len(result.Jobs) != 2 {
		t.Fatalf("limit=2 应返回 2 条: %+v", result)
	}
	target := result.Jobs[1].ID
	raw, perr = Call("backup.jobs", json.RawMessage(`{"id":"`+target+`"}`), opts, nil)
	if perr != nil {
		t.Fatalf("按 id 过滤: %v", perr)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if result.Count != 1 || result.Jobs[0].ID != target {
		t.Fatalf("按 id 过滤结果不对: %+v", result)
	}
	if _, perr := Call("backup.jobs", json.RawMessage(`{"limit":51}`), opts, nil); perr == nil ||
		perr.Code != pluginapi.E_INVALID {
		t.Fatalf("limit=51 期望 E_INVALID，得到 %v", perr)
	}
}
