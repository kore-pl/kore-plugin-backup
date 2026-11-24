// 边界类别：异常输入。
// 覆盖两类真实入口：① backup.create 的非法备份路径（相对路径、白名单外、系统关键目录、
// 不存在、NUL、反斜杠、符号链接越界）；② 格式错误的参数与插件配置（非 JSON 对象、
// 非法 compression / dimensions / exclude / keep / target_dir，以及 config.json 坏 JSON）。
package boundary

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore-plugin-backup/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestAbnormalInputIllegalBackupPath 非法备份路径必须被白名单 / 关键目录 / 存在性检查拒绝，
// 且错误码明确（E_INVALID 表示规则拒绝，E_NOTFOUND 表示路径不存在）。
func TestAbnormalInputIllegalBackupPath(t *testing.T) {
	t.Parallel()
	workspace, opts := newWorkspace(t)
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		t.Fatalf("建备份目录: %v", err)
	}
	outside := t.TempDir()
	// 白名单内目录里的符号链接指向白名单外：词法看像在工作区里，解析后越界。
	if err := os.Symlink(outside, filepath.Join(workspace, "link")); err != nil {
		t.Fatalf("建越界符号链接: %v", err)
	}

	cases := []struct {
		name    string
		path    string
		code    string
		message string
	}{
		{"相对路径", "relative/dir", pluginapi.E_INVALID, "必须是绝对路径"},
		{"白名单外", outside, pluginapi.E_INVALID, "不在允许的根目录白名单内"},
		{"系统关键目录", "/proc", pluginapi.E_INVALID, "系统关键目录"},
		{"路径不存在", filepath.Join(workspace, "missing"), pluginapi.E_NOTFOUND, "备份路径不存在"},
		{"NUL 字节", workspace + "/nul\x00byte", pluginapi.E_INVALID, "NUL"},
		{"反斜杠", workspace + `\win`, pluginapi.E_INVALID, "反斜杠"},
		{"符号链接越界", filepath.Join(workspace, "link", "data"), pluginapi.E_INVALID, "越出白名单"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, perr := call(t, "backup.create", mustJSON(t, map[string]any{
				"name":  "illegal",
				"paths": []string{tc.path},
			}), opts)
			if perr == nil {
				t.Fatalf("非法路径 %q 未被拒绝", tc.path)
			}
			if perr.Code != tc.code {
				t.Fatalf("非法路径 %q 错误码=%s，期望 %s（message=%s）", tc.path, perr.Code, tc.code, perr.Message)
			}
			if !strings.Contains(perr.Message, tc.message) {
				t.Fatalf("非法路径 %q 错误信息=%q，期望包含 %q", tc.path, perr.Message, tc.message)
			}
		})
	}
}

// TestAbnormalInputMalformedParams 格式错误的参数（坏 JSON / 非法枚举 / 越界值）必须返回
// E_INVALID，未知方法返回 E_NOTFOUND；错误码是调用方能稳定分支判断的契约。
func TestAbnormalInputMalformedParams(t *testing.T) {
	t.Parallel()
	workspace, opts := newWorkspace(t)
	src := filepath.Join(workspace, "src")
	writeTree(t, src, 1, 32)

	target := filepath.Join(workspace, "restore-target")
	createParams := func(extra map[string]any) string {
		payload := map[string]any{"name": "malformed", "paths": []string{src}}
		for key, value := range extra {
			payload[key] = value
		}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("构造参数: %v", err)
		}
		return string(data)
	}

	cases := []struct {
		name    string
		method  string
		params  string
		code    string
		message string
	}{
		{"非 JSON 载荷", "backup.create", "not-json", pluginapi.E_INVALID, "params 必须是 JSON 对象"},
		{"JSON null", "backup.create", "null", pluginapi.E_INVALID, "params 必须是 JSON 对象"},
		{"JSON 数组", "backup.create", "[]", pluginapi.E_INVALID, "params 必须是 JSON 对象"},
		{"name 穿越", "backup.create", createParams(map[string]any{"name": "../evil"}), pluginapi.E_INVALID, "备份 name"},
		{"compression 非法", "backup.create", createParams(map[string]any{"compression": "bzip2"}), pluginapi.E_INVALID, "compression 仅支持"},
		{"exclude glob 非法", "backup.create", createParams(map[string]any{"exclude": []string{"["}}), pluginapi.E_INVALID, "exclude glob 非法"},
		{"dimensions 非法", "backup.create", createParams(map[string]any{"dimensions": []string{"site", "wat"}}), pluginapi.E_INVALID, "dimensions 只支持"},
		{"retention keep=0", "backup.retention", `{"keep":0}`, pluginapi.E_INVALID, "keep 必须在"},
		{"retention keep=101", "backup.retention", `{"keep":101}`, pluginapi.E_INVALID, "keep 必须在"},
		{"restore id 穿越", "backup.restore", `{"id":"../etc","target_dir":"` + target + `"}`, pluginapi.E_INVALID, "备份 id"},
		{"target_dir 相对路径", "backup.restore", `{"id":"snap","target_dir":"relative"}`, pluginapi.E_INVALID, "target_dir 必须是绝对路径"},
		{"target_dir 关键目录", "backup.restore", `{"id":"snap","target_dir":"/proc/evil"}`, pluginapi.E_INVALID, "系统关键目录"},
		{"jobs limit 越界", "backup.jobs", `{"limit":51}`, pluginapi.E_INVALID, "limit 必须在"},
		{"未知方法", "backup.nope", `{}`, pluginapi.E_NOTFOUND, "未知方法"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, perr := call(t, tc.method, json.RawMessage(tc.params), opts)
			if perr == nil {
				t.Fatalf("格式错误参数未被拒绝: %s %s", tc.method, tc.params)
			}
			if perr.Code != tc.code {
				t.Fatalf("%s 错误码=%s，期望 %s（message=%s）", tc.name, perr.Code, tc.code, perr.Message)
			}
			if !strings.Contains(perr.Message, tc.message) {
				t.Fatalf("%s 错误信息=%q，期望包含 %q", tc.name, perr.Message, tc.message)
			}
		})
	}
}

// TestAbnormalInputMalformedConfig 损坏的插件配置（非 JSON / 非对象 / 空文件）不能被当成
// 有效配置静默接受：Store.Read 必须报错并标记存在；写入非对象同样拒绝。
func TestAbnormalInputMalformedConfig(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	store := plugincfg.NewStore(stateDir)
	configPath := filepath.Join(stateDir, plugincfg.ConfigFileName)

	cases := []struct {
		name    string
		content string
		message string
	}{
		{"半截 JSON", `{"backup_dir": `, "不是合法 JSON"},
		{"JSON 数组", `[]`, "必须是 JSON 对象"},
		{"空文件", "  \n", "不是合法 JSON"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(configPath, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("写坏配置: %v", err)
			}
			raw, exists, err := store.Read()
			if err == nil {
				t.Fatalf("坏配置 %q 被接受，返回 %s", tc.content, raw)
			}
			if !exists {
				t.Fatalf("坏配置 %q 应报告存在", tc.content)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("坏配置 %q 错误=%q，期望包含 %q", tc.content, err.Error(), tc.message)
			}
		})
	}

	// 写入非对象载荷同样必须拒绝，避免把数组写进配置契约。
	if err := store.Write(json.RawMessage(`[]`)); err == nil || !strings.Contains(err.Error(), "必须是合法 JSON 对象") {
		t.Fatalf("非法配置写入错误=%v，期望拒绝非 JSON 对象", err)
	}
}

// TestAbnormalInputEmptySitePaths site 维度没有 paths 时必须 E_INVALID：
// 空路径不是「备份空目录」，而是调用方漏传，静默成功会产出误导性备份。
func TestAbnormalInputEmptySitePaths(t *testing.T) {
	t.Parallel()
	_, opts := newWorkspace(t)
	_, perr := call(t, "backup.create", mustJSON(t, map[string]any{"name": "empty"}), opts)
	if perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("空 paths 错误=%v，期望 E_INVALID", perr)
	}
}
