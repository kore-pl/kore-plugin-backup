package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// embeddedInfo 是随二进制发布的内嵌清单：握手字段与安装期 info.json 同源。
//
//go:embed info.json
var embeddedInfo []byte

// manifest 是 info.json 中插件运行期真正需要的字段，其余字段归内核校验。
type manifest struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Protocol     string   `json:"protocol"`
	RequiresKore string   `json:"requires_kore"`
	Capabilities []string `json:"capabilities"`
}

// config 是插件运行期配置：环境变量 + 内嵌清单，不读任何配置文件。
type config struct {
	socket       string
	id           string
	token        string
	version      string
	requiresKore string
	capabilities []string
	backupDir    string
	allowRoots   []string
	startedAt    time.Time
}

// loadConfig 读取协议环境变量并解析内嵌 info.json。
func loadConfig() (config, error) {
	var m manifest
	if err := json.Unmarshal(embeddedInfo, &m); err != nil {
		return config{}, fmt.Errorf("解析内嵌 info.json: %w", err)
	}
	if m.Protocol != pluginapi.Protocol {
		return config{}, fmt.Errorf("info.json.protocol=%q 与 SDK 协议 %q 不一致", m.Protocol, pluginapi.Protocol)
	}
	roots, err := parseAllowRoots(os.Getenv("KORE_BACKUP_ALLOW_ROOTS"))
	if err != nil {
		return config{}, err
	}
	cfg := config{
		socket:       os.Getenv("KORE_PLUGIN_SOCK"),
		id:           os.Getenv("KORE_PLUGIN_ID"),
		token:        os.Getenv("KORE_PLUGIN_TOKEN"),
		version:      m.Version,
		requiresKore: m.RequiresKore,
		capabilities: m.Capabilities,
		backupDir:    resolveBackupDir(),
		allowRoots:   roots,
		startedAt:    time.Now(),
	}
	if cfg.id == "" {
		cfg.id = m.ID
	}
	if cfg.socket == "" {
		return config{}, fmt.Errorf("缺少环境变量 KORE_PLUGIN_SOCK")
	}
	return cfg, nil
}

// resolveBackupDir 解析备份目录：KORE_BACKUP_DIR 优先，否则 <KORE_STATE_DIR>/backups。
// KORE_STATE_DIR 缺省 /var/lib/kore，与其它插件保持一致。
func resolveBackupDir() string {
	if dir := strings.TrimSpace(os.Getenv("KORE_BACKUP_DIR")); dir != "" {
		return filepath.Clean(dir)
	}
	state := strings.TrimSpace(os.Getenv("KORE_STATE_DIR"))
	if state == "" {
		state = "/var/lib/kore"
	}
	return filepath.Join(filepath.Clean(state), "backups")
}

// parseAllowRoots 解析 KORE_BACKUP_ALLOW_ROOTS：支持系统路径分隔符与逗号两种写法，
// 缺省使用 /var/www、/etc/kore、/home；非绝对路径视为配置错误。
func parseAllowRoots(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return append([]string(nil), backupop.DefaultAllowRoots...), nil
	}
	replaced := strings.ReplaceAll(raw, ",", string(os.PathListSeparator))
	roots := make([]string, 0, 3)
	for _, item := range filepath.SplitList(replaced) {
		root := strings.TrimSpace(item)
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("KORE_BACKUP_ALLOW_ROOTS 含非绝对路径: %s", root)
		}
		roots = append(roots, filepath.Clean(root))
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("KORE_BACKUP_ALLOW_ROOTS 为空")
	}
	return roots, nil
}
