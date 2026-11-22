package backupop

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// deleteParams 是 backup.delete 的显式入参形状。
type deleteParams struct {
	ID string `json:"id"`
}

// callDelete 处理 backup.delete：删除备份包与两个 sidecar，不存在返回 E_NOTFOUND。
func callDelete(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p deleteParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	id, perr := ValidateID(p.ID)
	if perr != nil {
		return nil, perr
	}
	started := time.Now()
	if err := exec.Delete(opts.Dir, id); err != nil {
		return nil, classify(err)
	}
	return encode(DeleteResult{ID: id, Deleted: true, DurationMS: elapsedMS(started)})
}

// Delete 删除包与 sidecar；包不存在时返回 notfound，任一删除失败归 internal。
func (realFS) Delete(dir, id string) error {
	archivePath, _, err := resolveArchive(dir, id)
	if err != nil {
		return err
	}
	targets := []string{archivePath, archivePath + ".sha256", archivePath + ".meta.json"}
	for _, target := range targets {
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return newOpError(ErrInternal, "删除备份文件失败: "+err.Error())
		}
	}
	return nil
}
