package backupop

import (
	"path"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// ValidateTarget 把备份目标路径限制在配置根目录（白名单）内，返回归一化后的绝对路径。
// 拒绝：空路径、绝对路径、反斜杠、NUL、".." 段、以及 Clean 后越出根目录。
// 只做字符串校验，不访问文件系统。
func ValidateTarget(root, raw string) (string, *pluginapi.Error) {
	if raw == "" {
		return "", invalid("备份目标不能为空")
	}
	if strings.ContainsRune(raw, 0) {
		return "", invalid("备份目标含 NUL 字节")
	}
	if strings.Contains(raw, "\\") {
		return "", invalid("备份目标含反斜杠")
	}
	if path.IsAbs(raw) {
		return "", invalid("备份目标必须是根目录内的相对路径")
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", invalid("备份目标含 .. 段，禁止穿越")
		}
	}
	rootClean := path.Clean(root)
	joined := path.Clean(path.Join(rootClean, raw))
	if joined != rootClean && !strings.HasPrefix(joined, rootClean+"/") {
		return "", invalid("备份目标越出配置根目录")
	}
	return joined, nil
}

// ValidateBackend 校验后端标识：仅允许 local 与 s3 两种（S3 兼容统一归 s3）。
func ValidateBackend(raw string) (string, *pluginapi.Error) {
	switch raw {
	case "local", "s3":
		return raw, nil
	case "":
		return "", invalid("后端不能为空")
	default:
		return "", invalid("不支持的后端: " + raw)
	}
}

// ValidateRetention 校验 retention_days 在 [1, 3650] 区间内。
func ValidateRetention(days int) *pluginapi.Error {
	if days < minRetention || days > maxRetention {
		return invalid("retention_days 必须在 1..3650 之间")
	}
	return nil
}
