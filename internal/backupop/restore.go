package backupop

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// restoreParams 是 backup.restore 的显式入参形状。
type restoreParams struct {
	ID        string   `json:"id"`
	TargetDir string   `json:"target_dir"`
	Paths     []string `json:"paths"`
}

// callRestore 处理 backup.restore：校验 id、target_dir 白名单与 paths 过滤项，
// 再交给执行面「临时目录完整解包 → rename」的全有或全无恢复。
func callRestore(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p restoreParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	id, perr := ValidateID(p.ID)
	if perr != nil {
		return nil, perr
	}
	target, perr := ResolveTargetDir(p.TargetDir, opts.AllowRoots)
	if perr != nil {
		return nil, perr
	}
	filters := make([]string, 0, len(p.Paths))
	for _, rawFilter := range p.Paths {
		filter, perr := NormalizeRestoreFilter(rawFilter)
		if perr != nil {
			return nil, perr
		}
		filters = append(filters, filter)
	}
	started := time.Now()
	data, err := exec.Restore(RestoreSpec{Dir: opts.Dir, ID: id, TargetDir: target, Paths: filters})
	if err != nil {
		return nil, classify(err)
	}
	data.DurationMS = elapsedMS(started)
	if data.Failed == nil {
		data.Failed = []string{}
	}
	return encode(data)
}

// Restore 全有或全无地恢复：先解到目标同文件系统的临时目录，确认全部条目可解后
// rename 到 target_dir；任何条目失败都删除临时目录，目标目录不留半包。
func (realFS) Restore(spec RestoreSpec) (RestoreData, error) {
	archivePath, compression, err := resolveArchive(spec.Dir, spec.ID)
	if err != nil {
		return RestoreData{}, err
	}
	if err := checkRestoreTarget(spec.TargetDir); err != nil {
		return RestoreData{}, err
	}
	parent := filepath.Dir(spec.TargetDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return RestoreData{}, newOpError(ErrInternal, "创建目标父目录失败: "+err.Error())
	}
	staging, err := os.MkdirTemp(parent, ".kore-restore-*")
	if err != nil {
		return RestoreData{}, newOpError(ErrInternal, "创建恢复临时目录失败: "+err.Error())
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()

	file, err := os.Open(archivePath)
	if err != nil {
		return RestoreData{}, newOpError(ErrInternal, "打开备份包失败: "+err.Error())
	}
	defer func() { _ = file.Close() }()
	var reader io.Reader = file
	if compression == "gzip" {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return RestoreData{}, newOpError(ErrInvalid, "备份包不是合法 gzip")
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	restored, failed, err := extractArchive(tar.NewReader(reader), staging, spec.Paths)
	if err != nil {
		return RestoreData{}, err
	}
	if restored == 0 {
		if len(spec.Paths) > 0 {
			return RestoreData{}, newOpError(ErrNotFound, "指定路径在备份中均不存在")
		}
		return RestoreData{}, newOpError(ErrInvalid, "备份包为空，无可恢复条目")
	}

	// 目标若已存在，只允许空目录；移除后 rename 保证原子替换。
	if targetExists(spec.TargetDir) {
		if err := os.Remove(spec.TargetDir); err != nil {
			return RestoreData{}, newOpError(ErrConflict, "目标目录非空或不可移除")
		}
	}
	if err := os.Rename(staging, spec.TargetDir); err != nil {
		return RestoreData{}, newOpError(ErrInternal, "提交恢复目录失败: "+err.Error())
	}
	committed = true
	return RestoreData{
		ID:              spec.ID,
		RestoredEntries: restored,
		TargetDir:       spec.TargetDir,
		Failed:          failed,
	}, nil
}

// checkRestoreTarget 校验目标：不存在，或存在且为空目录；其余状态返回 E_CONFLICT。
func checkRestoreTarget(target string) error {
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return newOpError(ErrInternal, "读取目标目录状态失败: "+err.Error())
	}
	if !info.IsDir() {
		return newOpError(ErrConflict, "target_dir 已存在且不是目录")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return newOpError(ErrInternal, "读取目标目录失败: "+err.Error())
	}
	if len(entries) > 0 {
		return newOpError(ErrConflict, "target_dir 已存在且非空，拒绝半恢复覆盖")
	}
	return nil
}

// targetExists 判定目标是否仍存在（前一步可能并发删除）。
func targetExists(target string) bool {
	_, err := os.Lstat(target)
	return err == nil
}

// dirMeta 记录待恢复的目录元信息，待全部条目解完后再应用，避免子项写入失败。
type dirMeta struct {
	path    string
	mode    fs.FileMode
	modTime time.Time
	uid     int
	gid     int
}

// extractArchive 把 tar 条目解到 staging；先校验头再落盘，命中过滤项才写入。
func extractArchive(reader *tar.Reader, staging string, filters []string) (int, []string, error) {
	matched := make([]bool, len(filters))
	restored := 0
	dirs := make([]dirMeta, 0)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, nil, newOpError(ErrInvalid, "备份包损坏，无法恢复")
		}
		name, perr := validateRestoreHeader(header)
		if perr != nil {
			return 0, nil, errFromProtocol(perr)
		}
		if len(filters) > 0 {
			hit := false
			for index, filter := range filters {
				if matchEntry(name, filter) {
					matched[index] = true
					hit = true
				}
			}
			if !hit {
				continue
			}
		}
		dest := filepath.Join(staging, filepath.FromSlash(name))
		if !withinPath(staging, dest) {
			return 0, nil, newOpError(ErrInvalid, "归档条目越出恢复目录")
		}
		if err := extractEntry(reader, staging, dest, header, &dirs); err != nil {
			return 0, nil, err
		}
		restored++
	}

	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i].path, string(filepath.Separator)) >
			strings.Count(dirs[j].path, string(filepath.Separator))
	})
	for _, dir := range dirs {
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			return 0, nil, newOpError(ErrInternal, "恢复目录权限失败: "+err.Error())
		}
		if err := os.Chtimes(dir.path, dir.modTime, dir.modTime); err != nil {
			return 0, nil, newOpError(ErrInternal, "恢复目录时间失败: "+err.Error())
		}
		if err := applyOwner(dir.path, dir.uid, dir.gid, false); err != nil {
			return 0, nil, newOpError(ErrInternal, "恢复目录属主失败: "+err.Error())
		}
	}

	failed := make([]string, 0)
	for index, filter := range filters {
		if !matched[index] {
			failed = append(failed, filter)
		}
	}
	return restored, failed, nil
}

// extractEntry 按类型落盘单个条目；symbolic link 只重建链接，不跟随。
func extractEntry(reader *tar.Reader, staging, dest string, header *tar.Header, dirs *[]dirMeta) error {
	mode := fs.FileMode(header.Mode) & 0o7777
	switch header.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return newOpError(ErrInternal, "创建目录失败: "+err.Error())
		}
		*dirs = append(*dirs, dirMeta{
			path:    dest,
			mode:    mode,
			modTime: header.ModTime,
			uid:     header.Uid,
			gid:     header.Gid,
		})
		return nil
	case tar.TypeReg, tar.TypeRegA:
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return newOpError(ErrInternal, "创建父目录失败: "+err.Error())
		}
		file, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return newOpError(ErrInternal, "创建文件失败: "+err.Error())
		}
		written, copyErr := io.CopyN(file, reader, header.Size)
		closeErr := file.Close()
		if copyErr != nil || written != header.Size {
			_ = os.Remove(dest)
			return newOpError(ErrInternal, "解出文件内容失败")
		}
		if closeErr != nil {
			_ = os.Remove(dest)
			return newOpError(ErrInternal, "关闭文件失败: "+closeErr.Error())
		}
		// 不信任 tar 头：丢弃 setuid / setgid 位，只保留常规权限位。
		if err := os.Chmod(dest, mode&0o777); err != nil {
			return newOpError(ErrInternal, "恢复文件权限失败: "+err.Error())
		}
		if err := os.Chtimes(dest, header.ModTime, header.ModTime); err != nil {
			return newOpError(ErrInternal, "恢复文件时间失败: "+err.Error())
		}
		if err := applyOwner(dest, header.Uid, header.Gid, false); err != nil {
			return newOpError(ErrInternal, "恢复文件属主失败: "+err.Error())
		}
		return nil
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return newOpError(ErrInternal, "创建父目录失败: "+err.Error())
		}
		if err := os.Symlink(header.Linkname, dest); err != nil {
			return newOpError(ErrInternal, "创建符号链接失败: "+err.Error())
		}
		if err := applyOwner(dest, header.Uid, header.Gid, true); err != nil {
			return newOpError(ErrInternal, "恢复符号链接属主失败: "+err.Error())
		}
		return nil
	default:
		return newOpError(ErrInvalid, "不支持的归档条目类型")
	}
}

// validateRestoreHeader 不信任 tar 头：拒绝绝对路径、.. 段、超长路径、设备/块设备/
// FIFO/硬链接，以及大小超过 2 GiB 的单条目；符号链接目标不得逃出恢复根。
func validateRestoreHeader(header *tar.Header) (string, *pluginapi.Error) {
	raw := header.Name
	if raw == "" {
		return "", invalid("归档条目路径为空")
	}
	if len(raw) > MaxEntryNameLen {
		return "", invalid("归档条目路径超过 4096 字节上限")
	}
	if strings.ContainsRune(raw, 0) || strings.Contains(raw, "\\") {
		return "", invalid("归档条目路径含非法字符")
	}
	if path.IsAbs(raw) {
		return "", invalid("归档条目使用绝对路径，整包拒绝")
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "", invalid("归档条目含 .. 段，整包拒绝")
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") {
		return "", invalid("归档条目路径非法")
	}
	switch header.Typeflag {
	case tar.TypeDir, tar.TypeReg, tar.TypeRegA, tar.TypeSymlink:
	case tar.TypeChar, tar.TypeBlock:
		return "", invalid("归档包含字符/块设备条目，整包拒绝")
	default:
		return "", invalid("归档包含不支持的条目类型，整包拒绝")
	}
	if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
		if header.Size < 0 || header.Size > MaxEntryBytes {
			return "", invalid("归档条目大小超过 2 GiB 上限")
		}
	}
	if header.Typeflag == tar.TypeSymlink {
		if err := validateSymlinkTarget(clean, header.Linkname); err != nil {
			return "", err
		}
	}
	return clean, nil
}

// validateSymlinkTarget 拒绝绝对目标与逃出恢复根的相对目标。
func validateSymlinkTarget(entryName, target string) *pluginapi.Error {
	if target == "" || len(target) > MaxEntryNameLen {
		return invalid("符号链接目标为空或超长")
	}
	if strings.ContainsRune(target, 0) {
		return invalid("符号链接目标含 NUL 字节")
	}
	if path.IsAbs(target) {
		return invalid("符号链接目标为绝对路径，整包拒绝")
	}
	resolved := path.Clean(path.Join(path.Dir(entryName), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return invalid("符号链接目标越出恢复目录，整包拒绝")
	}
	return nil
}

// matchEntry 判定归档条目是否命中过滤项（精确命中或目录前缀）。
func matchEntry(name, filter string) bool {
	return name == filter || strings.HasPrefix(name, filter+"/")
}

// errFromProtocol 把包装级 pluginapi.Error 转回执行面错误，保持错误码不丢。
func errFromProtocol(perr *pluginapi.Error) error {
	switch perr.Code {
	case pluginapi.E_INVALID:
		return newOpError(ErrInvalid, perr.Message)
	case pluginapi.E_NOTFOUND:
		return newOpError(ErrNotFound, perr.Message)
	case pluginapi.E_CONFLICT:
		return newOpError(ErrConflict, perr.Message)
	case pluginapi.E_UNSUPPORTED:
		return newOpError(ErrUnsupported, perr.Message)
	default:
		return newOpError(ErrInternal, perr.Message)
	}
}
