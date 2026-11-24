package backupop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 保留份数边界：keep 必填，1..100。
const (
	MinRetentionKeep = 1
	MaxRetentionKeep = 100
)

// retentionParams 是 backup.retention 的显式入参形状。
//
// preset 是保留策略预设 id（daily7 / weekly4 / monthly12）：给出且 keep 未填时
// 用预设的份数，并回显推荐 cron；显式 keep 仍然优先（自定义兜底不坏）。
type retentionParams struct {
	Preset string `json:"preset,omitempty"`
	Keep   int    `json:"keep"`
	DryRun bool   `json:"dry_run"`
}

// callRetention 处理 backup.retention：按创建时间倒序保留最近 keep 份，
// 其余只删除"本插件创建"的包；最新一份由 keep≥1 天然排除在删除窗口之外。
//
// dry_run=true 只返回将删清单与预计释放字节数，不改磁盘。
func callRetention(raw json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p retentionParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	preset := findRetentionPreset(strings.TrimSpace(p.Preset))
	if strings.TrimSpace(p.Preset) != "" && preset == nil {
		return nil, invalid("未知保留策略预设: " + p.Preset + "；可用: daily7 / weekly4 / monthly12")
	}
	if preset != nil && p.Keep == 0 {
		p.Keep = preset.Keep
	}
	if p.Keep < MinRetentionKeep || p.Keep > MaxRetentionKeep {
		return nil, invalid(fmt.Sprintf("keep 必须在 %d–%d 之间", MinRetentionKeep, MaxRetentionKeep))
	}
	backups, err := exec.List(opts.Dir)
	if err != nil {
		return nil, classify(err)
	}
	// 不依赖执行面返回顺序：retention 自己按同一口径排序，keep 语义才可复现。
	sortBackups(backups)
	result := RetentionData{
		Deleted:   []RetentionItem{},
		Protected: []RetentionProtected{},
		DryRun:    p.DryRun,
	}
	// keep ≥ 1 ⇒ 排序后的第 0 份（最新）永远落在保留窗口内，不会被删除。
	for index, backup := range backups {
		if index < p.Keep {
			continue
		}
		if !backup.Managed {
			result.Protected = append(result.Protected, RetentionProtected{
				ID: backup.ID, Name: backup.Name, Reason: "非本插件创建，已跳过",
			})
			continue
		}
		if !p.DryRun {
			if err := exec.Delete(opts.Dir, backup.ID); err != nil {
				return nil, retentionDeleteError(err, len(result.Deleted))
			}
		}
		result.Deleted = append(result.Deleted, RetentionItem{
			ID: backup.ID, Name: backup.Name, CreatedAt: backup.CreatedAt, SizeBytes: backup.SizeBytes,
		})
		result.FreedBytes += backup.SizeBytes
	}
	result.Kept = len(backups) - len(result.Deleted)
	if preset != nil {
		result.Preset = preset.ID
		result.Schedule = preset.Schedule
	}
	return encode(result)
}

// retentionDeleteError 在删除中途失败时补上"已删除 N 份"上下文，避免调用方以为一份都没删。
func retentionDeleteError(err error, deleted int) *pluginapi.Error {
	perr := classify(err)
	if deleted == 0 {
		return perr
	}
	return &pluginapi.Error{
		Code:    perr.Code,
		Message: fmt.Sprintf("保留策略删除中止（已删除 %d 份）: %s", deleted, perr.Message),
	}
}
