package backupop

import (
	"encoding/json"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// createParams 是 backup.create 的显式入参形状。
type createParams struct {
	Name        string   `json:"name"`
	Paths       []string `json:"paths"`
	Exclude     []string `json:"exclude"`
	Compression string   `json:"compression"`
}

// callCreate 处理 backup.create：校验 name / paths 白名单 / exclude / compression，
// 再交给执行面真实打包。路径越界、关键目录、非法 glob 一律 E_INVALID。
func callCreate(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p createParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	name, perr := ValidateName(p.Name)
	if perr != nil {
		return nil, perr
	}
	if len(p.Paths) == 0 {
		return nil, invalid("paths 不能为空")
	}
	paths := make([]string, 0, len(p.Paths))
	for _, rawPath := range p.Paths {
		abs, perr := ResolveBackupPath(rawPath, opts.AllowRoots)
		if perr != nil {
			return nil, perr
		}
		paths = append(paths, abs)
	}
	if perr := ValidateExclude(p.Exclude); perr != nil {
		return nil, perr
	}
	compression, perr := NormalizeCompression(p.Compression)
	if perr != nil {
		return nil, perr
	}

	started := time.Now()
	data, err := exec.Create(CreateSpec{
		Dir:         opts.Dir,
		Name:        name,
		Paths:       paths,
		Exclude:     p.Exclude,
		Compression: compression,
		MaxBytes:    opts.MaxBytes,
		MaxEntries:  opts.MaxEntries,
	})
	if err != nil {
		return nil, classify(err)
	}
	data.DurationMS = elapsedMS(started)
	ensureStringSlices(&data.Skipped, &data.Warnings)
	if data.CreatedAt == "" {
		data.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return encode(data)
}

// ensureStringSlices 把 nil 切片替换为空切片，保证 JSON 输出是 [] 而不是 null。
func ensureStringSlices(items ...*[]string) {
	for _, item := range items {
		if *item == nil {
			*item = []string{}
		}
	}
}
