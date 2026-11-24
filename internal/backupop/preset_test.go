package backupop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// newPresetWorkspace 建一个隔离工作区：备份目录在站点目录之外，
// 避免"整站备份把正在写的备份包也打进去"的递归，同时让自动发现退回白名单根本身。
func newPresetWorkspace(t *testing.T) (string, Options) {
	t.Helper()
	workspace := t.TempDir()
	site := filepath.Join(workspace, "site")
	if err := os.MkdirAll(filepath.Join(site, "html"), 0o755); err != nil {
		t.Fatalf("建站点目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(site, "html", "index.html"), []byte("<h1>hello</h1>"), 0o644); err != nil {
		t.Fatalf("写站点文件: %v", err)
	}
	opts := Options{Dir: filepath.Join(workspace, "backups"), AllowRoots: []string{site}}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		t.Fatalf("建备份目录: %v", err)
	}
	return site, opts
}

// createCall 走真实执行面（exec=nil）调用 backup.create 并解析结果。
func createCall(t *testing.T, params string, opts Options) (CreateData, *pluginapi.Error) {
	t.Helper()
	raw, perr := Call("backup.create", json.RawMessage(params), opts, nil)
	if perr != nil {
		return CreateData{}, perr
	}
	var data CreateData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("解析 create 结果: %v", err)
	}
	return data, nil
}

// TestCreateZeroParamsUsesDefaultPreset 是「小白一键」主用例：
// backup.create 完全零参数（{}）必须成功，按默认预设 site-full 备份白名单内的站点目录。
func TestCreateZeroParamsUsesDefaultPreset(t *testing.T) {
	site, opts := newPresetWorkspace(t)

	data, perr := createCall(t, `{}`, opts)
	if perr != nil {
		t.Fatalf("零参数 create 失败（小白一键必须能成功）: %s %s", perr.Code, perr.Message)
	}
	if data.Preset != PresetSiteFull {
		t.Fatalf("零参数 create preset=%q，期望默认预设 %q", data.Preset, PresetSiteFull)
	}
	if len(data.Dimensions) != 1 || data.Dimensions[0] != DimensionSite {
		t.Fatalf("零参数 create dimensions=%v，期望 [site]", data.Dimensions)
	}
	if data.Entries == 0 || data.SizeBytes == 0 || data.SHA256 == "" {
		t.Fatalf("零参数 create 产出为空: entries=%d size=%d sha=%q", data.Entries, data.SizeBytes, data.SHA256)
	}
	if _, err := os.Stat(data.Path); err != nil {
		t.Fatalf("零参数 create 归档不存在: %v", err)
	}
	if data.Job == nil || data.Job.Status != JobStatusSucceeded {
		t.Fatalf("零参数 create 作业未成功: %+v", data.Job)
	}
	// 内容必须真的来自默认预设发现的站点目录，而不是空包。
	verified, verr := Call("backup.verify", json.RawMessage(`{"id":"`+data.ID+`"}`), opts, nil)
	if verr != nil {
		t.Fatalf("verify 零参数产物失败: %s %s", verr.Code, verr.Message)
	}
	var verifiedData VerifyData
	if err := json.Unmarshal(verified, &verifiedData); err != nil {
		t.Fatalf("解析 verify 结果: %v", err)
	}
	if !verifiedData.OK || verifiedData.Entries == 0 {
		t.Fatalf("零参数产物校验失败: ok=%v entries=%d", verifiedData.OK, verifiedData.Entries)
	}
	t.Logf("零参数 create 实测: preset=%s path=%s entries=%d size=%d sha256=%s",
		data.Preset, filepath.Base(data.Path), data.Entries, data.SizeBytes, data.SHA256)
	_ = site
}

// TestCreatePresetExpansionIsCorrect 断言"选预设 → 展开的参数正确"：
// 三条预设的维度/路径/压缩/额外必填字段逐项核对，并由 backup.presets 对外暴露。
func TestCreatePresetExpansionIsCorrect(t *testing.T) {
	site, opts := newPresetWorkspace(t)
	// 站点候选被替换成本工作区目录，验证"候选命中优先于白名单根兜底"。
	originalSite, originalConfig := autoSiteCandidates, autoConfigCandidates
	autoSiteCandidates = []string{filepath.Join(site, "html")}
	autoConfigCandidates = []string{filepath.Join(site, "html", "index.html")}
	t.Cleanup(func() { autoSiteCandidates, autoConfigCandidates = originalSite, originalConfig })

	cases := []struct {
		preset     string
		dimensions []string
		wantPath   string
	}{
		{PresetSiteFull, []string{DimensionSite}, filepath.Join(site, "html")},
		{PresetConfig, []string{DimensionSite}, filepath.Join(site, "html", "index.html")},
		{PresetFull, []string{DimensionSite}, filepath.Join(site, "html")},
	}
	for _, tc := range cases {
		expansion, perr := expandBackupPreset(tc.preset, opts.AllowRoots)
		if perr != nil {
			t.Fatalf("展开预设 %s 失败: %s %s", tc.preset, perr.Code, perr.Message)
		}
		if len(expansion.Dimensions) != 1 || expansion.Dimensions[0] != tc.dimensions[0] {
			t.Fatalf("预设 %s dimensions=%v，期望 %v", tc.preset, expansion.Dimensions, tc.dimensions)
		}
		if len(expansion.Paths) != 1 || expansion.Paths[0] != tc.wantPath {
			t.Fatalf("预设 %s paths=%v，期望 [%s]", tc.preset, expansion.Paths, tc.wantPath)
		}
		if expansion.Compression != "gzip" || expansion.Name != tc.preset || len(expansion.Exclude) != 0 {
			t.Fatalf("预设 %s 展开异常: name=%s compression=%s exclude=%v",
				tc.preset, expansion.Name, expansion.Compression, expansion.Exclude)
		}
		// 走真实 create：preset 参数原样生效，且回显 preset。
		data, perr := createCall(t, `{"preset":"`+tc.preset+`"}`, opts)
		if perr != nil {
			t.Fatalf("按预设 %s 创建失败: %s %s", tc.preset, perr.Code, perr.Message)
		}
		if data.Preset != tc.preset || data.Entries == 0 {
			t.Fatalf("按预设 %s 创建结果异常: preset=%q entries=%d", tc.preset, data.Preset, data.Entries)
		}
	}

	// 未知预设必须明确拒绝，并列出可用项。
	_, perr := expandBackupPreset("nope", opts.AllowRoots)
	if perr == nil || perr.Code != pluginapi.E_INVALID || !strings.Contains(perr.Message, "未知备份预设") {
		t.Fatalf("未知预设错误=%v，期望 E_INVALID / 未知备份预设", perr)
	}

	// db 预设：必须要求用户选库（不能静默备份某个"默认库"）。
	_, perr = createCall(t, `{"preset":"db"}`, opts)
	if perr == nil || perr.Code != pluginapi.E_INVALID || !strings.Contains(perr.Message, "db.database") {
		t.Fatalf("db 预设缺少库名错误=%v，期望 E_INVALID / db.database", perr)
	}

	// backup.presets 对外暴露预设与保留策略，且默认预设可被前端发现。
	raw, perr := Call("backup.presets", nil, opts, nil)
	if perr != nil {
		t.Fatalf("backup.presets 失败: %s %s", perr.Code, perr.Message)
	}
	var presets PresetsResult
	if err := json.Unmarshal(raw, &presets); err != nil {
		t.Fatalf("解析 presets 结果: %v", err)
	}
	if presets.DefaultPreset != PresetSiteFull {
		t.Fatalf("default_preset=%q，期望 %q", presets.DefaultPreset, PresetSiteFull)
	}
	if len(presets.Presets) != len(backupPresets) || len(presets.RetentionPresets) != len(retentionPresets) {
		t.Fatalf("presets 条数=%d/%d，期望 %d/%d", len(presets.Presets), len(presets.RetentionPresets),
			len(backupPresets), len(retentionPresets))
	}
	for _, info := range presets.Presets {
		if info.ID == PresetDB {
			if !info.Expanded.RequiresDatabase || len(info.Fields) == 0 {
				t.Fatalf("db 预设未声明必填字段: %+v", info)
			}
			continue
		}
		if len(info.Expanded.Paths) == 0 {
			t.Fatalf("预设 %s 展开路径为空: %+v", info.ID, info.Expanded)
		}
	}
}

// TestRetentionPresetExpansion 断言保留策略预设：daily7/weekly4/monthly12 → keep 份数 + 推荐 cron。
func TestRetentionPresetExpansion(t *testing.T) {
	_, opts := newPresetWorkspace(t)
	for _, preset := range retentionPresets {
		raw, perr := Call("backup.retention", json.RawMessage(`{"preset":"`+preset.ID+`","dry_run":true}`), opts, nil)
		if perr != nil {
			t.Fatalf("保留预设 %s 失败: %s %s", preset.ID, perr.Code, perr.Message)
		}
		var data RetentionData
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("解析 retention 结果: %v", err)
		}
		if data.Preset != preset.ID || data.Schedule != preset.Schedule {
			t.Fatalf("保留预设 %s 回显异常: preset=%q schedule=%q", preset.ID, data.Preset, data.Schedule)
		}
	}
	if _, perr := Call("backup.retention", json.RawMessage(`{"preset":"nope"}`), opts, nil); perr == nil ||
		perr.Code != pluginapi.E_INVALID || !strings.Contains(perr.Message, "未知保留策略预设") {
		t.Fatalf("未知保留预设错误=%v，期望 E_INVALID / 未知保留策略预设", perr)
	}
}

// TestCreateCustomParamsStillWork 断言自定义兜底没坏：
// 显式 name/paths/compression 仍然生效，且不会被默认预设改写；显式 name 漏 paths 仍严格报错。
func TestCreateCustomParamsStillWork(t *testing.T) {
	site, opts := newPresetWorkspace(t)

	data, perr := createCall(t, mustJSONString(t, map[string]any{
		"name":        "manual-snap",
		"paths":       []string{site},
		"compression": "none",
	}), opts)
	if perr != nil {
		t.Fatalf("自定义 create 失败: %s %s", perr.Code, perr.Message)
	}
	if data.Preset != "" {
		t.Fatalf("自定义 create 不应回显预设，实际 preset=%q", data.Preset)
	}
	if filepath.Ext(data.Path) != ".tar" {
		t.Fatalf("compression=none 应产出 .tar，实际 %s", data.Path)
	}
	if data.Job == nil || data.Job.Name != "manual-snap" {
		t.Fatalf("自定义 name 未生效: %+v", data.Job)
	}

	// 显式给了 name 却没给 paths：保持 E_INVALID（预设不静默兜底），
	// 这条同时守住 07e9ac5 的 boundary 断言 TestAbnormalInputEmptySitePaths。
	_, perr = createCall(t, `{"name":"empty"}`, opts)
	if perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("显式 name 漏 paths 错误=%v，期望 E_INVALID（不得被默认预设兜底）", perr)
	}
}

// TestCreateZeroParamsWithoutDefaultPresetFails 是反例自证：
// 把默认预设去掉后，同一条零参数用例必须失败——证明零参数成功依赖的是预设，不是"空包也成功"。
func TestCreateZeroParamsWithoutDefaultPresetFails(t *testing.T) {
	_, opts := newPresetWorkspace(t)
	original := defaultPresetID
	defaultPresetID = ""
	t.Cleanup(func() { defaultPresetID = original })

	_, perr := createCall(t, `{}`, opts)
	if perr == nil {
		t.Fatal("默认预设被去掉后零参数 create 仍然成功——零参数用例没有真正依赖预设")
	}
	if perr.Code != pluginapi.E_INVALID || !strings.Contains(perr.Message, "name") {
		t.Fatalf("反例错误=%s/%s，期望 E_INVALID 且指向 name", perr.Code, perr.Message)
	}
	t.Logf("反例自证: 去掉默认预设后零参数 create 失败 = %s / %s", perr.Code, perr.Message)
}

// mustJSONString 把参数编码为字符串（测试内部构造用）。
func mustJSONString(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化参数: %v", err)
	}
	return string(data)
}
