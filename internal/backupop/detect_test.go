package backupop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withDetectCandidates 把包级候选表临时换成测试用的临时目录（用例内串行，不并发）。
func withDetectCandidates(t *testing.T, siteContainers, sitePaths, configPaths, appContainers, dataPaths []string) {
	t.Helper()
	oldSiteContainers, oldSitePaths := detectSiteContainers, detectSitePaths
	oldConfigPaths, oldAppContainers, oldDataPaths := detectConfigPaths, detectAppContainers, detectDataPaths
	detectSiteContainers, detectSitePaths, detectConfigPaths, detectAppContainers, detectDataPaths =
		siteContainers, sitePaths, configPaths, appContainers, dataPaths
	t.Cleanup(func() {
		detectSiteContainers, detectSitePaths, detectConfigPaths = oldSiteContainers, oldSitePaths, oldConfigPaths
		detectAppContainers, detectDataPaths = oldAppContainers, oldDataPaths
	})
}

// mkdirs 在 root 下建目录并返回 root。
func mkdirs(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatalf("建目录 %s: %v", rel, err)
		}
	}
}

// findSource 返回 path 对应的探测条目；不存在返回 nil。
func findSource(sources []DetectedSource, path string) *DetectedSource {
	for i := range sources {
		if sources[i].Path == path {
			return &sources[i]
		}
	}
	return nil
}

// TestDetectSourcesFindsRealSiteDirs 断言"打开备份页就有东西可填"：
// 站点根存在时枚举出一层真实站点目录，且都在白名单内可直接预填。
func TestDetectSourcesFindsRealSiteDirs(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "sites/demo.test", "sites/blog.test")
	withDetectCandidates(t, []string{filepath.Join(root, "sites")}, nil, nil, nil, nil)

	result := DetectSources([]string{root}, time.Unix(0, 0))
	if len(result.Sources) != 2 {
		t.Fatalf("探测到 %d 条，期望 2 条（两个站点目录）：%+v", len(result.Sources), result.Sources)
	}
	for _, name := range []string{"demo.test", "blog.test"} {
		path := filepath.Join(root, "sites", name)
		source := findSource(result.Sources, path)
		if source == nil {
			t.Fatalf("缺少站点目录 %s：%+v", path, result.Sources)
		}
		if source.Kind != DetectKindSite || source.Label != "网站目录" {
			t.Fatalf("%s 类别=%q/%q，期望 site/网站目录", path, source.Kind, source.Label)
		}
		if !source.InRoots || !source.Usable {
			t.Fatalf("%s in_roots=%v usable=%v，期望都为 true", path, source.InRoots, source.Usable)
		}
	}
	if len(result.SuggestedPaths) != 2 {
		t.Fatalf("suggested_paths=%v，期望两个站点目录", result.SuggestedPaths)
	}
	if result.Hint != "" {
		t.Fatalf("有结果时不应给 hint，实际 %q", result.Hint)
	}
}

// TestDetectSourcesOutsideRootsNotSuggested 断言白名单外的目录只报告、不预填：
// 预填一个点了创建必定失败的路径比不填更糟。
func TestDetectSourcesOutsideRootsNotSuggested(t *testing.T) {
	inside, outside := t.TempDir(), t.TempDir()
	mkdirs(t, outside, "srv-site", "blog-site")
	withDetectCandidates(t, []string{outside}, nil, nil, nil, nil)

	result := DetectSources([]string{inside}, time.Unix(0, 0))
	// 白名单外的容器目录只报容器一级（不逐个铺子目录），标记 in_roots=false 供页面给"加入"。
	if len(result.Sources) != 1 {
		t.Fatalf("探测到 %d 条，期望只报容器目录一级：%+v", len(result.Sources), result.Sources)
	}
	source := findSource(result.Sources, outside)
	if source == nil {
		t.Fatalf("白名单外目录应被如实报告：%+v", result.Sources)
	}
	if source.InRoots || source.Usable {
		t.Fatalf("白名单外目录 in_roots=%v usable=%v，期望都为 false", source.InRoots, source.Usable)
	}
	if len(result.SuggestedPaths) != 0 {
		t.Fatalf("suggested_paths=%v，期望为空（白名单外不可预填）", result.SuggestedPaths)
	}
	if result.Hint == "" {
		t.Fatal("探测不到可预填目录时必须给「怎么办」")
	}
}

// TestDetectSourcesDataDirNeverSuggested 断言数据库数据目录只报告不预填，并给一致性导出建议。
func TestDetectSourcesDataDirNeverSuggested(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "mysql")
	withDetectCandidates(t, nil, nil, nil, nil, []string{filepath.Join(root, "mysql")})

	result := DetectSources([]string{root}, time.Unix(0, 0))
	source := findSource(result.Sources, filepath.Join(root, "mysql"))
	if source == nil {
		t.Fatalf("数据目录应被报告：%+v", result.Sources)
	}
	if source.Usable {
		t.Fatal("数据目录不得标为可用备份源")
	}
	if source.Kind != DetectKindData || source.Hint == "" {
		t.Fatalf("数据目录 kind=%q hint=%q，期望 data + 非空提示", source.Kind, source.Hint)
	}
	if len(result.SuggestedPaths) != 0 || result.Hint == "" {
		t.Fatalf("suggested_paths=%v hint=%q，期望空建议 + 怎么办", result.SuggestedPaths, result.Hint)
	}
	if result.Note == "" {
		t.Fatal("检测到白名单内数据目录时应给一致性导出提醒")
	}
}

// TestDetectSourcesEmptyContainerFallsBackToItself 断言容器目录为空时退回容器本身，
// 不出现"目录存在却什么都不给"的空窗。
func TestDetectSourcesEmptyContainerFallsBackToItself(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "www")
	withDetectCandidates(t, []string{filepath.Join(root, "www")}, nil, nil, nil, nil)

	result := DetectSources([]string{root}, time.Unix(0, 0))
	if len(result.SuggestedPaths) != 1 || result.SuggestedPaths[0] != filepath.Join(root, "www") {
		t.Fatalf("suggested_paths=%v，期望容器目录自身", result.SuggestedPaths)
	}
}

// TestDetectSourcesDedupAndSkipCoveredChildren 断言同一路径不重复、父目录已选时不重复列子目录。
func TestDetectSourcesDedupAndSkipCoveredChildren(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "www/html", "www/html/inner")
	withDetectCandidates(t,
		[]string{filepath.Join(root, "www")},
		[]string{filepath.Join(root, "www", "html")},
		nil, nil, nil)

	result := DetectSources([]string{root}, time.Unix(0, 0))
	counts := map[string]int{}
	for _, path := range result.SuggestedPaths {
		counts[path]++
	}
	for path, count := range counts {
		if count > 1 {
			t.Fatalf("%s 在 suggested_paths 里出现 %d 次", path, count)
		}
	}
	// 容器枚举出 www/html（唯一子目录），固定候选 www/html 被去重，inner 不枚举（只一层）。
	if len(result.SuggestedPaths) != 1 || result.SuggestedPaths[0] != filepath.Join(root, "www", "html") {
		t.Fatalf("suggested_paths=%v，期望只有 %s", result.SuggestedPaths, filepath.Join(root, "www", "html"))
	}
}

// TestCallSourcesDetectIsReadOnlyProtocol 断言方法契约：走 Call 分派、结果可解析、
// 只读路径写不出任何东西（探测前后目录内容一致）。
func TestCallSourcesDetectIsReadOnlyProtocol(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "backups", "www/site-a")
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	withDetectCandidates(t, []string{filepath.Join(root, "www")}, nil, nil, nil, nil)

	raw, perr := Call("backup.sources.detect", nil, Options{Dir: filepath.Join(root, "backups"), AllowRoots: []string{root}}, nil)
	if perr != nil {
		t.Fatalf("backup.sources.detect 失败: %s %s", perr.Code, perr.Message)
	}
	var result SourcesDetectResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析结果: %v", err)
	}
	if len(result.SuggestedPaths) != 1 || result.SuggestedPaths[0] != filepath.Join(root, "www", "site-a") {
		t.Fatalf("suggested_paths=%v，期望 %s", result.SuggestedPaths, filepath.Join(root, "www", "site-a"))
	}
	if result.DetectedAt == "" {
		t.Fatal("detected_at 为空：页面要用它标注探测时间")
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("探测改动了目录内容：前 %d 项 / 后 %d 项", len(before), len(after))
	}
	for i := range before {
		if before[i].Name() != after[i].Name() {
			t.Fatalf("探测改动了目录内容：%q → %q", before[i].Name(), after[i].Name())
		}
	}
}

// TestUnknownDetectMethodRejected 断言方法名严格：拼错的方法不会被当成探测成功。
func TestUnknownDetectMethodRejected(t *testing.T) {
	root := t.TempDir()
	if _, perr := Call("backup.sources.detectt", nil, Options{Dir: root, AllowRoots: []string{root}}, nil); perr == nil {
		t.Fatal("拼错的方法名应回 E_NOTFOUND")
	}
}
