//go:build unix

package backupop

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// fileOwner 从 FileInfo 提取 uid/gid；非 unix 平台返回 0/0（见 owner_other.go）。
func fileOwner(info fs.FileInfo) (int, int) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return int(st.Uid), int(st.Gid)
}

// applyOwner 尝试恢复 uid/gid；非 root 恢复他人属主会失败，权限不足时降级跳过，
// 其余错误（路径不存在、只读文件系统等）如实返回。
func applyOwner(path string, uid, gid int, link bool) error {
	if uid < 0 || gid < 0 {
		return nil
	}
	var err error
	if link {
		err = os.Lchown(path, uid, gid)
	} else {
		err = os.Chown(path, uid, gid)
	}
	if err != nil && errors.Is(err, fs.ErrPermission) {
		return nil
	}
	return err
}
