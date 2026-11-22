package fault

import (
	"testing"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
)

// TestDiskFullFault 注入磁盘满，期望两方法返回 E_INTERNAL。
func TestDiskFullFault(t *testing.T) {
	assertFault(t, backupop.FaultDisk, "E_INTERNAL", "磁盘满")
}
