package backupop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

const root = "/var/lib/kore/backup"

// TestBusinessVerb 方法名归一化：backup.<verb> 与裸 <verb> 都接受。
func TestBusinessVerb(t *testing.T) {
	cases := map[string]string{
		"backup.jobs": "jobs", "backup.create": "create",
		"jobs": "jobs", "create": "create",
	}
	for method, want := range cases {
		got, ok := businessVerb(method)
		if !ok || got != want {
			t.Fatalf("businessVerb(%q)=(%q,%v)，期望 (%q,true)", method, got, ok, want)
		}
	}
	if _, ok := businessVerb("backup.exec"); ok {
		t.Fatal("backup.exec 不应被接受")
	}
}

// TestCallResults 两方法正常路径返回结构化结果。
func TestCallResults(t *testing.T) {
	jobs, perr := Call("backup.jobs", json.RawMessage(`{}`), root, nil)
	if perr != nil || !strings.Contains(string(jobs), `"count":0`) {
		t.Fatalf("backup.jobs 结果=%s err=%v", jobs, perr)
	}
	create, perr := Call("backup.create", json.RawMessage(`{"target":"data/db","backend":"s3","retention_days":7}`), root, nil)
	if perr != nil || !strings.Contains(string(create), `"retention_days":7`) {
		t.Fatalf("backup.create 结果=%s err=%v", create, perr)
	}
}

// TestCallParamValidation 非法参数形状、路径越界、后端非法、retention 越界均返回 E_INVALID。
func TestCallParamValidation(t *testing.T) {
	bad := []struct {
		method string
		params string
	}{
		{"backup.jobs", `[]`},
		{"backup.create", `not-json`},
		{"backup.create", `{"target":"../../etc","backend":"local","retention_days":7}`},
		{"backup.create", `{"target":"/etc","backend":"local","retention_days":7}`},
		{"backup.create", `{"target":"data/db","backend":"gcs","retention_days":7}`},
		{"backup.create", `{"target":"data/db","backend":"local","retention_days":0}`},
		{"backup.create", `{"target":"data/db","backend":"local","retention_days":3651}`},
	}
	for _, c := range bad {
		_, perr := Call(c.method, json.RawMessage(c.params), root, nil)
		if perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("Call(%s,%s) 错误=%v，期望 E_INVALID", c.method, c.params, perr)
		}
	}
}

// TestCallUnknownMethod 未知方法返回 E_NOTFOUND。
func TestCallUnknownMethod(t *testing.T) {
	_, perr := Call("backup.exec", json.RawMessage(`{}`), root, nil)
	if perr == nil || perr.Code != pluginapi.E_NOTFOUND {
		t.Fatalf("未知方法错误=%v，期望 E_NOTFOUND", perr)
	}
}
