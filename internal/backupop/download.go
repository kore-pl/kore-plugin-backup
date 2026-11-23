package backupop

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 下载能力边界：只读本插件备份目录内的 tar 包，按 id 解析并二次校验归属；
// 大包用 offset/limit 分块（单块 ≤4 MiB，base64 后 ≤5.34 MiB，低于 8 MiB 帧上限），
// 同时返回宿主路径供主壳既有文件域 / 用户直接取走，不把整个包塞进单帧。
const (
	// DefaultDownloadChunk 是 inline 分块缺省大小：4 MiB。
	DefaultDownloadChunk int64 = 4 << 20
	// MaxDownloadChunk 是单次 inline 分块上限：4 MiB。
	MaxDownloadChunk int64 = 4 << 20
)

// DownloadSpec 是执行面收到的下载请求；Limit 为 0 表示只取元信息（不读内容）。
type DownloadSpec struct {
	Dir    string
	ID     string
	Offset int64
	Limit  int64
}

// DownloadData 是 backup.download 的结果：元信息 + 可选的分块 base64。
type DownloadData struct {
	ID            string `json:"id"`
	Filename      string `json:"filename"`
	Path          string `json:"path"`
	SizeBytes     int64  `json:"size_bytes"`
	SHA256        string `json:"sha256,omitempty"`
	ContentType   string `json:"content_type"`
	Inline        bool   `json:"inline"`
	Offset        int64  `json:"offset"`
	NextOffset    int64  `json:"next_offset"`
	EOF           bool   `json:"eof"`
	ChunkBytes    int    `json:"chunk_bytes"`
	MaxChunkBytes int64  `json:"max_chunk_bytes"`
	DataBase64    string `json:"data_base64,omitempty"`
	Hint          string `json:"hint"`
}

// downloadParams 是 backup.download 的显式入参形状。
type downloadParams struct {
	ID     string `json:"id"`
	Inline bool   `json:"inline"`
	Offset int64  `json:"offset"`
	Limit  int64  `json:"limit"`
}

// callDownload 处理 backup.download：校验 id 与偏移，再交给执行面按块读取。
func callDownload(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p downloadParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	id, perr := ValidateID(p.ID)
	if perr != nil {
		return nil, perr
	}
	if p.Offset < 0 {
		return nil, invalid("offset 不能为负数")
	}
	spec := DownloadSpec{Dir: opts.Dir, ID: id}
	if p.Inline {
		limit := p.Limit
		if limit == 0 {
			limit = DefaultDownloadChunk
		}
		if limit < 0 || limit > MaxDownloadChunk {
			return nil, invalid("limit 必须在 1–4194304 字节之间（不传按 4 MiB）")
		}
		spec.Offset = p.Offset
		spec.Limit = limit
	}
	data, err := exec.Download(spec)
	if err != nil {
		return nil, classify(err)
	}
	if data.Hint == "" {
		data.Hint = "inline=true 按 offset/limit 分块取内容；或直接用 path 在宿主机/主壳文件域取走该包"
	}
	return encode(data)
}

// Download 读取备份包元信息与可选分块；只允许本插件备份目录内的常规文件。
func (realFS) Download(spec DownloadSpec) (DownloadData, error) {
	if _, perr := ValidateID(spec.ID); perr != nil {
		return DownloadData{}, newOpError(ErrInvalid, perr.Message)
	}
	dir := filepath.Clean(spec.Dir)
	archivePath, compression, err := resolveArchive(dir, spec.ID)
	if err != nil {
		return DownloadData{}, err
	}
	if err := assertInsideBackupDir(dir, archivePath); err != nil {
		return DownloadData{}, err
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		return DownloadData{}, newOpError(ErrInternal, "读取备份包信息失败: "+err.Error())
	}
	data := DownloadData{
		ID:            spec.ID,
		Filename:      filepath.Base(archivePath),
		Path:          archivePath,
		SizeBytes:     info.Size(),
		ContentType:   contentTypeFor(compression),
		Inline:        spec.Limit > 0,
		Offset:        spec.Offset,
		NextOffset:    spec.Offset,
		EOF:           true,
		MaxChunkBytes: MaxDownloadChunk,
	}
	if sha, shaErr := readSidecarSHA(archivePath + ".sha256"); shaErr == nil {
		data.SHA256 = sha
	}
	if spec.Limit == 0 {
		return data, nil
	}
	if spec.Offset > info.Size() {
		return DownloadData{}, newOpError(ErrInvalid, "offset 超过备份包大小")
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return DownloadData{}, newOpError(ErrInternal, "打开备份包失败: "+err.Error())
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(spec.Offset, io.SeekStart); err != nil {
		return DownloadData{}, newOpError(ErrInternal, "定位备份包偏移失败: "+err.Error())
	}
	chunk, err := io.ReadAll(io.LimitReader(file, spec.Limit))
	if err != nil {
		return DownloadData{}, newOpError(ErrInternal, "读取备份包失败: "+err.Error())
	}
	next := spec.Offset + int64(len(chunk))
	data.ChunkBytes = len(chunk)
	data.NextOffset = next
	data.EOF = next >= info.Size()
	data.DataBase64 = base64.StdEncoding.EncodeToString(chunk)
	return data, nil
}

// assertInsideBackupDir 二次确认目标确实在本插件备份目录内（含符号链接解析后）。
// 这是下载 / 删除类动作的越界保护：绝不按调用方给的路径直接读。
func assertInsideBackupDir(dir, target string) error {
	cleanDir := filepath.Clean(dir)
	cleanTarget := filepath.Clean(target)
	if filepath.Dir(cleanTarget) != cleanDir || !withinPath(cleanDir, cleanTarget) {
		return newOpError(ErrInvalid, "目标不在本插件备份目录内，已拒绝")
	}
	realDir, err := filepath.EvalSymlinks(cleanDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return newOpError(ErrNotFound, "备份目录不存在")
		}
		return newOpError(ErrInternal, "解析备份目录失败: "+err.Error())
	}
	realTarget, err := filepath.EvalSymlinks(cleanTarget)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return newOpError(ErrNotFound, "备份包不存在")
		}
		return newOpError(ErrInternal, "解析备份包路径失败: "+err.Error())
	}
	if filepath.Dir(realTarget) != realDir {
		return newOpError(ErrInvalid, "备份包经符号链接解析后越出备份目录，已拒绝")
	}
	return nil
}

// contentTypeFor 按压缩算法给出下载 MIME。
func contentTypeFor(compression string) string {
	if compression == "gzip" {
		return "application/gzip"
	}
	return "application/x-tar"
}
