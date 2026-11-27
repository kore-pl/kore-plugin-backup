package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// infoManifest 是 info.json 中本测试关心的字段（方法级能力声明）。
type infoManifest struct {
	ID                 string            `json:"id"`
	Version            string            `json:"version"`
	Capabilities       []string          `json:"capabilities"`
	MethodCapabilities map[string]string `json:"method_capabilities"`
}

// manifestHandshake 是随二进制内嵌的 manifest.json 握手字段子集。
type manifestHandshake struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// privilegedCapabilities 与内核唯一白名单一致（6 项，方法级只能取其一）。
var privilegedCapabilities = map[string]struct{}{
	"system:read": {}, "system:admin": {},
	"net:read": {}, "net:write": {},
	"proc:read": {}, "proc:write": {},
}

// readOnlyVerbs 是只读业务方法（含 config.get）：必须声明 system:read，
// 否则配置读取会回退插件级 system:admin 并进管理审计（§3.5 #12）。
var readOnlyVerbs = map[string]struct{}{
	"list": {}, "verify": {}, "stats": {}, "download": {}, "jobs": {},
	"schedule.get": {}, "config.get": {}, "presets": {}, "sources.detect": {},
}

// writeVerbs 是写业务方法（含 config.set）：必须声明 system:admin。
var writeVerbs = map[string]struct{}{
	"create": {}, "restore": {}, "delete": {}, "retention": {}, "upload": {},
	"schedule.set": {}, "config.set": {},
}

func loadInfoManifest(t *testing.T) infoManifest {
	t.Helper()
	raw, err := os.ReadFile("info.json")
	if err != nil {
		t.Fatalf("读取 info.json: %v", err)
	}
	var info infoManifest
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("解析 info.json: %v", err)
	}
	return info
}

func loadManifestHandshake(t *testing.T) manifestHandshake {
	t.Helper()
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("读取 manifest.json: %v", err)
	}
	var manifest manifestHandshake
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("解析 manifest.json: %v", err)
	}
	return manifest
}

// TestMethodCapabilitiesConsistency 是「声明 vs 实现分派」一致性门禁：
// ①只读/写分类逐条断言 ②声明 ⊆ 实现（反向断言，防止声明了未实现的方法）
// ③条数相等（防遗漏）④config.get/config.set 必含 ⑤能力值只能是 6 项特权能力。
func TestMethodCapabilitiesConsistency(t *testing.T) {
	info := loadInfoManifest(t)
	manifest := loadManifestHandshake(t)
	if len(info.MethodCapabilities) == 0 {
		t.Fatal("info.json 未声明 method_capabilities")
	}
	if info.Version != manifest.Version {
		t.Fatalf("info.json version=%q 与 manifest.json version=%q 不一致", info.Version, manifest.Version)
	}
	total := len(readOnlyVerbs) + len(writeVerbs)
	if len(info.MethodCapabilities) != total {
		t.Fatalf("method_capabilities 声明 %d 条，实现分派 %d 条（声明与实现必须一一对应）",
			len(info.MethodCapabilities), total)
	}
	for verb := range readOnlyVerbs {
		key := info.ID + "." + verb
		if got := info.MethodCapabilities[key]; got != "system:read" {
			t.Errorf("%s 应声明 system:read，实际 %q", key, got)
		}
	}
	for verb := range writeVerbs {
		key := info.ID + "." + verb
		if got := info.MethodCapabilities[key]; got != "system:admin" {
			t.Errorf("%s 应声明 system:admin，实际 %q", key, got)
		}
	}
	for key, capability := range info.MethodCapabilities {
		if !strings.HasPrefix(key, info.ID+".") {
			t.Errorf("键 %q 必须以插件自身 id 为域（%s.）", key, info.ID)
		}
		verb := strings.TrimPrefix(key, info.ID+".")
		_, isRead := readOnlyVerbs[verb]
		_, isWrite := writeVerbs[verb]
		if !isRead && !isWrite {
			t.Errorf("声明了未实现的方法 %q（分派里没有该 verb）", key)
		}
		if _, ok := privilegedCapabilities[capability]; !ok {
			t.Errorf("%s 的能力值 %q 不是 6 项特权能力之一", key, capability)
		}
	}
	for _, verb := range []string{"config.get", "config.set"} {
		if _, ok := info.MethodCapabilities[info.ID+"."+verb]; !ok {
			t.Errorf("缺少 %s.%s 声明（配置读取会回退 admin 审计）", info.ID, verb)
		}
	}
}
