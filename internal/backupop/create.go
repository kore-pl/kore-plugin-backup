package backupop

import (
	"encoding/json"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 备份维度：site（站点 / 任意白名单目录，按 paths 显式声明）与 db（MySQL 库）。
// 组合写在 dimensions 里，缺省 ["site"]（向后兼容只传 paths 的老调用方）。
const (
	DimensionSite = "site"
	DimensionDB   = "db"
)

// createParams 是 backup.create 的显式入参形状。
type createParams struct {
	Name        string          `json:"name"`
	Dimensions  []string        `json:"dimensions,omitempty"`
	Paths       []string        `json:"paths,omitempty"`
	Exclude     []string        `json:"exclude,omitempty"`
	Compression string          `json:"compression,omitempty"`
	DB          *createDBParams `json:"db,omitempty"`
}

// createDBParams 是 db 维度的显式入参：只声明库名，连接信息来自插件配置。
type createDBParams struct {
	Database string `json:"database"`
}

// NormalizeDimensions 校验并归一 dimensions：缺省 site、只接受 site/db、去重并按固定顺序返回。
func NormalizeDimensions(raw []string) ([]string, *pluginapi.Error) {
	if len(raw) == 0 {
		return []string{DimensionSite}, nil
	}
	seen := map[string]bool{}
	for _, item := range raw {
		dimension := item
		switch dimension {
		case DimensionSite, DimensionDB:
		default:
			return nil, invalid("dimensions 只支持 site 与 db，收到: " + dimension)
		}
		if seen[dimension] {
			return nil, invalid("dimensions 重复声明: " + dimension)
		}
		seen[dimension] = true
	}
	result := make([]string, 0, 2)
	if seen[DimensionSite] {
		result = append(result, DimensionSite)
	}
	if seen[DimensionDB] {
		result = append(result, DimensionDB)
	}
	return result, nil
}

// hasDimension 判定归一后的维度集合是否包含某项。
func hasDimension(dimensions []string, target string) bool {
	for _, dimension := range dimensions {
		if dimension == target {
			return true
		}
	}
	return false
}

// callCreate 处理 backup.create：校验 name / 维度 / paths 白名单 / exclude / compression，
// 再交给执行面真实打包；db 维度先 mysqldump 再归档。
//
// 每次调用都会落一条作业记录（pending → running → succeeded/failed），
// 进度由执行面按真实条目/字节回调刷新，不使用伪百分比。
func callCreate(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p createParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	name, perr := ValidateName(p.Name)
	if perr != nil {
		return nil, perr
	}
	dimensions, perr := NormalizeDimensions(p.Dimensions)
	if perr != nil {
		return nil, perr
	}
	paths := make([]string, 0, len(p.Paths))
	if hasDimension(dimensions, DimensionSite) {
		if len(p.Paths) == 0 {
			return nil, invalid("site 维度的 paths 不能为空")
		}
		for _, rawPath := range p.Paths {
			abs, perr := ResolveBackupPath(rawPath, opts.AllowRoots)
			if perr != nil {
				return nil, perr
			}
			paths = append(paths, abs)
		}
	} else if len(p.Paths) > 0 {
		return nil, invalid("dimensions 不含 site 时不能传 paths")
	}
	if perr := ValidateExclude(p.Exclude); perr != nil {
		return nil, perr
	}
	compression, perr := NormalizeCompression(p.Compression)
	if perr != nil {
		return nil, perr
	}
	var db *DBSpec
	if hasDimension(dimensions, DimensionDB) {
		if p.DB == nil {
			return nil, invalid("db 维度需要 db.database")
		}
		database, perr := validateDatabaseName(p.DB.Database)
		if perr != nil {
			return nil, perr
		}
		if perr := ValidateMySQLConfig(opts.DB); perr != nil {
			return nil, perr
		}
		db = &DBSpec{Database: database}
	} else if p.DB != nil {
		return nil, invalid("dimensions 不含 db 时不能传 db")
	}

	tracker := newJobTracker(stateStoreFor(exec), opts.Dir, name, dimensions)
	tracker.markRunning()

	started := time.Now()
	data, err := exec.Create(CreateSpec{
		Dir:         opts.Dir,
		Name:        name,
		Paths:       paths,
		Exclude:     p.Exclude,
		Compression: compression,
		MaxBytes:    opts.MaxBytes,
		MaxEntries:  opts.MaxEntries,
		Dimensions:  dimensions,
		DB:          db,
		DBConfig:    opts.DB,
		Progress:    tracker.progress,
	})
	if err != nil {
		tracker.finish(CreateData{}, err)
		return nil, classify(err)
	}
	data.DurationMS = elapsedMS(started)
	ensureStringSlices(&data.Skipped, &data.Warnings)
	if data.CreatedAt == "" {
		data.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if len(data.Dimensions) == 0 {
		data.Dimensions = dimensions
	}
	if data.Stages == nil {
		data.Stages = []JobStage{}
	}
	tracker.finish(data, nil)
	record := tracker.snapshot()
	data.Job = &record
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
