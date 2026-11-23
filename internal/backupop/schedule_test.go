package backupop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// scheduleResult 是 backup.schedule.set 结果在测试里的显式解码形状。
type scheduleResult struct {
	Created         bool   `json:"created"`
	TaskID          string `json:"task_id"`
	Saved           bool   `json:"saved"`
	NextRunAt       string `json:"next_run_at"`
	RecommendedTask struct {
		Name     string          `json:"name"`
		Plugin   string          `json:"plugin"`
		Method   string          `json:"method"`
		Params   json.RawMessage `json:"params"`
		Schedule string          `json:"schedule"`
		TimeoutS int             `json:"timeout_s"`
	} `json:"recommended_task"`
	Hint string `json:"hint"`
}

// 造一个可备份的源目录。
func prepareSource(t *testing.T, workspace string) string {
	t.Helper()
	source := filepath.Join(workspace, "src")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("建源目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("写源文件: %v", err)
	}
	return source
}

// TestScheduleSetContract 校验任务描述与 ssl.renew.set 同构：created=false、task_id 空、
// plugin/method/schedule/timeout_s/params 齐备，且 next_run_at 在未来。
func TestScheduleSetContract(t *testing.T) {
	workspace := t.TempDir()
	source := prepareSource(t, workspace)
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	params := `{"name":"nightly","schedule":"0 3 * * *","timeout_s":1800,"paths":[` + quote(source) + `]}`
	raw, perr := Call("backup.schedule.set", json.RawMessage(params), opts, nil)
	if perr != nil {
		t.Fatalf("schedule.set 失败: %v", perr)
	}
	var got scheduleResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析结果: %v (%s)", err, raw)
	}
	if got.Created || got.TaskID != "" {
		t.Fatalf("插件不能创建内核任务，created/task_id 必须为 false/空: %+v", got)
	}
	if !got.Saved || got.Hint == "" {
		t.Fatalf("本地保存与解释文案缺失: %+v", got)
	}
	if got.RecommendedTask.Plugin != "backup" || got.RecommendedTask.Method != "backup.create" {
		t.Fatalf("任务目标不对: %+v", got.RecommendedTask)
	}
	if got.RecommendedTask.Name != "backup-nightly" || got.RecommendedTask.Schedule != "0 3 * * *" ||
		got.RecommendedTask.TimeoutS != 1800 {
		t.Fatalf("任务字段不对: %+v", got.RecommendedTask)
	}
	next, err := time.Parse(time.RFC3339, got.NextRunAt)
	if err != nil || !next.After(time.Now()) {
		t.Fatalf("next_run_at=%q 不是未来时刻: %v", got.NextRunAt, err)
	}
	var taskParams createParams
	if err := json.Unmarshal(got.RecommendedTask.Params, &taskParams); err != nil {
		t.Fatalf("任务参数不是 create 形状: %v (%s)", err, got.RecommendedTask.Params)
	}
	if taskParams.Name != "nightly" || len(taskParams.Paths) != 1 || !hasDimension(taskParams.Dimensions, DimensionSite) {
		t.Fatalf("任务参数不对: %+v", taskParams)
	}
	if _, err := os.Stat(filepath.Join(opts.Dir, scheduleFileName)); err != nil {
		t.Fatalf("任务描述未落盘: %v", err)
	}
}

// TestScheduleGetRoundTrip 校验 schedule.get 只读本地描述，未知 name 返回 E_NOTFOUND。
func TestScheduleGetRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	source := prepareSource(t, workspace)
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	for _, name := range []string{"nightly", "weekly"} {
		params := `{"name":"` + name + `","schedule":"0 3 * * *","paths":[` + quote(source) + `]}`
		if _, perr := Call("backup.schedule.set", json.RawMessage(params), opts, nil); perr != nil {
			t.Fatalf("schedule.set(%s): %v", name, perr)
		}
	}
	raw, perr := Call("backup.schedule.get", json.RawMessage(`{}`), opts, nil)
	if perr != nil {
		t.Fatalf("schedule.get: %v", perr)
	}
	var all ScheduleGetResult
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if all.Count != 2 || len(all.Records) != 2 {
		t.Fatalf("应有两条描述: %+v", all)
	}
	raw, perr = Call("backup.schedule.get", json.RawMessage(`{"name":"weekly"}`), opts, nil)
	if perr != nil {
		t.Fatalf("按 name 查询: %v", perr)
	}
	var one ScheduleGetResult
	if err := json.Unmarshal(raw, &one); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if one.Count != 1 || one.Records[0].Name != "weekly" || one.Records[0].Task.Name != "backup-weekly" {
		t.Fatalf("按 name 查询结果不对: %+v", one)
	}
	if _, perr := Call("backup.schedule.get", json.RawMessage(`{"name":"absent"}`), opts, nil); perr == nil ||
		perr.Code != pluginapi.E_NOTFOUND {
		t.Fatalf("未知 name 期望 E_NOTFOUND，得到 %v", perr)
	}
}

// TestScheduleSetRejects 校验参数与 cron 表达式边界。
func TestScheduleSetRejects(t *testing.T) {
	workspace := t.TempDir()
	source := prepareSource(t, workspace)
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{workspace}}
	bad := []string{
		`{"name":"snap","schedule":"0 3 * * *"}`,
		`{"name":"snap","schedule":"bad","paths":[` + quote(source) + `]}`,
		`{"name":"snap","schedule":"61 * * * *","paths":[` + quote(source) + `]}`,
		`{"name":"snap","schedule":"0 3 * *","paths":[` + quote(source) + `]}`,
		`{"name":"snap","schedule":"0 3 * * *","timeout_s":4000,"paths":[` + quote(source) + `]}`,
		`{"name":"snap","schedule":"0 3 * * *","dimensions":["s3"],"paths":[` + quote(source) + `]}`,
		`{"name":"snap","schedule":"0 3 * * *","paths":["relative"]}`,
		`{"name":"snap","schedule":"0 3 * * *","dimensions":["db"]}`,
		`{"name":"snap","schedule":"0 3 * * *","dimensions":["db"],"database":"shop"}`,
		`{"name":"../evil","schedule":"0 3 * * *","paths":[` + quote(source) + `]}`,
	}
	for _, params := range bad {
		if _, perr := Call("backup.schedule.set", json.RawMessage(params), opts, nil); perr == nil ||
			perr.Code != pluginapi.E_INVALID {
			t.Fatalf("params=%s 期望 E_INVALID，得到 %v", params, perr)
		}
	}
}

// TestScheduleTaskNameTruncates 校验超长备份名生成 ≤64 字符且确定的任务名。
func TestScheduleTaskNameTruncates(t *testing.T) {
	long := strings.Repeat("a", MaxNameLen)
	first := scheduleTaskName(long)
	second := scheduleTaskName(long)
	if len(first) > schedulerNameMaxLen || first != second {
		t.Fatalf("任务名=%q len=%d，期望 ≤%d 且确定", first, len(first), schedulerNameMaxLen)
	}
	if !strings.HasPrefix(first, scheduleTaskNamePrefix) {
		t.Fatalf("任务名缺少前缀: %q", first)
	}
	if scheduleTaskName("nightly") != "backup-nightly" {
		t.Fatalf("短名不应截断: %q", scheduleTaskName("nightly"))
	}
}

// TestCronNext 校验 5 段表达式的下次执行时间计算（本地时区）。
func TestCronNext(t *testing.T) {
	base := time.Date(2026, 11, 23, 10, 20, 30, 0, time.Local) // 周一
	cases := []struct {
		expr  string
		after time.Time
		want  string
	}{
		{"* * * * *", base, "2026-11-23T10:21:00"},
		{"0 3 * * *", base, "2026-11-24T03:00:00"},
		{"0 3 * * *", time.Date(2026, 11, 23, 2, 30, 0, 0, time.Local), "2026-11-23T03:00:00"},
		{"30 2 * * 1", base, "2026-11-30T02:30:00"},
		{"*/15 * * * *", time.Date(2026, 11, 23, 10, 16, 0, 0, time.Local), "2026-11-23T10:30:00"},
		{"0 0 1 * *", base, "2026-12-01T00:00:00"},
	}
	for _, c := range cases {
		expr, err := parseCron(c.expr)
		if err != nil {
			t.Fatalf("parseCron(%q): %v", c.expr, err)
		}
		got := expr.next(c.after)
		if got.Format("2006-01-02T15:04:05") != c.want {
			t.Fatalf("%q after %s = %s，期望 %s", c.expr, c.after.Format(time.RFC3339), got.Format(time.RFC3339), c.want)
		}
	}
}

// TestCronParseRejects 校验非法表达式逐段报错。
func TestCronParseRejects(t *testing.T) {
	bad := []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 0 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "5-1 * * * *", "a * * * *", "1,,2 * * * *"}
	for _, expr := range bad {
		if _, err := parseCron(expr); err == nil {
			t.Fatalf("parseCron(%q) 应失败", expr)
		}
	}
}
