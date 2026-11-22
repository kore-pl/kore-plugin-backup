package backupop

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// realFS 是 Executor 的真实实现：archive/tar + compress/gzip 全部走标准库。
type realFS struct{}

// Create 遍历 paths 真实打包：保留相对路径、模式、mtime、uid/gid，
// 不跟随符号链接（记 TypeSymlink）；边写边算压缩后文件 sha256。
func (realFS) Create(spec CreateSpec) (CreateData, error) {
	if err := os.MkdirAll(spec.Dir, 0o700); err != nil {
		return CreateData{}, newOpError(ErrInternal, "创建备份目录失败: "+err.Error())
	}
	now := time.Now().UTC()
	id := spec.Name + "-" + now.Format("20060102T150405Z")
	ext := ".tar.gz"
	if spec.Compression == "none" {
		ext = ".tar"
	}
	archivePath := filepath.Join(spec.Dir, id+ext)

	file, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return CreateData{}, newOpError(ErrConflict, "同名备份 id 已存在，请稍后重试")
		}
		return CreateData{}, newOpError(ErrInternal, "创建备份包失败: "+err.Error())
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(archivePath)
			_ = os.Remove(archivePath + ".sha256")
			_ = os.Remove(archivePath + ".meta.json")
		}
	}()

	hasher := sha256.New()
	sink := io.MultiWriter(file, hasher)
	var compressor *gzip.Writer
	if spec.Compression == "gzip" {
		compressor = gzip.NewWriter(sink)
		sink = compressor
	}
	writer := tar.NewWriter(sink)

	maxBytes := spec.MaxBytes
	if maxBytes <= 0 || maxBytes > DefaultMaxBytes {
		maxBytes = DefaultMaxBytes
	}
	maxEntries := spec.MaxEntries
	if maxEntries <= 0 || maxEntries > DefaultMaxEntries {
		maxEntries = DefaultMaxEntries
	}
	builder := &archiveBuilder{
		exclude:    spec.Exclude,
		maxBytes:   maxBytes,
		maxEntries: maxEntries,
		seen:       make(map[string]struct{}),
		skipped:    []string{},
		warnings:   []string{},
	}
	if err := builder.addAll(writer, spec.Paths); err != nil {
		return CreateData{}, err
	}
	if err := writer.Close(); err != nil {
		return CreateData{}, newOpError(ErrInternal, "收尾 tar 失败: "+err.Error())
	}
	if compressor != nil {
		if err := compressor.Close(); err != nil {
			return CreateData{}, newOpError(ErrInternal, "收尾 gzip 失败: "+err.Error())
		}
	}
	if err := file.Sync(); err != nil {
		return CreateData{}, newOpError(ErrInternal, "落盘备份包失败: "+err.Error())
	}
	info, err := file.Stat()
	if err != nil {
		return CreateData{}, newOpError(ErrInternal, "读取备份包大小失败: "+err.Error())
	}
	if err := file.Close(); err != nil {
		return CreateData{}, newOpError(ErrInternal, "关闭备份包失败: "+err.Error())
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	createdAt := now.Format(time.RFC3339)

	meta := metaFile{
		ID:          id,
		Name:        spec.Name,
		CreatedAt:   createdAt,
		Entries:     builder.entries,
		SizeBytes:   info.Size(),
		SHA256:      sum,
		Compression: spec.Compression,
	}
	if err := writeSidecars(archivePath, meta); err != nil {
		return CreateData{}, newOpError(ErrInternal, "写入 sidecar 失败: "+err.Error())
	}
	committed = true
	return CreateData{
		ID:        id,
		Path:      archivePath,
		SizeBytes: info.Size(),
		Entries:   builder.entries,
		SHA256:    sum,
		CreatedAt: createdAt,
		Skipped:   builder.skipped,
		Warnings:  builder.warnings,
	}, nil
}

// archiveBuilder 累积一次打包的条目数、未压缩字节数与跳过/告警信息。
type archiveBuilder struct {
	exclude    []string
	maxBytes   int64
	maxEntries int
	entries    int
	bytes      int64
	seen       map[string]struct{}
	skipped    []string
	warnings   []string
}

// addAll 按给定顺序遍历全部源路径，最终至少要求一个条目。
func (b *archiveBuilder) addAll(writer *tar.Writer, roots []string) error {
	for _, root := range roots {
		if err := b.addPath(writer, root); err != nil {
			return err
		}
	}
	if b.entries == 0 {
		return newOpError(ErrInvalid, "没有可备份的条目（路径为空或被 exclude 全部排除）")
	}
	return nil
}

// addPath 遍历一个源路径；符号链接只记录链接自身，不递归目标。
func (b *archiveBuilder) addPath(writer *tar.Writer, root string) error {
	base := archiveBase(root)
	return filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			rel := relativeName(base, current)
			b.skip(rel, "无法访问，已跳过")
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel := relativeName(base, current)
		if rel == "" {
			return nil
		}
		if b.excluded(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := b.seen[rel]; ok {
			b.skip(rel, "重复条目，已跳过")
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			b.skip(rel, "读取元信息失败，已跳过")
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case info.IsDir():
			return b.writeDir(writer, rel, info)
		case info.Mode().IsRegular():
			return b.writeFile(writer, current, rel, info)
		case info.Mode()&os.ModeSymlink != 0:
			return b.writeSymlink(writer, current, rel, info)
		default:
			b.skip(rel, "非常规文件（设备/FIFO/socket），已跳过")
			return nil
		}
	})
}

// writeDir 写入目录条目。
func (b *archiveBuilder) writeDir(writer *tar.Writer, rel string, info fs.FileInfo) error {
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return newOpError(ErrInternal, "构造目录头失败: "+err.Error())
	}
	name := rel
	if !strings.HasSuffix(name, "/") {
		name += "/"
	}
	header.Name = name
	header.Uid, header.Gid = fileOwner(info)
	header.Format = tar.FormatPAX
	clearVolatileTimes(header)
	if err := b.writeHeader(writer, header); err != nil {
		return err
	}
	b.seen[rel] = struct{}{}
	return nil
}

// writeFile 写入常规文件内容；先打开源文件再写头，避免「头已写但内容读不出」的半包。
func (b *archiveBuilder) writeFile(writer *tar.Writer, current, rel string, info fs.FileInfo) error {
	file, err := os.Open(current)
	if err != nil {
		b.skip(rel, "打开文件失败，已跳过")
		return nil
	}
	defer func() { _ = file.Close() }()

	if info.Size() > b.maxBytes-b.bytes {
		return newOpError(ErrInvalid, fmt.Sprintf("备份内容超过单次 %d 字节上限，已中止并删除半成品", b.maxBytes))
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return newOpError(ErrInternal, "构造文件头失败: "+err.Error())
	}
	header.Name = rel
	header.Uid, header.Gid = fileOwner(info)
	header.Format = tar.FormatPAX
	clearVolatileTimes(header)
	if err := b.writeHeader(writer, header); err != nil {
		return err
	}
	written, err := io.CopyN(writer, file, info.Size())
	if err != nil {
		return newOpError(ErrInternal, fmt.Sprintf("读取文件内容失败（已写 %d 字节）", written))
	}
	b.bytes += written
	b.seen[rel] = struct{}{}
	return nil
}

// writeSymlink 写入符号链接条目，不跟随目标。
func (b *archiveBuilder) writeSymlink(writer *tar.Writer, current, rel string, info fs.FileInfo) error {
	target, err := os.Readlink(current)
	if err != nil {
		b.skip(rel, "读取符号链接失败，已跳过")
		return nil
	}
	header, err := tar.FileInfoHeader(info, target)
	if err != nil {
		return newOpError(ErrInternal, "构造符号链接头失败: "+err.Error())
	}
	header.Name = rel
	header.Uid, header.Gid = fileOwner(info)
	header.Format = tar.FormatPAX
	clearVolatileTimes(header)
	if err := b.writeHeader(writer, header); err != nil {
		return err
	}
	b.seen[rel] = struct{}{}
	return nil
}

// writeHeader 在写入前检查条目上限。
func (b *archiveBuilder) writeHeader(writer *tar.Writer, header *tar.Header) error {
	if b.entries >= b.maxEntries {
		return newOpError(ErrInvalid, fmt.Sprintf("备份条目数超过单次 %d 上限，已中止并删除半成品", b.maxEntries))
	}
	if err := writer.WriteHeader(header); err != nil {
		return newOpError(ErrInternal, "写入 tar 头失败: "+err.Error())
	}
	b.entries++
	return nil
}

// excluded 判定相对路径是否命中 exclude glob（同时匹配完整相对路径与 basename）。
func (b *archiveBuilder) excluded(rel string) bool {
	for _, pattern := range b.exclude {
		if matched, _ := filepath.Match(pattern, rel); matched {
			return true
		}
		if matched, _ := filepath.Match(pattern, path.Base(rel)); matched {
			return true
		}
	}
	return false
}

// skip 记录被跳过的条目与原因。
func (b *archiveBuilder) skip(rel, reason string) {
	if rel != "" {
		b.skipped = append(b.skipped, rel)
		b.warnings = append(b.warnings, reason+": "+rel)
	}
}

// relativeName 计算相对父目录的斜杠路径；"." 归一为空串（不写根条目）。
func relativeName(base, current string) string {
	rel, err := filepath.Rel(base, current)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// clearVolatileTimes 丢弃 atime/ctime：读取源目录本身会刷新 atime，
// 若写入 PAX 头会让相同源两次打包的 sha256 不稳定；只保留 mtime。
func clearVolatileTimes(header *tar.Header) {
	header.AccessTime = time.Time{}
	header.ChangeTime = time.Time{}
}

// writeSidecars 原子写入 .sha256 与 .meta.json，权限均为 0600。
func writeSidecars(archivePath string, meta metaFile) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := atomicWrite(archivePath+".meta.json", append(data, '\n')); err != nil {
		return err
	}
	line := fmt.Sprintf("%s  %s\n", meta.SHA256, filepath.Base(archivePath))
	return atomicWrite(archivePath+".sha256", []byte(line))
}

// atomicWrite 用同目录临时文件 + rename 写 0600 文件，避免半截 sidecar。
func atomicWrite(target string, content []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, target)
}
