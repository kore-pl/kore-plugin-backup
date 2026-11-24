package backupop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 预设是"预设优先、自定义兜底"（产品原则 §7①）在备份插件上的落地：
//
//   - 零参数 backup.create（`{}`）按 defaultPresetID 展开成一份真实备份，小白一次点击即可；
//   - 显式传 preset 时只补齐未填字段（预设有契约，自定义字段仍然生效）；
//   - 只要调用方自己传了任何业务字段、又没给 preset，就保持严格校验——
//     漏传 paths 不静默变成另一份备份（静默成功会产出误导性备份）。
//
// 预设不写任何隐式 exclude：备份范围必须可预期，"以为备了却没备"比多备更危险。
const (
	// PresetSiteFull 是默认预设：网站全量备份。
	PresetSiteFull = "site-full"
	// PresetConfig 是配置文件备份。
	PresetConfig = "config"
	// PresetDB 是数据库备份（需要调用方指定 db.database）。
	PresetDB = "db"
	// PresetFull 是整机关键数据：站点目录 + 关键配置。
	PresetFull = "full"
)

// defaultPresetID 是零参数 create 使用的预设；置空即关闭零参数预设（测试反例用）。
var defaultPresetID = PresetSiteFull

// 自动发现的候选目录：只取"确实存在 + 在白名单内"的项。
// 站点目录优先，其后是关键配置；/etc/nginx 只有在白名单包含 /etc 时才会被选中。
var (
	autoSiteCandidates   = []string{"/var/www", "/srv/www", "/var/www/html"}
	autoConfigCandidates = []string{"/etc/nginx", "/etc/kore", "/etc/systemd/system/kore.service"}
)

// backupPresetDef 是一个备份预设的定义；pathsFn 在调用期按白名单做真实探测。
type backupPresetDef struct {
	ID          string
	Label       string
	Description string
	Dimensions  []string
	Compression string
	// pathsFn 返回该预设要备份的路径（已存在且在 allowed 内）；db 预设返回 nil。
	pathsFn func(allowed []string) []string
	// requiresDatabase 为真时调用方必须补 db.database。
	requiresDatabase bool
}

// backupPresets 是预设注册表（顺序即前端展示顺序）。
var backupPresets = []backupPresetDef{
	{
		ID:          PresetSiteFull,
		Label:       "网站全量备份",
		Description: "备份所有站点目录（自动发现 /var/www、/srv/www 等已存在的目录）",
		Dimensions:  []string{DimensionSite},
		Compression: "gzip",
		pathsFn:     autoSitePaths,
	},
	{
		ID:          PresetConfig,
		Label:       "配置文件备份",
		Description: "备份 nginx / kore 等关键配置目录；改配置前先打一份，出事能回滚",
		Dimensions:  []string{DimensionSite},
		Compression: "gzip",
		pathsFn:     autoConfigPaths,
	},
	{
		ID:               PresetDB,
		Label:            "数据库备份",
		Description:      "导出一个 MySQL 库并打进归档（需要选择库名）",
		Dimensions:       []string{DimensionDB},
		Compression:      "gzip",
		requiresDatabase: true,
	},
	{
		ID:          PresetFull,
		Label:       "整机关键数据",
		Description: "站点目录 + 关键配置一起备份；数据库请单独选「数据库备份」预设",
		Dimensions:  []string{DimensionSite},
		Compression: "gzip",
		pathsFn:     autoFullPaths,
	},
}

// retentionPresetDef 是保留策略预设：keep 份数 + 推荐执行频率 + 依据。
type retentionPresetDef struct {
	ID        string
	Label     string
	Keep      int
	Schedule  string
	Rationale string
}

// retentionPresets 是保留策略预设（业界惯例 + 磁盘成本）。
var retentionPresets = []retentionPresetDef{
	{
		ID:        "daily7",
		Label:     "每日 7 天",
		Keep:      7,
		Schedule:  "0 3 * * *",
		Rationale: "日备保留 7 份是小型站点的惯例：RPO ≤24h，磁盘占用约 7×单份体积",
	},
	{
		ID:        "weekly4",
		Label:     "每周 4 周",
		Keep:      4,
		Schedule:  "0 2 * * 0",
		Rationale: "周备保留 4 份覆盖一个月：适合变化少、磁盘紧的站点，占用约 4×单份体积",
	},
	{
		ID:        "monthly12",
		Label:     "每月 12 月",
		Keep:      12,
		Schedule:  "0 3 1 * *",
		Rationale: "月备保留 12 份覆盖一年：用于合规留档与季度回看，占用约 12×单份体积",
	},
}

// PresetField 描述预设展开后仍需用户补的字段。
type PresetField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Help     string `json:"help"`
}

// PresetExpanded 是预设在本机的展开结果：前端展示或直接作为 create 参数。
type PresetExpanded struct {
	Name             string   `json:"name"`
	Dimensions       []string `json:"dimensions"`
	Paths            []string `json:"paths"`
	Exclude          []string `json:"exclude"`
	Compression      string   `json:"compression"`
	RequiresDatabase bool     `json:"requires_database"`
}

// PresetInfo 是 backup.presets 返回的一条备份预设。
type PresetInfo struct {
	ID          string         `json:"id"`
	Label       string         `json:"label"`
	Description string         `json:"description"`
	Fields      []PresetField  `json:"fields"`
	Expanded    PresetExpanded `json:"expanded"`
}

// RetentionPresetInfo 是 backup.presets 返回的一条保留策略预设。
type RetentionPresetInfo struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Keep      int    `json:"keep"`
	Schedule  string `json:"schedule"`
	Rationale string `json:"rationale"`
}

// PresetsResult 是 backup.presets 的结构化结果。
type PresetsResult struct {
	DefaultPreset    string                `json:"default_preset"`
	Presets          []PresetInfo          `json:"presets"`
	RetentionPresets []RetentionPresetInfo `json:"retention_presets"`
	Hint             string                `json:"hint"`
}

// presetExpansion 是预设展开后用于 merge 进 create 参数的形状。
type presetExpansion struct {
	Name             string
	Dimensions       []string
	Paths            []string
	Exclude          []string
	Compression      string
	RequiresDatabase bool
}

// findBackupPreset 返回 id 对应的预设；未知 id 返回 nil。
func findBackupPreset(id string) *backupPresetDef {
	for index := range backupPresets {
		if backupPresets[index].ID == id {
			return &backupPresets[index]
		}
	}
	return nil
}

// findRetentionPreset 返回 id 对应的保留策略预设；未知 id 返回 nil。
func findRetentionPreset(id string) *retentionPresetDef {
	for index := range retentionPresets {
		if retentionPresets[index].ID == id {
			return &retentionPresets[index]
		}
	}
	return nil
}

// expandBackupPreset 按白名单探测本机路径并展开预设；路径为空时给出可操作提示。
func expandBackupPreset(id string, allowed []string) (presetExpansion, *pluginapi.Error) {
	preset := findBackupPreset(id)
	if preset == nil {
		return presetExpansion{}, invalid("未知备份预设: " + id + "；可用: " + presetIDs())
	}
	expansion := presetExpansion{
		Name:             id,
		Dimensions:       append([]string(nil), preset.Dimensions...),
		Exclude:          []string{},
		Compression:      preset.Compression,
		RequiresDatabase: preset.requiresDatabase,
	}
	if expansion.Compression == "" {
		expansion.Compression = "gzip"
	}
	if preset.pathsFn != nil {
		expansion.Paths = preset.pathsFn(allowed)
		if len(expansion.Paths) == 0 {
			return presetExpansion{}, invalid("预设「" + preset.Label + "」在本机没有找到可备份的目录（已检查 " +
				strings.Join(preset.autoHint(), "、") + "）；如果站点在别的目录，请在「备份设置 → 允许的根路径」里加上它，或显式传 paths")
		}
	}
	if expansion.Paths == nil {
		expansion.Paths = []string{}
	}
	return expansion, nil
}

// autoHint 返回该预设置信探测过的候选项（用于错误提示）。
func (preset *backupPresetDef) autoHint() []string {
	if preset.ID == PresetFull {
		return append(append([]string(nil), autoSiteCandidates...), autoConfigCandidates...)
	}
	if preset.ID == PresetConfig {
		return autoConfigCandidates
	}
	return autoSiteCandidates
}

// presetLabel 返回预设展示名（错误提示用）；未知 id 原样返回。
func presetLabel(id string) string {
	if preset := findBackupPreset(id); preset != nil {
		return preset.Label
	}
	return id
}

// presetIDs 返回全部备份预设 id（错误提示用）。
func presetIDs() string {
	ids := make([]string, 0, len(backupPresets))
	for _, preset := range backupPresets {
		ids = append(ids, preset.ID)
	}
	return strings.Join(ids, " / ")
}

// autoSitePaths 探测白名单内已存在的站点目录；一个都没有时退回"已存在的白名单根本身"。
func autoSitePaths(allowed []string) []string {
	return existingAllowed(autoSiteCandidates, allowed, true)
}

// autoConfigPaths 探测白名单内已存在的关键配置目录。
func autoConfigPaths(allowed []string) []string {
	return existingAllowed(autoConfigCandidates, allowed, false)
}

// autoFullPaths 合并站点与关键配置（父目录已选中时跳过其子路径）。
func autoFullPaths(allowed []string) []string {
	return existingAllowed(append(append([]string(nil), autoSiteCandidates...), autoConfigCandidates...), allowed, true)
}

// existingAllowed 过滤候选：存在、非系统关键目录、在白名单内，并去掉已被父路径覆盖的子路径。
// fallbackToRoots=true 且候选全空时，退回白名单里已存在的根目录本身
// （排除 / 与 /home：整份 home 不是"一键备份"的合理默认）。
func existingAllowed(candidates, allowed []string, fallbackToRoots bool) []string {
	seen := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if !underAnyRoot(candidate, allowed) || isCriticalDir(candidate) {
			continue
		}
		if _, err := os.Lstat(candidate); err != nil {
			continue
		}
		if coveredBy(seen, candidate) {
			continue
		}
		seen = append(seen, candidate)
	}
	if len(seen) > 0 || !fallbackToRoots {
		return seen
	}
	for _, root := range allowed {
		clean := filepath.Clean(root)
		if clean == "/" || clean == "/home" || isCriticalDir(clean) {
			continue
		}
		if _, err := os.Lstat(clean); err != nil {
			continue
		}
		if coveredBy(seen, clean) {
			continue
		}
		seen = append(seen, clean)
	}
	return seen
}

// coveredBy 判定 candidate 是否已被 selected 里的某个路径包含（自身或子路径）。
func coveredBy(selected []string, candidate string) bool {
	for _, existing := range selected {
		if withinPath(existing, candidate) {
			return true
		}
	}
	return false
}

// isZeroCreateParams 判定调用方是否"什么都没填"（零参数一键路径）。
// 只要显式给了任何业务字段，就按严格校验，不静默套默认预设。
func isZeroCreateParams(p createParams) bool {
	return strings.TrimSpace(p.Preset) == "" &&
		strings.TrimSpace(p.Name) == "" &&
		len(p.Dimensions) == 0 &&
		len(p.Paths) == 0 &&
		len(p.Exclude) == 0 &&
		strings.TrimSpace(p.Compression) == "" &&
		p.DB == nil
}

// applyPreset 把预设展开结果 merge 进参数：只补空字段，显式传入的字段优先。
func applyPreset(p *createParams, expansion presetExpansion) {
	if strings.TrimSpace(p.Name) == "" {
		p.Name = expansion.Name
	}
	if len(p.Dimensions) == 0 {
		p.Dimensions = expansion.Dimensions
	}
	if len(p.Paths) == 0 {
		p.Paths = expansion.Paths
	}
	if len(p.Exclude) == 0 {
		p.Exclude = expansion.Exclude
	}
	if strings.TrimSpace(p.Compression) == "" {
		p.Compression = expansion.Compression
	}
}

// callPresets 处理 backup.presets：返回备份预设、保留策略预设与本机展开结果。
func callPresets(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p struct{}
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	result := PresetsResult{
		DefaultPreset:    defaultPresetID,
		Presets:          make([]PresetInfo, 0, len(backupPresets)),
		RetentionPresets: make([]RetentionPresetInfo, 0, len(retentionPresets)),
		Hint:             "不传任何参数调用 backup.create 会按默认预设备份；也可以传 preset=<id> 展开预设，再覆盖 name/paths 等字段",
	}
	for _, preset := range backupPresets {
		expansion, perr := expandBackupPreset(preset.ID, opts.AllowRoots)
		if perr != nil && preset.requiresDatabase {
			// db 预设不需要 paths，展开失败只可能来自未知 id；这里兜底成空路径。
			expansion = presetExpansion{Dimensions: preset.Dimensions, Exclude: []string{}, Compression: "gzip", RequiresDatabase: true}
			perr = nil
		}
		if perr != nil {
			// 本机没有可备份目录时不隐藏预设，只把展开结果留空，由前端给提示。
			expansion = presetExpansion{Dimensions: preset.Dimensions, Exclude: []string{}, Compression: "gzip"}
		}
		info := PresetInfo{
			ID:          preset.ID,
			Label:       preset.Label,
			Description: preset.Description,
			Fields:      []PresetField{},
			Expanded: PresetExpanded{
				Name:             preset.ID,
				Dimensions:       append([]string(nil), preset.Dimensions...),
				Paths:            expansion.Paths,
				Exclude:          []string{},
				Compression:      "gzip",
				RequiresDatabase: preset.requiresDatabase,
			},
		}
		if preset.requiresDatabase {
			info.Fields = []PresetField{{
				Key: "db.database", Label: "数据库名", Type: "string", Required: true,
				Help: "要导出的 MySQL 库名；连接信息来自插件配置",
			}}
		}
		result.Presets = append(result.Presets, info)
	}
	for _, preset := range retentionPresets {
		result.RetentionPresets = append(result.RetentionPresets, RetentionPresetInfo{
			ID: preset.ID, Label: preset.Label, Keep: preset.Keep,
			Schedule: preset.Schedule, Rationale: preset.Rationale,
		})
	}
	return encode(result)
}
