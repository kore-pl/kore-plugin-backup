package backupop

import (
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestValidateTarget 路径白名单：合法相对路径放行，穿越/绝对/NUL/反斜杠拒绝。
func TestValidateTarget(t *testing.T) {
	good := []string{"data", "data/db", "a/b/c", "./data"}
	for _, raw := range good {
		got, perr := ValidateTarget(root, raw)
		if perr != nil || !strings.HasPrefix(got, root) {
			t.Fatalf("ValidateTarget(%q)=(%q,%v)，期望根内路径", raw, got, perr)
		}
	}
	bad := []string{"", "/etc", "../etc", "data/../../etc", "a\\b", "a\x00b"}
	for _, raw := range bad {
		if _, perr := ValidateTarget(root, raw); perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("ValidateTarget(%q) 错误=%v，期望 E_INVALID", raw, perr)
		}
	}
}

// TestValidateBackend 仅 local/s3 两种后端放行。
func TestValidateBackend(t *testing.T) {
	for _, raw := range []string{"local", "s3"} {
		if got, perr := ValidateBackend(raw); perr != nil || got != raw {
			t.Fatalf("ValidateBackend(%q)=(%q,%v)", raw, got, perr)
		}
	}
	for _, raw := range []string{"", "gcs", " LOCAL "} {
		if _, perr := ValidateBackend(raw); perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("ValidateBackend(%q) 错误=%v，期望 E_INVALID", raw, perr)
		}
	}
}

// TestValidateRetention retention_days 边界（1 与 3650 合法，0 与 3651 非法）。
func TestValidateRetention(t *testing.T) {
	for _, days := range []int{1, 7, 3650} {
		if perr := ValidateRetention(days); perr != nil {
			t.Fatalf("ValidateRetention(%d) 意外错误: %v", days, perr)
		}
	}
	for _, days := range []int{0, -1, 3651} {
		if perr := ValidateRetention(days); perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("ValidateRetention(%d) 错误=%v，期望 E_INVALID", days, perr)
		}
	}
}
