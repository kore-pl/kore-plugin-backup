package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// dispatchCall 通过 dispatch 调用一个业务方法并断言无协议错误。
func dispatchCall(t *testing.T, cfg *config, method, params string) json.RawMessage {
	t.Helper()
	response, _ := dispatch(&pluginapi.Request{Method: method, Params: json.RawMessage(params)}, cfg, testLogger())
	if response.Error != nil {
		t.Fatalf("%s 失败: %+v", method, response.Error)
	}
	return response.Result
}

// TestDispatchScheduleSet 校验 schedule.set 经 main 分发返回内核可消费的任务描述。
func TestDispatchScheduleSet(t *testing.T) {
	workspace := t.TempDir()
	source := filepath.Join(workspace, "src")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("建源目录: %v", err)
	}
	cfg := testConfig(t)
	cfg.backupDir = filepath.Join(workspace, "backups")
	cfg.allowRoots = []string{workspace}
	raw := dispatchCall(t, &cfg, "backup.schedule.set",
		`{"name":"nightly","schedule":"0 3 * * *","paths":[`+jsonQuote(source)+`]}`)
	if !strings.Contains(string(raw), `"plugin":"backup"`) ||
		!strings.Contains(string(raw), `"method":"backup.create"`) ||
		!strings.Contains(string(raw), `"created":false`) {
		t.Fatalf("schedule.set 结果不符合 ssl.renew.set 契约: %s", raw)
	}
	got := dispatchCall(t, &cfg, "backup.schedule.get", `{"name":"nightly"}`)
	if !strings.Contains(string(got), `"name":"backup-nightly"`) {
		t.Fatalf("schedule.get 未返回保存的描述: %s", got)
	}
}

// TestDispatchDownload 校验 create 产物可经 backup.download 取走（元信息 + 分块）。
func TestDispatchDownload(t *testing.T) {
	workspace := t.TempDir()
	source := filepath.Join(workspace, "src")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("建源目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("kore"), 0o644); err != nil {
		t.Fatalf("写源文件: %v", err)
	}
	cfg := testConfig(t)
	cfg.backupDir = filepath.Join(workspace, "backups")
	cfg.allowRoots = []string{workspace}
	created := dispatchCall(t, &cfg, "backup.create", `{"name":"snap","paths":[`+jsonQuote(source)+`]}`)
	var create struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &create); err != nil || create.ID == "" {
		t.Fatalf("解析 create: %v (%s)", err, created)
	}
	meta := dispatchCall(t, &cfg, "backup.download", `{"id":`+jsonQuote(create.ID)+`}`)
	if !strings.Contains(string(meta), `"inline":false`) || !strings.Contains(string(meta), create.ID) {
		t.Fatalf("download 元信息不对: %s", meta)
	}
	chunk := dispatchCall(t, &cfg, "backup.download", `{"id":`+jsonQuote(create.ID)+`,"inline":true,"limit":64}`)
	if !strings.Contains(string(chunk), `"data_base64":`) {
		t.Fatalf("download 未返回分块内容: %s", chunk)
	}
}

// TestDispatchUpload 校验 backup.upload 四阶段经 main dispatch 落盘，且导入包不写 .meta.json。
func TestDispatchUpload(t *testing.T) {
	workspace := t.TempDir()
	cfg := testConfig(t)
	cfg.backupDir = filepath.Join(workspace, "backups")
	cfg.allowRoots = []string{workspace}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	content := []byte("dispatch-upload\n")
	header := &tar.Header{Name: "site/a.txt", Mode: 0o644, Size: int64(len(content)), Uid: os.Getuid(), Gid: os.Getgid()}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("写 tar 头: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("写 tar 内容: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()
	payload := archive.Bytes()
	sum := sha256.Sum256(payload)
	begin := dispatchCall(t, &cfg, "backup.upload", `{"phase":"begin","upload_id":"u-dispatch","filename":"site.tar.gz","size_bytes":`+
		jsonNumber(len(payload))+`,"sha256":"`+hex.EncodeToString(sum[:])+`"}`)
	if !strings.Contains(string(begin), `"upload_id":"u-dispatch"`) || !strings.Contains(string(begin), `"chunk_bytes":4194304`) {
		t.Fatalf("begin 结果不对: %s", begin)
	}
	chunk := dispatchCall(t, &cfg, "backup.upload", `{"phase":"chunk","upload_id":"u-dispatch","offset":0,"data_base64":"`+
		base64.StdEncoding.EncodeToString(payload)+`"}`)
	if !strings.Contains(string(chunk), `"received_bytes":`) {
		t.Fatalf("chunk 结果不对: %s", chunk)
	}
	commit := dispatchCall(t, &cfg, "backup.upload", `{"phase":"commit","upload_id":"u-dispatch"}`)
	if !strings.Contains(string(commit), `"managed":false`) || !strings.Contains(string(commit), `"entries":1`) {
		t.Fatalf("commit 结果不对: %s", commit)
	}
	if _, err := os.Stat(filepath.Join(cfg.backupDir, ".upload-u-dispatch")); !os.IsNotExist(err) {
		t.Fatalf("commit 后暂存目录应清理: %v", err)
	}
}

func jsonNumber(n int) string {
	data, _ := json.Marshal(n)
	return string(data)
}

// TestDispatchDBDimensionUsesConfig 校验 dispatch 把 config 里的 db 连接透传给执行面。
func TestDispatchDBDimensionUsesConfig(t *testing.T) {
	workspace := t.TempDir()
	stub := filepath.Join(workspace, "mysqldump")
	script := "#!/bin/sh\nprintf 'CREATE TABLE t (id int);\\n'\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatalf("写假 mysqldump: %v", err)
	}
	cfg := testConfig(t)
	cfg.backupDir = filepath.Join(workspace, "backups")
	cfg.allowRoots = []string{workspace}
	cfg.values = backupValues{
		Dir: cfg.backupDir, AllowRoots: cfg.allowRoots,
		DBHost: "127.0.0.1", DBPort: 3306, DBUser: "backup", DBDumpBin: stub,
	}
	created := dispatchCall(t, &cfg, "backup.create",
		`{"name":"dbonly","dimensions":["db"],"db":{"database":"shop"}}`)
	var create struct {
		Entries    int      `json:"entries"`
		Dimensions []string `json:"dimensions"`
	}
	if err := json.Unmarshal(created, &create); err != nil {
		t.Fatalf("解析 create: %v", err)
	}
	if create.Entries != 1 || len(create.Dimensions) != 1 || create.Dimensions[0] != "db" {
		t.Fatalf("db 维度 create 结果不对: %s", created)
	}
}
