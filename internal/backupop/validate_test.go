package backupop

import (
	"path/filepath"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestValidateName 备份 name 只允许安全字符集，路径分隔符与点开头被拒。
func TestValidateName(t *testing.T) {
	good := []string{"snap", "site-1", "db_backup_20261122", "a.b"}
	for _, raw := range good {
		if got, perr := ValidateName(raw); perr != nil || got != raw {
			t.Fatalf("ValidateName(%q)=(%q,%v)", raw, got, perr)
		}
	}
	bad := []string{"", ".", "..", "../evil", "a/b", `a\b`, "-lead", "中文名", string(make([]byte, MaxNameLen+1))}
	for _, raw := range bad {
		if _, perr := ValidateName(raw); perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("ValidateName(%q) 错误=%v，期望 E_INVALID", raw, perr)
		}
	}
}

// TestValidateID 备份 id 拒绝分隔符、点开头与超长值。
func TestValidateID(t *testing.T) {
	for _, raw := range []string{"snap-20260101T000000Z", "a.b-c_1"} {
		if got, perr := ValidateID(raw); perr != nil || got != raw {
			t.Fatalf("ValidateID(%q)=(%q,%v)", raw, got, perr)
		}
	}
	for _, raw := range []string{"", "../evil", "a/b", ".hidden", `a\b`, "中文"} {
		if _, perr := ValidateID(raw); perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("ValidateID(%q) 错误=%v，期望 E_INVALID", raw, perr)
		}
	}
}

// TestNormalizeCompression 缺省 gzip，仅接受 gzip / none（大小写不敏感）。
func TestNormalizeCompression(t *testing.T) {
	cases := map[string]string{"": "gzip", "gzip": "gzip", "GZIP": "gzip", "none": "none", "NONE": "none"}
	for raw, want := range cases {
		if got, perr := NormalizeCompression(raw); perr != nil || got != want {
			t.Fatalf("NormalizeCompression(%q)=(%q,%v)，期望 %q", raw, got, perr, want)
		}
	}
	for _, raw := range []string{"zip", "bzip2", "tar"} {
		if _, perr := NormalizeCompression(raw); perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("NormalizeCompression(%q) 错误=%v，期望 E_INVALID", raw, perr)
		}
	}
}

// TestValidateExclude 空模式与非法 glob 被拒。
func TestValidateExclude(t *testing.T) {
	if perr := ValidateExclude([]string{"*.log", "cache/*", "a[0-9]"}); perr != nil {
		t.Fatalf("合法 glob 被拒: %v", perr)
	}
	for _, patterns := range [][]string{{""}, {"a["}} {
		if perr := ValidateExclude(patterns); perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("ValidateExclude(%v) 错误=%v，期望 E_INVALID", patterns, perr)
		}
	}
}

// TestResolveTargetDir 目标目录必须在白名单内，且不存在路径可被安全解析。
func TestResolveTargetDir(t *testing.T) {
	workspace := t.TempDir()
	got, perr := ResolveTargetDir(filepath.Join(workspace, "a", "b"), []string{workspace})
	if perr != nil || got != filepath.Join(workspace, "a", "b") {
		t.Fatalf("ResolveTargetDir=(%q,%v)", got, perr)
	}
}

// TestNormalizeRestoreFilter 过滤项必须是归档内相对路径。
func TestNormalizeRestoreFilter(t *testing.T) {
	if got, perr := NormalizeRestoreFilter("src/sub/"); perr != nil || got != "src/sub" {
		t.Fatalf("NormalizeRestoreFilter=(%q,%v)", got, perr)
	}
	for _, raw := range []string{"", "/abs", "../evil", "a/../../b", `a\b`} {
		if _, perr := NormalizeRestoreFilter(raw); perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("NormalizeRestoreFilter(%q) 错误=%v，期望 E_INVALID", raw, perr)
		}
	}
}
