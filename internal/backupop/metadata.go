package backupop

import (
	"archive/tar"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// archiveExts 是备份包支持的两种扩展名及其压缩算法。
var archiveExts = []struct {
	ext         string
	compression string
}{
	{".tar.gz", "gzip"},
	{".tar", "none"},
}

// List 扫描备份目录，解析 sidecar 与 tar 头；读头失败标记 corrupted=true。
func (realFS) List(dir string) ([]BackupInfo, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []BackupInfo{}, nil
		}
		return nil, newOpError(ErrInternal, "读取备份目录失败: "+err.Error())
	}
	backups := make([]BackupInfo, 0, len(items))
	for _, item := range items {
		if item.IsDir() {
			continue
		}
		base := item.Name()
		ext, compression, ok := archiveExt(base)
		if !ok {
			continue
		}
		id := strings.TrimSuffix(base, ext)
		if _, perr := ValidateID(id); perr != nil {
			continue
		}
		info, err := item.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		archivePath := filepath.Join(dir, base)
		backup := BackupInfo{
			ID:          id,
			Name:        derivedName(id),
			CreatedAt:   derivedCreatedAt(id, info.ModTime()),
			SizeBytes:   info.Size(),
			Compression: compression,
			Corrupted:   archiveHeaderBroken(archivePath, compression),
		}
		meta, metaErr := readMeta(archivePath + ".meta.json")
		if metaErr == nil {
			backup.Name = meta.Name
			backup.CreatedAt = meta.CreatedAt
			backup.Entries = meta.Entries
			backup.SHA256 = meta.SHA256
		}
		if sha, err := readSidecarSHA(archivePath + ".sha256"); err == nil {
			backup.SHA256 = sha
		}
		backup.Managed = metaErr == nil && managedMeta(meta, id, backup.SHA256)
		backups = append(backups, backup)
	}
	sortBackups(backups)
	return backups, nil
}

// managedMeta 判定 sidecar 是否构成"本插件创建"的证据：
// meta.id 必须与文件名一致，且 meta.sha256 必须与 .sha256 sidecar 相同。
// 手工拷贝/外来包缺 sidecar 或两者不一致时 Managed=false，保留策略不删。
func managedMeta(meta metaFile, id, sidecarSHA string) bool {
	if meta.ID != id || meta.SHA256 == "" || sidecarSHA == "" {
		return false
	}
	return meta.SHA256 == sidecarSHA
}

// sortBackups 按创建时间倒序、同刻按 id 倒序；List 与 retention 共用同一排序口径。
func sortBackups(backups []BackupInfo) {
	sort.Slice(backups, func(i, j int) bool {
		ti, tj := parsedTime(backups[i].CreatedAt), parsedTime(backups[j].CreatedAt)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return backups[i].ID > backups[j].ID
	})
}

// resolveArchive 在备份目录内定位 id 对应的包，返回绝对路径与压缩算法。
func resolveArchive(dir, id string) (string, string, error) {
	for _, candidate := range archiveExts {
		archivePath := filepath.Join(dir, id+candidate.ext)
		info, err := os.Lstat(archivePath)
		if err == nil {
			if !info.Mode().IsRegular() {
				return "", "", newOpError(ErrInvalid, "备份包不是常规文件")
			}
			return archivePath, candidate.compression, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", "", newOpError(ErrInternal, "读取备份包失败: "+err.Error())
		}
	}
	return "", "", newOpError(ErrNotFound, "备份不存在")
}

// archiveExt 判定文件名是否备份包，返回匹配的扩展名与压缩算法。
func archiveExt(name string) (string, string, bool) {
	for _, candidate := range archiveExts {
		if strings.HasSuffix(name, candidate.ext) {
			return candidate.ext, candidate.compression, true
		}
	}
	return "", "", false
}

// readSidecarSHA 读取 .sha256 文件的首个字段（64 位 hex）。
func readSidecarSHA(sidecar string) (string, error) {
	data, err := os.ReadFile(sidecar)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || len(fields[0]) != hex.EncodedLen(32) {
		return "", errors.New("sha256 sidecar 格式非法")
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", errors.New("sha256 sidecar 不是合法 hex")
	}
	return fields[0], nil
}

// readMeta 读取 .meta.json；不存在或损坏都返回错误，由调用方降级。
func readMeta(metaPath string) (metaFile, error) {
	var meta metaFile
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// archiveHeaderBroken 只读第一个 tar 头判定包是否可解；空包与读取失败算损坏。
func archiveHeaderBroken(archivePath, compression string) bool {
	file, err := os.Open(archivePath)
	if err != nil {
		return true
	}
	defer func() { _ = file.Close() }()
	var reader io.Reader = file
	if compression == "gzip" {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return true
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	header, err := tar.NewReader(reader).Next()
	return err != nil || header.Name == ""
}

// derivedName 从 id（<name>-<timestamp>）反推 name；没有时间戳后缀时原样返回。
func derivedName(id string) string {
	index := strings.LastIndex(id, "-")
	if index <= 0 {
		return id
	}
	if _, err := time.Parse("20060102T150405Z", id[index+1:]); err == nil {
		return id[:index]
	}
	return id
}

// derivedCreatedAt 从 id 时间戳解析创建时间，失败回退文件 mtime。
func derivedCreatedAt(id string, modTime time.Time) string {
	index := strings.LastIndex(id, "-")
	if index > 0 {
		if t, err := time.Parse("20060102T150405Z", id[index+1:]); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return modTime.UTC().Format(time.RFC3339)
}
