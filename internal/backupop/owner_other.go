//go:build !unix

package backupop

import "io/fs"

// fileOwner 在非 unix 平台不提供 uid/gid（Tier B 降级），返回 0/0。
func fileOwner(fs.FileInfo) (int, int) { return 0, 0 }

// applyOwner 在非 unix 平台不做属主恢复（Tier B 降级）。
func applyOwner(string, int, int, bool) error { return nil }
