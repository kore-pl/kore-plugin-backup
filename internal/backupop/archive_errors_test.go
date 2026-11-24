package backupop

import (
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

// TestZeroEntriesMessageMentionsPermission 零条目且存在权限跳过时，错误消息必须点明
// 权限原因并给「怎么办」，且不泄露宿主绝对路径（HANDOFF B.6 #9）。
func TestZeroEntriesMessageMentionsPermission(t *testing.T) {
	noPerm := &archiveBuilder{permissionSkips: 2}
	msg := noPerm.zeroEntriesMessage()
	for _, want := range []string{"没有可备份的条目", "权限不足", "permission denied", "请检查属主", "root"} {
		if !strings.Contains(msg, want) {
			t.Errorf("权限跳过错误消息 %q 未含关键片段 %q", msg, want)
		}
	}
	if strings.Contains(msg, "/home") || strings.Contains(msg, "/etc") {
		t.Errorf("错误消息泄露宿主路径: %q", msg)
	}

	// 无权限跳过时保持原消息，避免误导。
	plain := (&archiveBuilder{}).zeroEntriesMessage()
	if !strings.Contains(plain, "被 exclude 全部排除") || strings.Contains(plain, "权限不足") {
		t.Errorf("无权限跳过时消息不应提权限: %q", plain)
	}
}

// TestErrnoTextStripsHostPath errno 文案必须剥掉 PathError 的宿主绝对路径。
func TestErrnoTextStripsHostPath(t *testing.T) {
	const hostPath = "/home/pt/private/data.bin"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"PathError 取 errno", &fs.PathError{Op: "read", Path: hostPath, Err: syscall.EACCES}, "permission denied"},
		{"普通错误原样", syscall.ENOSPC, "no space left on device"},
		{"nil 错误", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errnoText(tc.err)
			if !strings.Contains(got, tc.want) {
				t.Errorf("errnoText(%v)=%q，期望含 %q", tc.err, got, tc.want)
			}
			if strings.Contains(got, hostPath) {
				t.Errorf("errnoText 泄露宿主路径: %q", got)
			}
		})
	}
}
