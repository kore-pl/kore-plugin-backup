package backupop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 作业状态机：pending（已受理）→ running（打包/导出中）→ succeeded | failed。
// 状态与计数落盘在 <备份目录>/.jobs/，是「可轮询」的真实来源，不是伪进度。
const (
	JobStatusPending   = "pending"
	JobStatusRunning   = "running"
	JobStatusSucceeded = "succeeded"
	JobStatusFailed    = "failed"

	// DefaultJobsLimit / MaxJobsLimit 是 backup.jobs 一次返回的记录条数。
	DefaultJobsLimit = 20
	MaxJobsLimit     = 50
	// maxJobRecords 是落盘作业记录的保留上限（按文件 mtime 淘汰最旧）。
	maxJobRecords = 200
	// jobsDirName 是作业记录子目录名；backup.list 只扫顶层文件，不会误认。
	jobsDirName = ".jobs"
	// jobProgressInterval 是 running 状态落盘的最小间隔，避免大量小文件时写放大。
	jobProgressInterval = time.Second
	// jobErrorMaxLen 是作业错误消息的截断上限（不落超长 stderr）。
	jobErrorMaxLen = 512
)

// JobStage 是真实分阶段记录（db_dump / pack），由执行面按实测耗时填写。
type JobStage struct {
	Name       string `json:"name"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DurationMS int64  `json:"duration_ms"`
	Entries    int    `json:"entries"`
	Bytes      int64  `json:"bytes"`
}

// JobRecord 是一次 create 的持久化作业记录。
//
// 状态字段是 pending / running / succeeded / failed；Entries/Bytes 是已写入的
// 真实计数（running 期间由进度回调按 ≥1s 间隔刷新）。
type JobRecord struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Dimensions []string `json:"dimensions"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Entries    int      `json:"entries"`
	// Bytes 是已写入的未压缩内容字节数（running 期间由进度回调刷新）。
	Bytes int64 `json:"bytes"`
	// ArchiveBytes 是成功后的归档包字节数。
	ArchiveBytes int64      `json:"archive_bytes,omitempty"`
	ArchiveID    string     `json:"archive_id,omitempty"`
	SHA256       string     `json:"sha256,omitempty"`
	Error        string     `json:"error,omitempty"`
	Stages       []JobStage `json:"stages"`
}

// jobsParams 是 backup.jobs 的显式入参形状。
type jobsParams struct {
	ID    string `json:"id"`
	Limit int    `json:"limit"`
}

// JobsResult 是 backup.jobs 的结构化结果。
type JobsResult struct {
	Jobs  []JobRecord `json:"jobs"`
	Count int         `json:"count"`
	Hint  string      `json:"hint"`
}

// StateStore 是可选的作业 / 定时描述持久化能力：真实执行面用本地文件实现，
// 故障注入执行面可注入 I/O 故障，从而让 backup.jobs / backup.schedule.* 也进故障矩阵。
type StateStore interface {
	SaveJob(dir string, record JobRecord) error
	ListJobs(dir, jobID string) ([]JobRecord, error)
	SaveSchedules(dir string, records []ScheduleRecord) error
	LoadSchedules(dir string) ([]ScheduleRecord, error)
}

// stateStoreFor 优先使用执行面自带的状态存储，否则回落到本地文件实现。
func stateStoreFor(exec Executor) StateStore {
	if store, ok := exec.(StateStore); ok {
		return store
	}
	return realState{}
}

// realState 是 StateStore 的真实文件实现。
type realState struct{}

// callJobs 处理 backup.jobs：读取持久化作业记录，按开始时间倒序返回。
func callJobs(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p jobsParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	limit := p.Limit
	if limit == 0 {
		limit = DefaultJobsLimit
	}
	if limit < 1 || limit > MaxJobsLimit {
		return nil, invalid(fmt.Sprintf("limit 必须在 1–%d 之间", MaxJobsLimit))
	}
	records, err := stateStoreFor(exec).ListJobs(opts.Dir, strings.TrimSpace(p.ID))
	if err != nil {
		return nil, classify(err)
	}
	if records == nil {
		records = []JobRecord{}
	}
	if len(records) > limit {
		records = records[:limit]
	}
	return encode(JobsResult{
		Jobs:  records,
		Count: len(records),
		Hint:  "作业状态落盘在备份目录的 .jobs/；create 的结果体也带 job 字段。插件内没有 ticker，不主动推送，按需调用 backup.jobs 轮询即可",
	})
}

// newJobID 生成确定可排序的作业 id：<name>-<UTC 纳秒>。name 已通过 ValidateName。
func newJobID(name string, now time.Time) string {
	return fmt.Sprintf("%s-%d", name, now.UTC().UnixNano())
}

// jobTracker 负责 create 期间的状态落盘；所有写入都是 best-effort，不因记账失败中断备份。
type jobTracker struct {
	store     StateStore
	dir       string
	record    JobRecord
	lastWrite time.Time
}

// newJobTracker 构造作业跟踪器并写入 pending 记录。
func newJobTracker(store StateStore, dir, name string, dimensions []string) *jobTracker {
	now := time.Now().UTC()
	record := JobRecord{
		ID:         newJobID(name, now),
		Name:       name,
		Status:     JobStatusPending,
		Dimensions: append([]string(nil), dimensions...),
		StartedAt:  now.Format(time.RFC3339),
		Stages:     []JobStage{},
	}
	tracker := &jobTracker{store: store, dir: dir, record: record}
	tracker.save()
	return tracker
}

// markRunning 在真正开始执行前把状态推进到 running。
func (t *jobTracker) markRunning() {
	t.record.Status = JobStatusRunning
	t.lastWrite = time.Now()
	t.save()
}

// progress 是执行面的真实进度回调：只更新计数与状态，按 1s 节流落盘。
func (t *jobTracker) progress(entries int, bytes int64) {
	t.record.Status = JobStatusRunning
	t.record.Entries = entries
	t.record.Bytes = bytes
	if time.Since(t.lastWrite) < jobProgressInterval {
		return
	}
	t.lastWrite = time.Now()
	t.save()
}

// finish 回填最终状态与结果，并在成功时记录归档 id / sha256 / 阶段。
func (t *jobTracker) finish(data CreateData, err error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		t.record.Status = JobStatusFailed
		t.record.Error = truncateJobError(err)
		t.record.FinishedAt = now
		t.save()
		return
	}
	t.record.Status = JobStatusSucceeded
	t.record.FinishedAt = now
	t.record.Entries = data.Entries
	t.record.Bytes = data.UncompressedBytes
	t.record.ArchiveBytes = data.SizeBytes
	t.record.ArchiveID = data.ID
	t.record.SHA256 = data.SHA256
	if len(data.Stages) > 0 {
		t.record.Stages = data.Stages
	}
	t.save()
}

// record 返回当前记录的副本，用于填充 create 结果体。
func (t *jobTracker) snapshot() JobRecord {
	record := t.record
	record.Stages = append([]JobStage{}, t.record.Stages...)
	return record
}

// save 原子写作业记录；失败只影响可观测性，不阻断备份。
func (t *jobTracker) save() {
	_ = t.store.SaveJob(t.dir, t.record)
}

// truncateJobError 归一错误消息：单行、截断，避免把超长 stderr 或换行写进记录。
func truncateJobError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > jobErrorMaxLen {
		message = message[:jobErrorMaxLen]
	}
	return message
}

// SaveJob 原子写单条作业记录，并按 mtime 淘汰超过上限的旧记录。
func (realState) SaveJob(dir string, record JobRecord) error {
	jobsDir := filepath.Join(dir, jobsDirName)
	if err := os.MkdirAll(jobsDir, 0o700); err != nil {
		return newOpError(ErrInternal, "创建作业目录失败: "+err.Error())
	}
	data, err := json.Marshal(record)
	if err != nil {
		return newOpError(ErrInternal, "序列化作业记录失败: "+err.Error())
	}
	if err := atomicWrite(filepath.Join(jobsDir, record.ID+".json"), append(data, '\n')); err != nil {
		return newOpError(ErrInternal, "写作业记录失败: "+err.Error())
	}
	pruneJobs(jobsDir)
	return nil
}

// pruneJobs 按 mtime 从旧到新淘汰，保留最近 maxJobRecords 条。
func pruneJobs(jobsDir string) {
	entries, err := os.ReadDir(jobsDir)
	if err != nil || len(entries) <= maxJobRecords {
		return
	}
	type item struct {
		name string
		mod  time.Time
	}
	items := make([]item, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{name: entry.Name(), mod: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	remove := len(items) - maxJobRecords
	for i := 0; i < remove; i++ {
		_ = os.Remove(filepath.Join(jobsDir, items[i].name))
	}
}

// ListJobs 读取作业记录：id 为空返回全部（最多 500 条），否则只返回该 id。
func (realState) ListJobs(dir, jobID string) ([]JobRecord, error) {
	jobsDir := filepath.Join(dir, jobsDirName)
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []JobRecord{}, nil
		}
		return nil, newOpError(ErrInternal, "读取作业目录失败: "+err.Error())
	}
	records := make([]JobRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jobsDir, entry.Name()))
		if err != nil {
			return nil, newOpError(ErrInternal, "读取作业记录失败: "+err.Error())
		}
		var record JobRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, newOpError(ErrInternal, "作业记录损坏: "+err.Error())
		}
		if record.Stages == nil {
			record.Stages = []JobStage{}
		}
		if jobID != "" && record.ID != jobID {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].StartedAt != records[j].StartedAt {
			return records[i].StartedAt > records[j].StartedAt
		}
		return records[i].ID > records[j].ID
	})
	if len(records) > 500 {
		records = records[:500]
	}
	return records, nil
}
