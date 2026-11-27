package backupop

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// backup.upload 的边界与 backup.download 对称：入站按 4 MiB 分块 base64，
// 只把外部 tar/tar.gz 导入本插件备份目录，绝不自动恢复、绝不接收 target_dir。
// 所有阶段共用隐藏暂存目录 <dir>/.upload-<upload_id>/（0700），校验通过才
// 以 create-only 方式移入备份目录；失败或 30 分钟未提交一律清理。
const (
	// MaxUploadBytes 是上传包声明总长上限：512 MiB（防 DoS），硬顶不超过 DefaultMaxBytes（2 GiB）。
	MaxUploadBytes int64 = 512 << 20
	// MaxUploadChunk 是单次上传分块上限：4 MiB，与 MaxDownloadChunk 对齐。
	MaxUploadChunk int64 = 4 << 20
	// UploadTTL 是未完成暂存的有效期；超时按失败清理。
	UploadTTL = 30 * time.Minute
	// uploadDirPrefix 是暂存目录前缀（点号开头，backup.list 会跳过目录条目）。
	uploadDirPrefix = ".upload-"
	// uploadStateFile 是暂存目录内的显式状态文件（不写 .meta.json）。
	uploadStateFile = ".upload.json"
	// uploadPayload 是暂存目录内接收字节的载荷文件。
	uploadPayload = "payload.bin"
)

// uploadParams 是 backup.upload 四阶段共用的显式入参形状（G-24：无 any / object）。
type uploadParams struct {
	Phase      string `json:"phase"`
	UploadID   string `json:"upload_id"`
	Filename   string `json:"filename,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Offset     int64  `json:"offset,omitempty"`
	DataBase64 string `json:"data_base64,omitempty"`
}

// UploadSpec 是执行面收到的上传请求；Data 为已严格解码的分块原始字节。
type UploadSpec struct {
	Dir        string
	Phase      string
	UploadID   string
	Filename   string
	SizeBytes  int64
	SHA256     string
	Offset     int64
	Data       []byte
	MaxEntries int
	MaxBytes   int64
}

// UploadData 是执行面各阶段结果的显式汇总；Call 按 phase 收窄为对应结果形状。
type UploadData struct {
	UploadID      string `json:"upload_id"`
	ChunkBytes    int64  `json:"chunk_bytes,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	Offset        int64  `json:"offset,omitempty"`
	NextOffset    int64  `json:"next_offset,omitempty"`
	ReceivedBytes int64  `json:"received_bytes,omitempty"`
	ID            string `json:"id,omitempty"`
	Filename      string `json:"filename,omitempty"`
	SizeBytes     int64  `json:"size_bytes,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Entries       int    `json:"entries,omitempty"`
	Compression   string `json:"compression,omitempty"`
	Managed       bool   `json:"managed,omitempty"`
	Aborted       bool   `json:"aborted,omitempty"`
}

// uploadState 是 .upload.json 的落盘结构；CreatedAt 用于 30 分钟 TTL 清理。
type uploadState struct {
	UploadID      string `json:"upload_id"`
	Filename      string `json:"filename"`
	SizeBytes     int64  `json:"size_bytes"`
	SHA256        string `json:"sha256,omitempty"`
	ReceivedBytes int64  `json:"received_bytes"`
	CreatedAt     string `json:"created_at"`
	Compression   string `json:"compression"`
}

// 四阶段的对外结果形状；显式字段，避免 omitempty 丢掉 managed=false 等语义值。
type uploadBeginResult struct {
	UploadID   string `json:"upload_id"`
	ChunkBytes int64  `json:"chunk_bytes"`
	ExpiresAt  string `json:"expires_at"`
	Hint       string `json:"hint"`
}
type uploadChunkResult struct {
	UploadID      string `json:"upload_id"`
	Offset        int64  `json:"offset"`
	NextOffset    int64  `json:"next_offset"`
	ReceivedBytes int64  `json:"received_bytes"`
}
type uploadCommitResult struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
	Entries     int    `json:"entries"`
	Compression string `json:"compression"`
	Managed     bool   `json:"managed"`
	DurationMS  int64  `json:"duration_ms"`
	Hint        string `json:"hint"`
}
type uploadAbortResult struct {
	UploadID string `json:"upload_id"`
	Aborted  bool   `json:"aborted"`
}

// callUpload 校验 phase 专属参数后交给执行面；业务错误分类复用 Call 的统一出口。
func callUpload(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p uploadParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	spec := UploadSpec{
		Dir: opts.Dir, Phase: p.Phase, UploadID: p.UploadID, Filename: p.Filename,
		SizeBytes: p.SizeBytes, SHA256: p.SHA256, Offset: p.Offset,
		MaxEntries: opts.MaxEntries, MaxBytes: opts.MaxBytes,
	}
	started := time.Now()
	switch p.Phase {
	case "begin":
		id, perr := ValidateID(p.UploadID)
		if perr != nil {
			return nil, perr
		}
		spec.UploadID = id
		if perr := validateUploadFilename(p.Filename); perr != nil {
			return nil, perr
		}
		if p.SizeBytes <= 0 || p.SizeBytes > MaxUploadBytes || p.SizeBytes > DefaultMaxBytes {
			return nil, invalid(fmt.Sprintf("文件大小必须在 1–%d 字节之间（上限 512 MiB）", MaxUploadBytes))
		}
		if p.SHA256 != "" {
			if len(p.SHA256) != hex.EncodedLen(32) {
				return nil, invalid("sha256 必须是 64 位十六进制字符串")
			}
			if _, err := hex.DecodeString(p.SHA256); err != nil {
				return nil, invalid("sha256 必须是 64 位十六进制字符串")
			}
		}
	case "chunk":
		id, perr := ValidateID(p.UploadID)
		if perr != nil {
			return nil, perr
		}
		spec.UploadID = id
		if p.Offset < 0 {
			return nil, invalid("offset 不能为负数")
		}
		if p.DataBase64 == "" {
			return nil, invalid("data_base64 不能为空")
		}
		decoded, err := base64.StdEncoding.DecodeString(p.DataBase64)
		if err != nil {
			return nil, invalid("data_base64 解码失败: " + err.Error())
		}
		if int64(len(decoded)) > MaxUploadChunk {
			return nil, invalid("单块超过 4 MiB 上限，已拒绝")
		}
		spec.Data = decoded
	case "commit", "abort":
		id, perr := ValidateID(p.UploadID)
		if perr != nil {
			return nil, perr
		}
		spec.UploadID = id
	default:
		return nil, invalid("phase 必须是 begin / chunk / commit / abort 之一")
	}
	data, err := exec.Upload(spec)
	if err != nil {
		return nil, classify(err)
	}
	switch p.Phase {
	case "begin":
		return encode(uploadBeginResult{
			UploadID: data.UploadID, ChunkBytes: MaxUploadChunk, ExpiresAt: data.ExpiresAt,
			Hint: "按 4 MiB 分块调用 phase=chunk；全部传完再调用 phase=commit 导入，导入不会自动恢复",
		})
	case "chunk":
		return encode(uploadChunkResult{
			UploadID: data.UploadID, Offset: data.Offset, NextOffset: data.NextOffset,
			ReceivedBytes: data.ReceivedBytes,
		})
	case "commit":
		return encode(uploadCommitResult{
			ID: data.ID, Filename: data.Filename, SizeBytes: data.SizeBytes, SHA256: data.SHA256,
			Entries: data.Entries, Compression: data.Compression, Managed: data.Managed,
			DurationMS: elapsedMS(started),
			Hint:       "导入完成；到备份列表点「恢复」并二次确认才会写入目标目录",
		})
	default:
		return encode(uploadAbortResult{UploadID: data.UploadID, Aborted: data.Aborted})
	}
}

// validateUploadFilename 只取 basename：拒绝对路径、NUL、反斜杠与路径分隔符，
// 且必须匹配本插件支持的 .tar / .tar.gz 扩展名（复用 archiveExt）。
func validateUploadFilename(raw string) *pluginapi.Error {
	if raw == "" {
		return invalid("filename 不能为空")
	}
	if strings.ContainsRune(raw, 0) || strings.Contains(raw, "\\") || strings.Contains(raw, "/") {
		return invalid("filename 只能是文件名，不能含路径分隔符")
	}
	if _, _, ok := archiveExt(raw); !ok {
		return invalid("仅支持 .tar 或 .tar.gz 备份包")
	}
	return nil
}

// Upload 实现四阶段：begin 隔离暂存 → chunk 顺序写 → commit 只读安全扫描后 create-only 移入 → abort/TTL 清理。
func (realFS) Upload(spec UploadSpec) (UploadData, error) {
	dir := filepath.Clean(spec.Dir)
	staging := filepath.Join(dir, uploadDirPrefix+spec.UploadID)
	switch spec.Phase {
	case "begin":
		return uploadBegin(dir, staging, spec)
	case "chunk":
		return uploadChunk(dir, staging, spec)
	case "commit":
		return uploadCommit(dir, staging, spec)
	case "abort":
		return uploadAbort(staging, spec.UploadID)
	default:
		return UploadData{}, newOpError(ErrInvalid, "未知上传阶段")
	}
}

// uploadBegin 建立 0700 隐藏暂存目录并写显式状态文件；同 id 已存在一律拒绝。
func uploadBegin(dir, staging string, spec UploadSpec) (UploadData, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return UploadData{}, newOpError(ErrInternal, "创建备份目录失败: "+err.Error())
	}
	if err := SweepStaleUploads(dir); err != nil {
		return UploadData{}, err
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return UploadData{}, newOpError(ErrConflict, "同名上传会话已存在，请先确认或直接导入完成")
		}
		return UploadData{}, newOpError(ErrInternal, "创建上传暂存目录失败: "+err.Error())
	}
	// 暂存目录已存在，assertInsideBackupDir 的 EvalSymlinks 可正常二次校验。
	if err := assertInsideBackupDir(dir, staging); err != nil {
		_ = os.Remove(staging)
		return UploadData{}, err
	}
	_, compression, _ := archiveExt(spec.Filename)
	now := time.Now().UTC()
	state := uploadState{
		UploadID: spec.UploadID, Filename: spec.Filename, SizeBytes: spec.SizeBytes,
		SHA256: spec.SHA256, ReceivedBytes: 0, CreatedAt: now.Format(time.RFC3339),
		Compression: compression,
	}
	if err := writeUploadState(staging, state); err != nil {
		_ = os.RemoveAll(staging)
		return UploadData{}, err
	}
	return UploadData{
		UploadID: spec.UploadID, ChunkBytes: MaxUploadChunk,
		ExpiresAt: now.Add(UploadTTL).UTC().Format(time.RFC3339),
	}, nil
}

// uploadChunk 只接受 offset == received_bytes 的顺序写，拒绝重叠与回退。
func uploadChunk(dir, staging string, spec UploadSpec) (UploadData, error) {
	state, err := readUploadState(staging)
	if err != nil {
		return UploadData{}, err
	}
	if err := checkUploadFresh(staging, state); err != nil {
		return UploadData{}, err
	}
	if state.UploadID != spec.UploadID {
		return UploadData{}, newOpError(ErrInvalid, "上传会话 id 不匹配")
	}
	if spec.Offset != state.ReceivedBytes {
		return UploadData{}, newOpError(ErrConflict, "分块 offset 必须等于已接收字节数，拒绝重叠或回退")
	}
	if int64(len(spec.Data)) == 0 || spec.Offset+int64(len(spec.Data)) > state.SizeBytes {
		return UploadData{}, newOpError(ErrInvalid, "分块超出声明的文件大小，已拒绝")
	}
	if state.SizeBytes > MaxUploadBytes || state.SizeBytes > DefaultMaxBytes {
		return UploadData{}, newOpError(ErrInvalid, "上传包超过 512 MiB 上限，已拒绝")
	}
	payload := filepath.Join(staging, uploadPayload)
	if !withinPath(staging, payload) {
		return UploadData{}, newOpError(ErrInvalid, "载荷路径越出暂存目录，已拒绝")
	}
	if info, err := os.Lstat(payload); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return UploadData{}, newOpError(ErrInvalid, "载荷路径是符号链接，已拒绝")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return UploadData{}, newOpError(ErrInternal, "读取载荷状态失败: "+err.Error())
	}
	file, err := os.OpenFile(payload, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return UploadData{}, newOpError(ErrInternal, "打开上传载荷失败: "+err.Error())
	}
	written, writeErr := file.WriteAt(spec.Data, spec.Offset)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || written != len(spec.Data) {
		return UploadData{}, newOpError(ErrInternal, "写入上传分块失败")
	}
	state.ReceivedBytes += int64(written)
	if err := writeUploadState(staging, state); err != nil {
		return UploadData{}, err
	}
	return UploadData{
		UploadID: spec.UploadID, Offset: spec.Offset,
		NextOffset: state.ReceivedBytes, ReceivedBytes: state.ReceivedBytes,
	}, nil
}

// uploadCommit 先校验完整性，再只读扫描归档安全门；只有全部通过才 create-only 移入备份目录。
func uploadCommit(dir, staging string, spec UploadSpec) (UploadData, error) {
	state, err := readUploadState(staging)
	if err != nil {
		return UploadData{}, err
	}
	if err := checkUploadFresh(staging, state); err != nil {
		return UploadData{}, err
	}
	// 校验失败一律清理暂存，绝不把半包或恶意包留在备份目录附近。
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()
	if state.ReceivedBytes != state.SizeBytes {
		return UploadData{}, newOpError(ErrInvalid, "上传未完成：已接收字节数与声明大小不一致")
	}
	payload := filepath.Join(staging, uploadPayload)
	if !withinPath(staging, payload) {
		return UploadData{}, newOpError(ErrInvalid, "载荷路径越出暂存目录，已拒绝")
	}
	sum, err := hashUploadFile(payload)
	if err != nil {
		return UploadData{}, err
	}
	if state.SHA256 != "" && sum != state.SHA256 {
		return UploadData{}, newOpError(ErrInvalid, "sha256 与收到的内容不匹配，整包拒绝")
	}
	maxEntries := spec.MaxEntries
	if maxEntries <= 0 || maxEntries > DefaultMaxEntries {
		maxEntries = DefaultMaxEntries
	}
	maxBytes := spec.MaxBytes
	if maxBytes <= 0 || maxBytes > DefaultMaxBytes {
		maxBytes = DefaultMaxBytes
	}
	entries, _, err := scanUploadArchive(payload, state.Compression, staging, maxEntries, maxBytes)
	if err != nil {
		return UploadData{}, err
	}
	name := strings.TrimSuffix(state.Filename, archiveNameExt(state.Filename))
	if _, perr := ValidateName(name); perr != nil {
		return UploadData{}, errFromProtocol(perr)
	}
	id := name + "-" + time.Now().UTC().Format("20060102T150405Z")
	ext := archiveNameExt(state.Filename)
	final := filepath.Join(dir, id+ext)
	if filepath.Dir(final) != dir || !withinPath(dir, final) {
		return UploadData{}, newOpError(ErrInvalid, "目标路径越出备份目录，已拒绝")
	}
	// os.Link 是 create-only 且同文件系统原子：同 id 已存在时 EEXIST，绝不覆盖。
	if err := os.Link(payload, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return UploadData{}, newOpError(ErrConflict, "同名备份 id 已存在，请稍后重试")
		}
		return UploadData{}, newOpError(ErrInternal, "提交上传包失败: "+err.Error())
	}
	line := fmt.Sprintf("%s  %s\n", sum, filepath.Base(final))
	if err := atomicWrite(final+".sha256", []byte(line)); err != nil {
		_ = os.Remove(final)
		return UploadData{}, newOpError(ErrInternal, "写入上传包 sidecar 失败: "+err.Error())
	}
	committed = true
	_ = os.RemoveAll(staging)
	return UploadData{
		ID: id, Filename: filepath.Base(final), SizeBytes: state.SizeBytes, SHA256: sum,
		Entries: entries, Compression: state.Compression, Managed: false,
	}, nil
}

// uploadAbort 删除整个暂存目录；不存在时明确报错，不伪造成功。
func uploadAbort(staging, uploadID string) (UploadData, error) {
	if _, err := os.Lstat(staging); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return UploadData{}, newOpError(ErrNotFound, "上传会话不存在或已清理")
		}
		return UploadData{}, newOpError(ErrInternal, "读取上传会话失败: "+err.Error())
	}
	if err := os.RemoveAll(staging); err != nil {
		return UploadData{}, newOpError(ErrInternal, "清理上传暂存失败: "+err.Error())
	}
	return UploadData{UploadID: uploadID, Aborted: true}, nil
}

// checkUploadFresh 拒绝超过 30 分钟的暂存会话并清理。
func checkUploadFresh(staging string, state uploadState) error {
	created, err := time.Parse(time.RFC3339, state.CreatedAt)
	if err != nil || time.Since(created) > UploadTTL {
		_ = os.RemoveAll(staging)
		return newOpError(ErrInvalid, "上传会话已超过 30 分钟，已清理，请重新上传")
	}
	return nil
}

// writeUploadState 用同目录临时文件 + rename 原子写状态（0600）。
func writeUploadState(staging string, state uploadState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return newOpError(ErrInternal, "序列化上传状态失败: "+err.Error())
	}
	if err := atomicWrite(filepath.Join(staging, uploadStateFile), append(data, '\n')); err != nil {
		return newOpError(ErrInternal, "写入上传状态失败: "+err.Error())
	}
	return nil
}

// readUploadState 读取暂存目录内的显式状态文件；不存在视为会话已失效。
func readUploadState(staging string) (uploadState, error) {
	data, err := os.ReadFile(filepath.Join(staging, uploadStateFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return uploadState{}, newOpError(ErrNotFound, "上传会话不存在或已清理")
		}
		return uploadState{}, newOpError(ErrInternal, "读取上传状态失败: "+err.Error())
	}
	var state uploadState
	if err := json.Unmarshal(data, &state); err != nil {
		return uploadState{}, newOpError(ErrInvalid, "上传状态损坏，已拒绝")
	}
	return state, nil
}

// hashUploadFile 流式计算实际收到字节的 sha256（不信任调用方声明）。
func hashUploadFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", newOpError(ErrInternal, "打开上传载荷失败: "+err.Error())
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", newOpError(ErrInternal, "计算上传包校验值失败: "+err.Error())
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// scanUploadArchive 只读扫描归档安全门：逐头复用 restore 的 validateRestoreHeader
// （拒绝对路径 / .. / NUL / 反斜杠 / 超长路径 / 设备 / FIFO / 硬链接 / 逃逸符号链接），
// 并用 withinPath 二次确认成员落点；只累计声明字节与条目数，绝不解出任何条目（防解压炸弹）。
func scanUploadArchive(payload, compression, staging string, maxEntries int, maxBytes int64) (int, int64, error) {
	file, err := os.Open(payload)
	if err != nil {
		return 0, 0, newOpError(ErrInternal, "打开上传包失败: "+err.Error())
	}
	defer func() { _ = file.Close() }()
	var reader io.Reader = file
	if compression == "gzip" {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return 0, 0, newOpError(ErrInvalid, "上传包不是合法 gzip，整包拒绝")
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	entries := 0
	var uncompressed int64
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, newOpError(ErrInvalid, "上传包损坏，无法解析归档，整包拒绝")
		}
		name, perr := validateRestoreHeader(header)
		if perr != nil {
			return 0, 0, errFromProtocol(perr)
		}
		dest := filepath.Join(staging, filepath.FromSlash(name))
		if !withinPath(staging, dest) {
			return 0, 0, newOpError(ErrInvalid, "归档条目越出暂存目录，整包拒绝")
		}
		uncompressed += header.Size
		if uncompressed > maxBytes {
			return 0, 0, newOpError(ErrInvalid, "解压后内容超过 2 GiB 上限，疑似解压炸弹，整包拒绝")
		}
		entries++
		if entries > maxEntries {
			return 0, 0, newOpError(ErrInvalid, "归档条目数超过 100 万上限，整包拒绝")
		}
	}
	if entries == 0 {
		return 0, 0, newOpError(ErrInvalid, "上传包为空，无可恢复条目，整包拒绝")
	}
	return entries, uncompressed, nil
}

// SweepStaleUploads 扫描备份目录里的 .upload-* 暂存，清理超过 30 分钟未提交的半包。
// 每次 begin 与插件启动时调用；状态缺失或损坏的隐藏暂存同样清理。
func SweepStaleUploads(dir string) error {
	items, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return newOpError(ErrInternal, "扫描上传暂存失败: "+err.Error())
	}
	now := time.Now()
	for _, item := range items {
		if !item.IsDir() || !strings.HasPrefix(item.Name(), uploadDirPrefix) {
			continue
		}
		root := filepath.Join(dir, item.Name())
		state, err := readUploadState(root)
		if err != nil {
			_ = os.RemoveAll(root)
			continue
		}
		created, parseErr := time.Parse(time.RFC3339, state.CreatedAt)
		if parseErr != nil || now.Sub(created) > UploadTTL {
			_ = os.RemoveAll(root)
		}
	}
	return nil
}

// archiveNameExt 返回文件名匹配的备份扩展名（非备份名返回空串）。
func archiveNameExt(name string) string {
	ext, _, ok := archiveExt(name)
	if !ok {
		return ""
	}
	return ext
}
