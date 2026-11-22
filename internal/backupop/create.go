package backupop

import (
	"encoding/json"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// callCreate 处理 backup.create：校验目标路径白名单、后端与 retention 天数后创建备份作业。
// 骨架口径只做参数校验与执行面调用，不落盘、不上传任何对象。
func callCreate(raw json.RawMessage, root string, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p createParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	target, perr := ValidateTarget(root, p.Target)
	if perr != nil {
		return nil, perr
	}
	backend, perr := ValidateBackend(p.Backend)
	if perr != nil {
		return nil, perr
	}
	if perr := ValidateRetention(p.RetentionDays); perr != nil {
		return nil, perr
	}
	applied, err := exec.Create(root, target, backend, p.RetentionDays)
	if err != nil {
		return nil, classify(err)
	}
	return encode(CreateResult{
		Target:        target,
		Backend:       backend,
		RetentionDays: p.RetentionDays,
		Applied:       applied,
	})
}
