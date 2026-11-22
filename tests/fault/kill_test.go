package fault

import (
	"encoding/json"
	"testing"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
)

// TestKillFault 注入 kill -9（进程被信号终止），期望五个方法返回 E_UPSTREAM。
func TestKillFault(t *testing.T) {
	assertFault(t, backupop.FaultKill, "E_UPSTREAM", "信号 9")
}

// TestKillFaultContract 单独验证 kill 故障的错误体结构完整（真实调用，非空壳）。
func TestKillFaultContract(t *testing.T) {
	workspace := t.TempDir()
	opts := backupop.Options{Dir: workspace, AllowRoots: []string{workspace}}
	_, perr := backupop.Call("backup.list", json.RawMessage(`{}`), opts, faultExecutor{kind: backupop.FaultKill})
	verifyErrorContract(t, perr)
}
