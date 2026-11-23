package backupop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 定时备份的口径与 kore-plugin-ssl 的 ssl.renew.set 一致：
// 插件不能直接调内核 API（G-19 / ADR-0019：调度归内核 scheduler），
// backup.schedule.set 只返回"待创建任务描述"，由前端 POST /api/v1/scheduler/tasks。
// 插件内**没有**任何 ticker / AfterFunc / 定时 goroutine。
const (
	// ScheduleDefaultCron 是推荐的 5 段表达式：每天 03:00。
	ScheduleDefaultCron = "0 3 * * *"
	// SchedulePlugin / ScheduleMethod 是任务要调的目标。
	SchedulePlugin = "backup"
	ScheduleMethod = "backup.create"
	// ScheduleDefaultTimeoutS 是单次备份的超时建议值（30 分钟）。
	ScheduleDefaultTimeoutS = 1800
	// ScheduleMinTimeoutS / ScheduleMaxTimeoutS 与内核 scheduler 的 timeout_s 上限对齐。
	ScheduleMinTimeoutS = 1
	ScheduleMaxTimeoutS = 3600
	// scheduleTaskNamePrefix 是任务名前缀，前端据此识别本插件创建的任务。
	scheduleTaskNamePrefix = "backup-"
	// schedulerNameMaxLen 与内核 scheduler 的 name ≤64 字符校验对齐。
	schedulerNameMaxLen = 64
	// scheduleHashLen 是超长 name 截断后附加的指纹长度（8 位十六进制）。
	scheduleHashLen = 8
	// scheduleFileName 是"最近一次设置的任务描述"落盘文件（0600）；插件不据此调度。
	scheduleFileName = ".schedules.json"
	// maxScheduleRecords 是本地保存的定时描述上限（按 name 覆盖，不无限增长）。
	maxScheduleRecords = 50
	// scheduleHint 是返回给前端的固定说明：插件没有创建内核任务的能力。
	scheduleHint = "插件不能直接创建内核任务：把 recommended_task 原样 POST 到 /api/v1/scheduler/tasks 才会真正生效"
	// scheduleQueryHint 说明 schedule.get 只读插件本地保存的描述，不代表内核任务状态。
	scheduleQueryHint = "schedule.get 只返回插件本地保存的任务描述；内核任务是否生效、下次执行时间以内核 GET /api/v1/scheduler/tasks 为准"
)

// RecommendedTask 是前端可直接 POST 到内核的任务定义（字段与 scheduler.CreateInput 对齐）。
type RecommendedTask struct {
	Name     string          `json:"name"`
	Plugin   string          `json:"plugin"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
	Schedule string          `json:"schedule"`
	TimeoutS int             `json:"timeout_s"`
}

// ScheduleRecord 是插件本地保存的一条任务描述。
type ScheduleRecord struct {
	Name      string          `json:"name"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	NextRunAt string          `json:"next_run_at"`
	Task      RecommendedTask `json:"recommended_task"`
	Hint      string          `json:"hint"`
}

// scheduleFile 是 .schedules.json 的落盘形状。
type scheduleFile struct {
	Records []ScheduleRecord `json:"records"`
}

// scheduleSetParams 是 backup.schedule.set 的显式入参形状，与 backup.create 的参数同构。
type scheduleSetParams struct {
	Name        string   `json:"name"`
	Schedule    string   `json:"schedule"`
	TimeoutS    int      `json:"timeout_s"`
	Dimensions  []string `json:"dimensions"`
	Paths       []string `json:"paths"`
	Exclude     []string `json:"exclude"`
	Compression string   `json:"compression"`
	Database    string   `json:"database"`
}

// ScheduleSetResult 是 backup.schedule.set 的结果；created/task_id 恒为 false/空，
// 如实表达"插件没有创建内核任务的能力"，而不是假装成功。
type ScheduleSetResult struct {
	Created         bool            `json:"created"`
	TaskID          string          `json:"task_id"`
	Saved           bool            `json:"saved"`
	NextRunAt       string          `json:"next_run_at"`
	RecommendedTask RecommendedTask `json:"recommended_task"`
	Hint            string          `json:"hint"`
}

// scheduleGetParams 是 backup.schedule.get 的入参：name 为空时返回全部描述。
type scheduleGetParams struct {
	Name string `json:"name"`
}

// ScheduleGetResult 是 backup.schedule.get 的结果。
type ScheduleGetResult struct {
	Records []ScheduleRecord `json:"records"`
	Count   int              `json:"count"`
	Hint    string           `json:"hint"`
}

// callScheduleSet 生成"待创建定时备份任务"的描述并保存到本地，不调内核 API、不起定时器。
func callScheduleSet(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p scheduleSetParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	name, perr := ValidateName(p.Name)
	if perr != nil {
		return nil, perr
	}
	dimensions, perr := NormalizeDimensions(p.Dimensions)
	if perr != nil {
		return nil, perr
	}
	schedule := strings.TrimSpace(p.Schedule)
	if schedule == "" {
		schedule = ScheduleDefaultCron
	}
	expr, err := parseCron(schedule)
	if err != nil {
		return nil, invalid("schedule 不是合法的 5 段表达式: " + err.Error())
	}
	timeout := p.TimeoutS
	if timeout == 0 {
		timeout = ScheduleDefaultTimeoutS
	}
	if timeout < ScheduleMinTimeoutS || timeout > ScheduleMaxTimeoutS {
		return nil, invalid(fmt.Sprintf("timeout_s 必须在 %d–%d 之间", ScheduleMinTimeoutS, ScheduleMaxTimeoutS))
	}
	createParams, perr := buildScheduledCreateParams(name, dimensions, p, opts)
	if perr != nil {
		return nil, perr
	}
	paramsJSON, err := json.Marshal(createParams)
	if err != nil {
		return nil, internalErr("序列化任务参数失败: " + err.Error())
	}
	now := time.Now()
	next := expr.next(now)
	if next.IsZero() {
		return nil, invalid("schedule 在可预见范围内没有下次执行时间")
	}
	nextAt := next.UTC().Format(time.RFC3339)
	task := RecommendedTask{
		Name:     scheduleTaskName(name),
		Plugin:   SchedulePlugin,
		Method:   ScheduleMethod,
		Params:   paramsJSON,
		Schedule: schedule,
		TimeoutS: timeout,
	}
	record := ScheduleRecord{
		Name:      name,
		CreatedAt: now.UTC().Format(time.RFC3339),
		UpdatedAt: now.UTC().Format(time.RFC3339),
		NextRunAt: nextAt,
		Task:      task,
		Hint:      scheduleHint,
	}
	store := stateStoreFor(exec)
	records, err := store.LoadSchedules(opts.Dir)
	if err != nil {
		return nil, classify(err)
	}
	records = upsertSchedule(records, record)
	if err := store.SaveSchedules(opts.Dir, records); err != nil {
		return nil, classify(err)
	}
	return encode(ScheduleSetResult{
		Created:         false,
		TaskID:          "",
		Saved:           true,
		NextRunAt:       nextAt,
		RecommendedTask: task,
		Hint:            scheduleHint,
	})
}

// buildScheduledCreateParams 用与 backup.create 完全相同的参数形状构造任务参数。
func buildScheduledCreateParams(name string, dimensions []string, p scheduleSetParams, opts Options) (createParams, *pluginapi.Error) {
	params := createParams{
		Name:        name,
		Dimensions:  dimensions,
		Exclude:     p.Exclude,
		Compression: p.Compression,
	}
	if err := ValidateExclude(p.Exclude); err != nil {
		return params, err
	}
	if _, err := NormalizeCompression(p.Compression); err != nil {
		return params, err
	}
	if hasDimension(dimensions, DimensionSite) {
		if len(p.Paths) == 0 {
			return params, invalid("site 维度的 paths 不能为空")
		}
		resolved := make([]string, 0, len(p.Paths))
		for _, rawPath := range p.Paths {
			abs, perr := ResolveBackupPath(rawPath, opts.AllowRoots)
			if perr != nil {
				return params, perr
			}
			resolved = append(resolved, abs)
		}
		params.Paths = resolved
	}
	if hasDimension(dimensions, DimensionDB) {
		if _, perr := validateDatabaseName(p.Database); perr != nil {
			return params, perr
		}
		if perr := ValidateMySQLConfig(opts.DB); perr != nil {
			return params, perr
		}
		params.DB = &createDBParams{Database: p.Database}
	}
	return params, nil
}

// callScheduleGet 返回插件本地保存的任务描述；不读内核状态（内核不向插件暴露任务）。
func callScheduleGet(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p scheduleGetParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	name := strings.TrimSpace(p.Name)
	records, err := stateStoreFor(exec).LoadSchedules(opts.Dir)
	if err != nil {
		return nil, classify(err)
	}
	result := ScheduleGetResult{Records: []ScheduleRecord{}, Hint: scheduleQueryHint}
	for _, record := range records {
		if name != "" && record.Name != name {
			continue
		}
		result.Records = append(result.Records, record)
	}
	if name != "" && len(result.Records) == 0 {
		return nil, notFound("未找到名为 " + name + " 的定时备份描述")
	}
	result.Count = len(result.Records)
	return encode(result)
}

// scheduleTaskName 生成符合内核约束（≤64 字符）的任务名；超长时保留前缀 + 名字前段 + 指纹。
func scheduleTaskName(name string) string {
	full := scheduleTaskNamePrefix + name
	if len(full) <= schedulerNameMaxLen {
		return full
	}
	digest := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(digest[:])[:scheduleHashLen]
	head := schedulerNameMaxLen - len(scheduleTaskNamePrefix) - len(suffix)
	return scheduleTaskNamePrefix + name[:head] + suffix
}

// upsertSchedule 按 name 覆盖更新，并按 UpdatedAt 倒序保留最近 maxScheduleRecords 条。
func upsertSchedule(records []ScheduleRecord, record ScheduleRecord) []ScheduleRecord {
	out := make([]ScheduleRecord, 0, len(records)+1)
	for _, existing := range records {
		if existing.Name == record.Name {
			record.CreatedAt = existing.CreatedAt
			continue
		}
		out = append(out, existing)
	}
	out = append(out, record)
	// 按 UpdatedAt 倒序，超限时淘汰最旧的描述；同刻按 name 保证确定性。
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > maxScheduleRecords {
		out = out[:maxScheduleRecords]
	}
	return out
}

// SaveSchedules 原子写任务描述文件（0600）。
func (realState) SaveSchedules(dir string, records []ScheduleRecord) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return newOpError(ErrInternal, "创建备份目录失败: "+err.Error())
	}
	if records == nil {
		records = []ScheduleRecord{}
	}
	data, err := json.Marshal(scheduleFile{Records: records})
	if err != nil {
		return newOpError(ErrInternal, "序列化定时描述失败: "+err.Error())
	}
	if err := atomicWrite(filepath.Join(dir, scheduleFileName), append(data, '\n')); err != nil {
		return newOpError(ErrInternal, "写入定时描述失败: "+err.Error())
	}
	return nil
}

// LoadSchedules 读取任务描述文件；文件不存在返回空集，损坏则报错（不静默丢配置）。
func (realState) LoadSchedules(dir string) ([]ScheduleRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, scheduleFileName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []ScheduleRecord{}, nil
		}
		return nil, newOpError(ErrInternal, "读取定时描述失败: "+err.Error())
	}
	var file scheduleFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, newOpError(ErrInternal, "定时描述损坏: "+err.Error())
	}
	if file.Records == nil {
		file.Records = []ScheduleRecord{}
	}
	return file.Records, nil
}

// cronField 是一段解析结果：允许值集合 + 是否为通配（决定"日/周"的与或语义）。
type cronField struct {
	values map[int]bool
	star   bool
}

// cronExpr 是一份解析后的 5 段表达式（分 时 日 月 周）。
type cronExpr struct {
	minute  cronField
	hour    cronField
	day     cronField
	month   cronField
	weekday cronField
}

// parseCron 解析 5 段表达式；支持 *、n、a-b、a,b、*/n、a-b/n、a/n，不支持名称与秒级字段。
func parseCron(expr string) (*cronExpr, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("表达式必须是 5 段（分 时 日 月 周），实际 %d 段", len(parts))
	}
	parsed := make([]cronField, 5)
	bounds := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	names := []string{"分钟", "小时", "日", "月", "周"}
	for i, part := range parts {
		field, err := parseCronField(part, bounds[i][0], bounds[i][1])
		if err != nil {
			return nil, fmt.Errorf("%s段 %q: %w", names[i], part, err)
		}
		parsed[i] = field
	}
	// 7 与 0 等价（周日），统一折叠成 0。
	if parsed[4].values[7] {
		delete(parsed[4].values, 7)
		parsed[4].values[0] = true
	}
	return &cronExpr{
		minute: parsed[0], hour: parsed[1], day: parsed[2], month: parsed[3], weekday: parsed[4],
	}, nil
}

// parseCronField 解析单段：逗号分隔的多个 token，每个 token 可带 /n 步长。
func parseCronField(raw string, low, high int) (cronField, error) {
	field := cronField{values: map[int]bool{}}
	for _, token := range strings.Split(raw, ",") {
		if token == "" {
			return field, errors.New("存在空 token")
		}
		base, step, hasStep, err := splitCronStep(token)
		if err != nil {
			return field, err
		}
		from, to, isStar, err := cronRange(base, low, high, hasStep)
		if err != nil {
			return field, err
		}
		if !hasStep {
			step = 1
		}
		if step < 1 {
			return field, errors.New("步长必须 ≥1")
		}
		for value := from; value <= to; value += step {
			field.values[value] = true
		}
		if isStar {
			field.star = true
		}
	}
	if len(field.values) == 0 {
		return field, errors.New("没有匹配值")
	}
	return field, nil
}

// splitCronStep 拆出 base 与 /n 步长。
func splitCronStep(token string) (base string, step int, hasStep bool, err error) {
	parts := strings.Split(token, "/")
	if len(parts) == 1 {
		return token, 0, false, nil
	}
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", 0, false, errors.New("步长写法非法（应为 a/n 或 a-b/n）")
	}
	parsed, convErr := strconv.Atoi(parts[1])
	if convErr != nil || parsed < 1 {
		return "", 0, false, errors.New("步长必须是正整数")
	}
	return parts[0], parsed, true, nil
}

// cronRange 把 base（* / n / a-b）转成闭区间；带步长时单值 n 表示 n..high。
func cronRange(base string, low, high int, hasStep bool) (from, to int, isStar bool, err error) {
	if base == "*" {
		return low, high, true, nil
	}
	if strings.Contains(base, "-") {
		parts := strings.Split(base, "-")
		if len(parts) != 2 {
			return 0, 0, false, errors.New("区间写法非法（应为 a-b）")
		}
		from, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, 0, false, errors.New("区间起点不是整数")
		}
		to, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, false, errors.New("区间终点不是整数")
		}
		if from < low || to > high || from > to {
			return 0, 0, false, fmt.Errorf("区间必须在 %d–%d 内且起点 ≤ 终点", low, high)
		}
		return from, to, false, nil
	}
	single, convErr := strconv.Atoi(base)
	if convErr != nil {
		return 0, 0, false, errors.New("不是整数")
	}
	if single < low || single > high {
		return 0, 0, false, fmt.Errorf("取值必须在 %d–%d 内", low, high)
	}
	if hasStep {
		return single, high, false, nil
	}
	return single, single, false, nil
}

// next 返回严格大于 after 的下一次执行时刻（按进程本地时区），8 年内找不到返回零值。
func (c cronExpr) next(after time.Time) time.Time {
	local := after.In(time.Local)
	start := local.Truncate(time.Minute).Add(time.Minute)
	midnight := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local)
	for day := 0; day <= 366*8+2; day++ {
		date := midnight.AddDate(0, 0, day)
		if !c.month.values[int(date.Month())] {
			continue
		}
		if !c.dayMatches(date) {
			continue
		}
		for hour := 0; hour < 24; hour++ {
			if !c.hour.values[hour] {
				continue
			}
			if day == 0 && hour < start.Hour() {
				continue
			}
			for minute := 0; minute < 60; minute++ {
				if !c.minute.values[minute] {
					continue
				}
				if day == 0 && hour == start.Hour() && minute < start.Minute() {
					continue
				}
				candidate := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, time.Local)
				if candidate.After(after) {
					return candidate
				}
			}
		}
	}
	return time.Time{}
}

// dayMatches 实现标准 cron 的日/周语义：两者都受限时取「或」，任一为 * 时取「与」。
func (c cronExpr) dayMatches(date time.Time) bool {
	domOK := c.day.values[date.Day()]
	dowOK := c.weekday.values[int(date.Weekday())]
	if c.day.star || c.weekday.star {
		return domOK && dowOK
	}
	return domOK || dowOK
}
