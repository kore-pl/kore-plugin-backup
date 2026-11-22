package fault

import (
	"testing"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
)

// TestCgroupLimitFault 注入 cgroup 超限，期望两方法返回 E_UNSUPPORTED。
func TestCgroupLimitFault(t *testing.T) {
	assertFault(t, backupop.FaultCgroup, "E_UNSUPPORTED", "cgroup")
}
