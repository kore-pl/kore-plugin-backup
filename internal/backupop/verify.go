package backupop

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// verifyParams 是 backup.verify 的显式入参形状。
type verifyParams struct {
	ID string `json:"id"`
}

// callVerify 处理 backup.verify：重算 sha256 与 sidecar 比对，并逐条目解压校验。
func callVerify(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p verifyParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	id, perr := ValidateID(p.ID)
	if perr != nil {
		return nil, perr
	}
	started := time.Now()
	data, err := exec.Verify(opts.Dir, id)
	if err != nil {
		return nil, classify(err)
	}
	data.DurationMS = elapsedMS(started)
	return encode(data)
}

// Verify 计算压缩包真实 sha256 并与 sidecar 比对，再完整读取一遍 tar 内容。
// 包损坏时仍返回结果（corrupted=true、sha_match 按实际比对），便于调用方决策。
func (realFS) Verify(dir, id string) (VerifyData, error) {
	archivePath, compression, err := resolveArchive(dir, id)
	if err != nil {
		return VerifyData{}, err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return VerifyData{}, newOpError(ErrInternal, "打开备份包失败: "+err.Error())
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return VerifyData{}, newOpError(ErrInternal, "读取备份包大小失败: "+err.Error())
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return VerifyData{}, newOpError(ErrInternal, "计算 sha256 失败: "+err.Error())
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	sidecar, sidecarErr := readSidecarSHA(archivePath + ".sha256")

	result := VerifyData{
		ID:            id,
		SizeBytes:     info.Size(),
		SHA256:        sum,
		SidecarSHA256: sidecar,
		SHAMatch:      sidecarErr == nil && sidecar == sum,
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return VerifyData{}, newOpError(ErrInternal, "重置读取位置失败: "+err.Error())
	}
	var reader io.Reader = file
	if compression == "gzip" {
		gz, err := gzip.NewReader(file)
		if err != nil {
			result.Corrupted = true
			return result, nil
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	tarReader := tar.NewReader(reader)
	result.Entries, result.UncompressedBytes, result.Corrupted = scanTarEntries(tarReader)
	return result, nil
}

// scanTarEntries 逐条目读取内容，验证 gzip/tar 流与条目大小声明一致。
func scanTarEntries(reader *tar.Reader) (int, int64, bool) {
	entries := 0
	var total int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return entries, total, false
		}
		if err != nil {
			return entries, total, true
		}
		entries++
		if header.Size < 0 || header.Size > MaxEntryBytes {
			return entries, total, true
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			written, err := io.Copy(io.Discard, reader)
			total += written
			if err != nil || written != header.Size {
				return entries, total, true
			}
		}
	}
}
