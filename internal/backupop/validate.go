package backupop

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// ValidateName 校验备份 name：只允许字母、数字、点、下划线、连字符，
// 首字符必须是字母或数字，拒绝空串、"." / ".." 与路径分隔符。
func ValidateName(raw string) (string, *pluginapi.Error) {
	if raw == "" {
		return "", invalid("备份 name 不能为空")
	}
	if len(raw) > MaxNameLen {
		return "", invalid("备份 name 超过 128 字符上限")
	}
	if raw == "." || raw == ".." {
		return "", invalid("备份 name 不能是 . 或 ..")
	}
	for i, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
			if i == 0 {
				return "", invalid("备份 name 必须以字母或数字开头")
			}
		default:
			return "", invalid("备份 name 只允许字母、数字、点、下划线与连字符")
		}
	}
	return raw, nil
}

// ValidateID 校验备份 id：仅允许 name 字符集 + 时间戳，禁止路径分隔符与穿越。
func ValidateID(raw string) (string, *pluginapi.Error) {
	if raw == "" {
		return "", invalid("备份 id 不能为空")
	}
	if len(raw) > MaxNameLen+32 {
		return "", invalid("备份 id 过长")
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return "", invalid("备份 id 含非法字符")
		}
	}
	if raw == "." || raw == ".." || strings.HasPrefix(raw, ".") {
		return "", invalid("备份 id 非法")
	}
	return raw, nil
}

// NormalizeCompression 归一 compression：缺省 gzip，仅接受 gzip / none。
func NormalizeCompression(raw string) (string, *pluginapi.Error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "gzip", nil
	case "gzip":
		return "gzip", nil
	case "none":
		return "none", nil
	default:
		return "", invalid("compression 仅支持 gzip 或 none")
	}
}

// ValidateExclude 校验 exclude glob：空模式与非法 glob 都拒绝。
func ValidateExclude(patterns []string) *pluginapi.Error {
	for _, pattern := range patterns {
		if pattern == "" {
			return invalid("exclude 列表不能包含空模式")
		}
		if strings.ContainsRune(pattern, 0) {
			return invalid("exclude 模式含 NUL 字节")
		}
		if _, err := filepath.Match(pattern, "probe"); err != nil {
			return invalid("exclude glob 非法: " + pattern)
		}
	}
	return nil
}

// ResolveBackupPath 校验备份源路径：必须是白名单内的绝对路径，且拒绝系统关键目录。
// 只解析祖先目录的符号链接，不跟随末尾条目本身——符号链接按链接记录，不读目标内容。
func ResolveBackupPath(raw string, roots []string) (string, *pluginapi.Error) {
	if raw == "" {
		return "", invalid("备份路径不能为空")
	}
	if strings.ContainsRune(raw, 0) {
		return "", invalid("备份路径含 NUL 字节")
	}
	if strings.Contains(raw, "\\") {
		return "", invalid("备份路径含反斜杠")
	}
	if !filepath.IsAbs(raw) {
		return "", invalid("备份路径必须是绝对路径")
	}
	clean := filepath.Clean(raw)
	if isCriticalDir(clean) {
		return "", invalid("拒绝备份系统关键目录（/proc、/sys、/dev、/run）")
	}
	if !underAnyRoot(clean, roots) {
		return "", invalid("备份路径不在允许的根目录白名单内：默认 /var/www、/etc/kore、/home")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", notFound("备份路径的父目录不存在")
		}
		return "", classify(err)
	}
	candidate := filepath.Join(parent, filepath.Base(clean))
	if isCriticalDir(candidate) {
		return "", invalid("拒绝备份系统关键目录（/proc、/sys、/dev、/run）")
	}
	if !underAnyRoot(candidate, roots) {
		return "", invalid("备份路径经符号链接解析后越出白名单")
	}
	if _, err := os.Lstat(candidate); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", notFound("备份路径不存在")
		}
		return "", classify(err)
	}
	return candidate, nil
}

// ResolveTargetDir 校验恢复目标目录：白名单内绝对路径、非关键目录，
// 并解析最近的已存在祖先，防止经符号链接写穿白名单。
func ResolveTargetDir(raw string, roots []string) (string, *pluginapi.Error) {
	if raw == "" {
		return "", invalid("target_dir 不能为空")
	}
	if strings.ContainsRune(raw, 0) {
		return "", invalid("target_dir 含 NUL 字节")
	}
	if strings.Contains(raw, "\\") {
		return "", invalid("target_dir 含反斜杠")
	}
	if !filepath.IsAbs(raw) {
		return "", invalid("target_dir 必须是绝对路径")
	}
	clean := filepath.Clean(raw)
	if isCriticalDir(clean) {
		return "", invalid("拒绝恢复到系统关键目录（/proc、/sys、/dev、/run）")
	}
	if !underAnyRoot(clean, roots) {
		return "", invalid("target_dir 不在允许的根目录白名单内：默认 /var/www、/etc/kore、/home")
	}
	cur, tail := clean, ""
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			target := filepath.Join(real, tail)
			if isCriticalDir(target) {
				return "", invalid("拒绝恢复到系统关键目录（/proc、/sys、/dev、/run）")
			}
			if !underAnyRoot(target, roots) {
				return "", invalid("target_dir 经符号链接解析后越出白名单")
			}
			return target, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", classify(err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", invalid("target_dir 路径无效")
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// NormalizeRestoreFilter 归一 restore 的 paths 过滤项：必须是归档内相对路径。
func NormalizeRestoreFilter(raw string) (string, *pluginapi.Error) {
	if raw == "" {
		return "", invalid("restore paths 不能包含空路径")
	}
	if strings.ContainsRune(raw, 0) {
		return "", invalid("restore paths 含 NUL 字节")
	}
	if strings.Contains(raw, "\\") {
		return "", invalid("restore paths 含反斜杠")
	}
	if path.IsAbs(raw) {
		return "", invalid("restore paths 必须是归档内相对路径")
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", invalid("restore paths 含 .. 段，禁止穿越")
		}
	}
	clean := path.Clean(raw)
	if clean == "." {
		return "", invalid("restore paths 不能是 .")
	}
	return clean, nil
}

// isCriticalDir 判定路径是否落在系统关键目录内。
func isCriticalDir(p string) bool {
	for _, dir := range criticalDirs {
		if withinPath(dir, p) {
			return true
		}
	}
	return false
}

// underAnyRoot 判定路径是否位于任一白名单根目录内。
func underAnyRoot(p string, roots []string) bool {
	for _, root := range roots {
		if withinPath(filepath.Clean(root), p) {
			return true
		}
	}
	return false
}

// withinPath 判定 p 等于 root 或位于 root 之下（用 Rel 避免 /home2 命中 /home）。
func withinPath(root, p string) bool {
	if root == "" || !filepath.IsAbs(root) {
		return false
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// archiveBase 是 create 的相对路径基准：路径的父目录。
// 例如 /home/u/src -> 父目录 /home/u，归档条目为 src/...；单文件 /home/u/a.txt -> a.txt。
func archiveBase(root string) string {
	return filepath.Dir(root)
}
