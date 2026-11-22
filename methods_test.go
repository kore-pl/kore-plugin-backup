package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestDispatchMethods 五个 backup 方法经 dispatch 返回结构化结果。
func TestDispatchMethods(t *testing.T) {
	workspace := t.TempDir()
	source := filepath.Join(workspace, "src")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("建源目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("写源文件: %v", err)
	}
	cfg := testConfig(t)
	cfg.backupDir = filepath.Join(workspace, "backups")
	cfg.allowRoots = []string{workspace}

	created := call(t, cfg, "backup.create",
		`{"name":"snap","paths":[`+jsonQuote(source)+`],"compression":"gzip"}`)
	var create struct {
		ID        string   `json:"id"`
		SHA256    string   `json:"sha256"`
		Entries   int      `json:"entries"`
		SizeBytes int64    `json:"size_bytes"`
		Duration  int64    `json:"duration_ms"`
		Skipped   []string `json:"skipped"`
		Warnings  []string `json:"warnings"`
	}
	if err := json.Unmarshal(created, &create); err != nil {
		t.Fatalf("解析 create: %v (%s)", err, created)
	}
	if create.ID == "" || len(create.SHA256) != 64 || create.Entries == 0 || create.SizeBytes == 0 {
		t.Fatalf("create 结果不完整: %+v", create)
	}
	if create.Skipped == nil || create.Warnings == nil {
		t.Fatalf("skipped/warnings 必须是数组: %s", created)
	}

	listed := call(t, cfg, "backup.list", `{}`)
	var list struct {
		Backups []struct {
			ID        string `json:"id"`
			Entries   int    `json:"entries"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"backups"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(listed, &list); err != nil {
		t.Fatalf("解析 list: %v (%s)", err, listed)
	}
	if list.Count != 1 || list.Backups[0].ID != create.ID || list.Backups[0].Entries != create.Entries {
		t.Fatalf("list 结果不对: %+v", list)
	}

	verified := call(t, cfg, "backup.verify", `{"id":`+jsonQuote(create.ID)+`}`)
	if !strings.Contains(string(verified), `"sha_match":true`) {
		t.Fatalf("verify 应 sha_match=true: %s", verified)
	}

	target := filepath.Join(workspace, "dst")
	restored := call(t, cfg, "backup.restore",
		`{"id":`+jsonQuote(create.ID)+`,"target_dir":`+jsonQuote(target)+`}`)
	if !strings.Contains(string(restored), `"restored_entries":2`) {
		t.Fatalf("restore 应恢复 2 个条目: %s", restored)
	}
	if data, err := os.ReadFile(filepath.Join(target, "src", "a.txt")); err != nil || string(data) != "hello\n" {
		t.Fatalf("恢复内容不对: %q err=%v", data, err)
	}

	deleted := call(t, cfg, "backup.delete", `{"id":`+jsonQuote(create.ID)+`}`)
	if !strings.Contains(string(deleted), `"deleted":true`) {
		t.Fatalf("delete 结果: %s", deleted)
	}
}

// TestDispatchReject 白名单外路径、/proc、非法 compression 与未知方法经 dispatch 返回协议错误。
func TestDispatchReject(t *testing.T) {
	workspace := t.TempDir()
	cfg := testConfig(t)
	cfg.backupDir = filepath.Join(workspace, "backups")
	cfg.allowRoots = []string{workspace}
	cases := []struct {
		method string
		params string
		code   string
	}{
		{"backup.create", `{"name":"snap","paths":["/etc"]}`, pluginapi.E_INVALID},
		{"backup.create", `{"name":"snap","paths":["/proc/self"]}`, pluginapi.E_INVALID},
		{"backup.create", `{"name":"snap","paths":[` + jsonQuote(workspace) + `],"compression":"bzip2"}`, pluginapi.E_INVALID},
		{"backup.restore", `{"id":"snap-20260101T000000Z","target_dir":"/proc/restore"}`, pluginapi.E_INVALID},
		{"backup.exec", `{}`, pluginapi.E_NOTFOUND},
	}
	for _, c := range cases {
		req := &pluginapi.Request{
			Type: pluginapi.TypeRequest, ID: "id", ReqID: "r",
			Method: c.method, Params: json.RawMessage(c.params),
		}
		resp, _ := dispatch(req, cfg, testLogger())
		if resp.Error == nil || resp.Error.Code != c.code {
			t.Fatalf("%s 错误=%v，期望 %q", c.method, resp.Error, c.code)
		}
		if resp.Result != nil {
			t.Fatalf("%s 出错时不应带结果", c.method)
		}
	}
}

// TestPingResult kore.ping 返回 ready 状态。
func TestPingResult(t *testing.T) {
	req := &pluginapi.Request{Type: pluginapi.TypeRequest, ID: "id", ReqID: "r", Method: "kore.ping"}
	resp, _ := dispatch(req, testConfig(t), testLogger())
	if !strings.Contains(string(resp.Result), `"status":"ready"`) {
		t.Fatalf("ping 结果=%s", resp.Result)
	}
}

// call 经 dispatch 调用一个方法，断言无协议错误并返回结果原文。
func call(t *testing.T, cfg config, method, params string) json.RawMessage {
	t.Helper()
	req := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id", ReqID: "r",
		Method: method, Params: json.RawMessage(params),
	}
	resp, shutdown := dispatch(req, cfg, testLogger())
	if shutdown {
		t.Fatalf("%s 不应触发 shutdown", method)
	}
	if resp.Error != nil {
		t.Fatalf("%s 返回错误: %s/%s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp.Result
}

// jsonQuote 把字符串编码为 JSON 字面量。
func jsonQuote(raw string) string {
	data, _ := json.Marshal(raw)
	return string(data)
}
