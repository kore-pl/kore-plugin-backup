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
	"os"
	"path/filepath"
	"strings"
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

// Verify 逐项校验备份包：存在 / 大小 / sha256 / 可读性 / 条目数。
// 失败不返回 E_*（除目录级错误）：把每项写进 checks，调用方据 ok 与 detail 定位坏在哪。
func (realFS) Verify(dir, id string) (VerifyData, error) {
	archivePath, compression, err := resolveArchive(dir, id)
	if err != nil {
		var op *OpError
		if errors.As(err, &op) && op.Kind == ErrNotFound {
			return VerifyData{
				ID: id,
				Checks: []VerifyCheck{{
					Name: "exists", OK: false,
					Detail: "备份不存在（已查 id.tar.gz 与 id.tar）",
				}},
			}, nil
		}
		return VerifyData{}, err
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		return VerifyData{}, newOpError(ErrInternal, "读取备份包大小失败: "+err.Error())
	}
	meta, _ := readMeta(archivePath + ".meta.json")
	sidecar, _ := readSidecarSHA(archivePath + ".sha256")
	createdAt := meta.CreatedAt
	if createdAt == "" {
		createdAt = derivedCreatedAt(id, info.ModTime())
	}
	result := VerifyData{
		ID: id, SizeBytes: info.Size(), SidecarSHA256: sidecar, CreatedAt: createdAt,
		Checks: make([]VerifyCheck, 0, 5),
	}
	result.Checks = append(result.Checks, VerifyCheck{
		Name: "exists", OK: true, Detail: "备份文件存在：" + filepath.Base(archivePath),
	})
	sizeOK := info.Size() > 0
	sizeDetail := fmt.Sprintf("大小 %d 字节", info.Size())
	if !sizeOK {
		sizeDetail = "文件为空（0 字节）"
	}
	result.Checks = append(result.Checks, VerifyCheck{Name: "size", OK: sizeOK, Detail: sizeDetail})
	if !sizeOK {
		return result, nil
	}

	hasher := sha256.New()
	file, err := os.Open(archivePath)
	if err != nil {
		return VerifyData{}, newOpError(ErrInternal, "打开备份包失败: "+err.Error())
	}
	defer func() { _ = file.Close() }()
	if _, err := io.Copy(hasher, file); err != nil {
		return VerifyData{}, newOpError(ErrInternal, "计算 sha256 失败: "+err.Error())
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	result.SHA256 = sum
	// SHAMatch 保持旧口径：只认 .sha256 sidecar（缺 sidecar 即 false），
	// 而 checks 里的 sha256 优先用 meta 记录、缺失时回退 sidecar。
	result.SHAMatch = sidecar != "" && strings.EqualFold(sidecar, sum)
	recorded := meta.SHA256
	if recorded == "" {
		recorded = sidecar
	}
	if recorded == "" {
		result.Legacy = true
		result.Checks = append(result.Checks, VerifyCheck{
			Name: "sha256", OK: true,
			Detail: "旧备份未记录 sha256，无法校验完整性；已降级为仅检查可读性",
		})
	} else {
		match := strings.EqualFold(recorded, sum)
		detail := "sha256 与记录一致"
		if !match {
			detail = fmt.Sprintf("sha256 与记录不一致：内容被改动或写入不完整（记录 %s…，实际 %s…）",
				shortSHA(recorded), shortSHA(sum))
		}
		result.Checks = append(result.Checks, VerifyCheck{Name: "sha256", OK: match, Detail: detail})
	}
	if meta.SizeBytes > 0 {
		sizeMatch := info.Size() == meta.SizeBytes
		detail := fmt.Sprintf("大小 %d 字节与记录一致", info.Size())
		if info.Size() < meta.SizeBytes {
			detail = fmt.Sprintf("文件不完整（实际 %d 字节 < 记录 %d 字节，疑似被截断）", info.Size(), meta.SizeBytes)
		} else if info.Size() > meta.SizeBytes {
			detail = fmt.Sprintf("文件大小超出记录（实际 %d 字节 > 记录 %d 字节）", info.Size(), meta.SizeBytes)
		}
		result.Checks = append(result.Checks, VerifyCheck{Name: "size_recorded", OK: sizeMatch, Detail: detail})
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return VerifyData{}, newOpError(ErrInternal, "重置读取位置失败: "+err.Error())
	}
	var reader io.Reader = file
	if compression == "gzip" {
		gz, gzErr := gzip.NewReader(file)
		if gzErr != nil {
			result.Corrupted = true
			result.Checks = append(result.Checks, VerifyCheck{
				Name: "readable", OK: false, Detail: "压缩包损坏（gzip 头无法解析）",
			})
			return result, nil
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	entries, uncompressed, broken := scanTarEntries(tar.NewReader(reader))
	result.Entries = entries
	result.UncompressedBytes = uncompressed
	result.Corrupted = broken
	readableDetail := fmt.Sprintf("压缩包可完整读取（%d 个条目）", entries)
	if broken {
		if meta.SizeBytes > 0 && info.Size() < meta.SizeBytes {
			readableDetail = "文件不完整（压缩流在中途结束，疑似截断）"
		} else {
			readableDetail = "压缩包损坏（tar 条目流无法完整读取）"
		}
	}
	result.Checks = append(result.Checks, VerifyCheck{Name: "readable", OK: !broken, Detail: readableDetail})
	if meta.Entries > 0 {
		entriesMatch := entries == meta.Entries
		detail := fmt.Sprintf("条目数 %d 与记录一致", entries)
		if !entriesMatch {
			detail = fmt.Sprintf("条目数与记录不一致（实际 %d，记录 %d）", entries, meta.Entries)
		}
		result.Checks = append(result.Checks, VerifyCheck{Name: "entries", OK: entriesMatch, Detail: detail})
	}

	result.OK = true
	for _, check := range result.Checks {
		if !check.OK {
			result.OK = false
			break
		}
	}
	return result, nil
}

// shortSHA 截取 hash 前 12 位用于人读的错误说明。
func shortSHA(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
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
