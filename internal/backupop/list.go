package backupop

import (
	"encoding/json"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// callList 处理 backup.list：扫描备份目录并附带 sidecar 元信息。
func callList(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p struct{}
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	started := time.Now()
	backups, err := exec.List(opts.Dir)
	if err != nil {
		return nil, classify(err)
	}
	if backups == nil {
		backups = []BackupInfo{}
	}
	return encode(ListResult{Backups: backups, Count: len(backups), DurationMS: elapsedMS(started)})
}
