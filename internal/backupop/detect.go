package backupop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 备份源自动探测：产品易用性原则②「自动发现，不让人填环境细节」在备份页的落地。
//
// 口径与 preset.go 的 existingAllowed 完全一致，不新造探测手段：
//   - 只读：只用 os.Lstat / os.ReadDir 看"本机到底有什么"，不装包、不改配置、不猜路径；
//   - 不强依赖 root：读不了（EPERM/EACCES）就跳过该项，不报错、不阻塞页面；
//   - 白名单内才预填：只有「允许的根路径」之内的目录才会进 suggested_paths（否则用户点了创建
//     也会撞白名单校验）；白名单外的照样如实报告 in_roots=false，由页面给"加入允许根路径"；
//   - 有界：每个容器最多枚举 maxDetectChildren 个子目录、总数 maxDetectedSources，
//     不递归、不遍历整个磁盘，一次调用是毫秒级。
//
// 数据目录（/var/lib/mysql 等）只报告不预填：直接拷数据文件得不到一致的备份，
// 正确做法是用「数据库备份」预设做 mysqldump（见 Note）。

const (
	// maxDetectChildren 是单个容器目录最多枚举的子目录数。
	maxDetectChildren = 12
	// maxDetectedSources 是一次探测最多返回的条目数（含白名单外与数据目录）。
	maxDetectedSources = 32
)

// 探测候选。容器会被枚举一层子目录（站点根/应用目录的真实粒度在这里），
// 固定路径只判断自身是否存在。
var (
	// detectSiteContainers 是常见站点根：子目录通常就是一个站点。
	detectSiteContainers = []string{"/var/www", "/srv/www"}
	// detectSitePaths 是不需要枚举的常见站点目录。
	detectSitePaths = []string{"/var/www/html", "/usr/share/nginx/html"}
	// detectConfigPaths 是关键配置目录（与 preset.go 的 autoConfigCandidates 同源）。
	detectConfigPaths = []string{"/etc/nginx", "/etc/kore"}
	// detectAppContainers 是应用部署目录：子目录通常是一个应用。
	detectAppContainers = []string{"/opt"}
	// detectDataPaths 是数据库数据目录：只报告、不预填（见 Note）。
	detectDataPaths = []string{"/var/lib/mysql", "/var/lib/postgresql", "/var/lib/redis"}
)

// detectKind 是探测条目的类别（前端据此分组与给建议）。
const (
	DetectKindSite   = "site"
	DetectKindConfig = "config"
	DetectKindApp    = "app"
	DetectKindData   = "data"
)

// detectKindLabels 是类别到用户可读标签的映射（原则④：不暴露内部术语）。
var detectKindLabels = map[string]string{
	DetectKindSite:   "网站目录",
	DetectKindConfig: "配置目录",
	DetectKindApp:    "应用目录",
	DetectKindData:   "数据目录",
}

// DetectedSource 是一条探测结果。字段全部显式（G-24）。
type DetectedSource struct {
	// Path 是绝对路径（已 Clean）。
	Path string `json:"path"`
	// Kind 是类别：site / config / app / data。
	Kind string `json:"kind"`
	// Label 是该类别的中文标签（网站目录 / 配置目录 / 应用目录 / 数据目录）。
	Label string `json:"label"`
	// InRoots 表示该路径是否在「允许的根路径」之内（false 时不可直接用作备份源）。
	InRoots bool `json:"in_roots"`
	// Usable 表示可以直接填进备份源（InRoots 且不是数据目录）。
	Usable bool `json:"usable"`
	// Hint 是该项自己的处置建议（目前只有数据目录需要）。
	Hint string `json:"hint,omitempty"`
}

// SourcesDetectResult 是 backup.sources.detect 的结构化结果。
type SourcesDetectResult struct {
	// AllowRoots 是本次探测使用的允许根路径白名单（页面据此解释为什么某项不可用）。
	AllowRoots []string `json:"allow_roots"`
	// Sources 是全部探测到的条目（含白名单外与数据目录）。
	Sources []DetectedSource `json:"sources"`
	// SuggestedPaths 是可直接填进备份源的路径（白名单内、非数据目录、已去重折叠）。
	SuggestedPaths []string `json:"suggested_paths"`
	// Checked 是本次看过的候选路径（"怎么办"文案里如实列出来，避免"未检测到"没有下文）。
	Checked []string `json:"checked"`
	// DetectedAt 是探测时间（RFC3339，UTC）。
	DetectedAt string `json:"detected_at"`
	// Hint 是"探测不到可备份目录"时的下一步（有结果时为空）。
	Hint string `json:"hint,omitempty"`
	// Note 是数据目录的一般性提醒（探测到数据库数据目录时出现）。
	Note string `json:"note,omitempty"`
}

// detectCandidate 是探测用的内部形状：容器点名 + 是否枚举子目录。
type detectCandidate struct {
	Paths        []string
	Kind         string
	ListChildren bool
}

// detectCandidates 返回按优先级排列的候选（站点 → 应用 → 配置 → 数据）。
func detectCandidates() []detectCandidate {
	return []detectCandidate{
		{Paths: detectSiteContainers, Kind: DetectKindSite, ListChildren: true},
		{Paths: detectSitePaths, Kind: DetectKindSite},
		{Paths: detectAppContainers, Kind: DetectKindApp, ListChildren: true},
		{Paths: detectConfigPaths, Kind: DetectKindConfig},
		{Paths: detectDataPaths, Kind: DetectKindData},
	}
}

// callSourcesDetect 处理 backup.sources.detect：只读探测本机常见备份源。
func callSourcesDetect(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var params struct{}
	if perr := decodeParams(raw, &params); perr != nil {
		return nil, perr
	}
	// 空 params 与 {"refresh":true} 都接受：探测量级是"几十次 stat"，不需要缓存层。
	result := DetectSources(opts.AllowRoots, time.Now())
	return encode(result)
}

// DetectSources 执行一次只读探测，返回可直接展示的结果。now 由调用方传入（可测）。
func DetectSources(allowRoots []string, now time.Time) SourcesDetectResult {
	roots := append([]string(nil), allowRoots...)
	if len(roots) == 0 {
		roots = append([]string(nil), DefaultAllowRoots...)
	}
	result := SourcesDetectResult{
		AllowRoots:     roots,
		Sources:        []DetectedSource{},
		SuggestedPaths: []string{},
		Checked:        []string{},
		DetectedAt:     now.UTC().Format(time.RFC3339),
	}
	seen := map[string]struct{}{}
	add := func(path, kind string, inRoots, usable, dataHint bool) {
		clean := filepath.Clean(path)
		if _, dup := seen[clean]; dup {
			return
		}
		seen[clean] = struct{}{}
		source := DetectedSource{
			Path:    clean,
			Kind:    kind,
			Label:   detectKindLabels[kind],
			InRoots: inRoots,
			Usable:  usable,
		}
		if dataHint {
			source.Hint = "数据库正在写入时直接拷数据文件会得到不一致的备份；请改用「数据库备份」预设导出（mysqldump），需要停库快照时先停库再备份。"
		}
		result.Sources = append(result.Sources, source)
	}
	for _, candidate := range detectCandidates() {
		for _, path := range candidate.Paths {
			clean := filepath.Clean(path)
			result.Checked = append(result.Checked, clean)
			info, err := os.Lstat(clean)
			if err != nil || !info.IsDir() {
				continue
			}
			if isCriticalDir(clean) {
				continue
			}
			inRoots := underAnyRoot(clean, roots)
			switch candidate.Kind {
			case DetectKindData:
				// 数据目录只报告：即便在白名单内也不预填（见文件头 Note）。
				add(clean, candidate.Kind, inRoots, false, true)
				continue
			}
			children := []string{}
			if candidate.ListChildren {
				children = existingSubdirs(clean, roots)
			}
			if len(children) > 0 {
				for _, child := range children {
					childInRoots := underAnyRoot(child, roots)
					add(child, candidate.Kind, childInRoots, childInRoots, false)
				}
				continue
			}
			add(clean, candidate.Kind, inRoots, inRoots, false)
		}
	}
	if len(result.Sources) > maxDetectedSources {
		result.Sources = result.Sources[:maxDetectedSources]
	}
	result.SuggestedPaths = suggestedPaths(result.Sources, roots)
	if len(result.SuggestedPaths) == 0 {
		result.Hint = "本机没找到可备份的目录。怎么办：①先建站点（或在允许的根路径下放要备份的数据），再点「重新检测」；" +
			"②数据放在别处，就到「备份设置 → 允许的根路径」把那个目录加进来；③也可以直接在下面手填绝对路径。"
	}
	for _, source := range result.Sources {
		if source.Kind == DetectKindData && source.InRoots {
			result.Note = "检测到数据库数据目录，但它不适合直接文件备份：请用「数据库备份」预设做一致性导出。"
			break
		}
	}
	return result
}

// existingSubdirs 返回容器目录下真实存在、在白名单内、非关键目录的子目录（按名字排序，
// 上限 maxDetectChildren）。只列能直接用的子目录：白名单外的目录逐个铺出来只会变成噪声
// （/opt 下十几个应用中只有一个可备份时尤其明显），改为由调用方退回"容器目录自身"并标注
// in_roots=false，页面给一句"加入允许根路径"即可：加入后重新探测自然会列出子目录。
// 读不了目录（权限不足）时同样返回空。
func existingSubdirs(container string, roots []string) []string {
	entries, err := os.ReadDir(container)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if len(out) >= maxDetectChildren {
			break
		}
		path := filepath.Join(container, name)
		if !underAnyRoot(path, roots) || isCriticalDir(path) {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		out = append(out, path)
	}
	return out
}

// suggestedPaths 从探测结果里挑出可直接填进备份源的路径：白名单内、可用的，
// 去掉被父路径覆盖的子路径（父目录已选时再备份子目录是重复打包）。
func suggestedPaths(sources []DetectedSource, roots []string) []string {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		if !source.Usable || !source.InRoots || !underAnyRoot(source.Path, roots) {
			continue
		}
		if coveredBy(out, source.Path) {
			continue
		}
		out = append(out, source.Path)
	}
	return out
}
