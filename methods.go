package main

import (
	"encoding/json"
	"log/slog"
	"runtime"
	"time"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
	"github.com/pengtang1024/kore-plugin-backup/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// statusResult 是 kore.ping 的结果（PLUGIN-PROTOCOL §7.1）。
type statusResult struct {
	Status     string `json:"status"`
	UptimeS    int64  `json:"uptime_s"`
	Goroutines int    `json:"goroutines"`
	RSSKB      int64  `json:"rss_kb"`
}

// dispatch 把请求分发为响应帧；第二个返回值表示收到 kore.shutdown。
func dispatch(request *pluginapi.Request, cfg *config, logger *slog.Logger) (*pluginapi.Response, bool) {
	response := &pluginapi.Response{Type: pluginapi.TypeResponse, ID: request.ID, ReqID: request.ReqID}
	switch request.Method {
	case "kore.shutdown":
		response.Result = json.RawMessage(`{"ok":true}`)
		logger.Info("收到 kore.shutdown，优雅退出")
		return response, true
	case "kore.ping":
		response.Result = pingResult(*cfg)
		return response, false
	case "kore.reload":
		response.Result = json.RawMessage(`{"ok":true,"config_digest":""}`)
		return response, false
	case cfg.id + ".config.get", "config.get":
		result, perr := handleConfigGet(cfg)
		if perr != nil {
			response.Error = perr
			return response, false
		}
		response.Result = result
		return response, false
	case cfg.id + ".config.set", "config.set":
		result, perr := handleConfigSet(request.Params, cfg)
		if perr != nil {
			response.Error = perr
			return response, false
		}
		response.Result = result
		return response, false
	}
	options := backupop.Options{Dir: cfg.backupDir, AllowRoots: cfg.allowRoots}
	result, perr := backupop.Call(request.Method, request.Params, options, nil)
	if perr != nil {
		response.Error = perr
		response.Result = nil
		return response, false
	}
	response.Result = result
	return response, false
}

// handleConfigGet 返回表单 schema、当前值与来源；敏感字段经 plugincfg 掩码后不回传原值。
func handleConfigGet(cfg *config) (json.RawMessage, *pluginapi.Error) {
	schema := backupSchema(defaultBackupDir(cfg.stateDir))
	values, err := json.Marshal(cfg.values)
	if err != nil {
		return nil, internalErr("序列化配置值失败: " + err.Error())
	}
	masked, err := plugincfg.MaskValues(schema, values)
	if err != nil {
		return nil, internalErr("掩码配置值失败: " + err.Error())
	}
	return encodeResult(plugincfg.GetResult{Schema: schema, Values: masked, Source: cfg.source}, "backup.config.get")
}

// handleConfigSet 执行「校验 → 原子写 → 更新内存」：写盘成功后才替换运行期目录与白名单，
// 下一次 backup.* 调用立即使用新配置（无需重启）。
func handleConfigSet(params json.RawMessage, cfg *config) (json.RawMessage, *pluginapi.Error) {
	var p plugincfg.SetParams
	if perr := decodeParams(params, &p); perr != nil {
		return nil, perr
	}
	if len(p.Values) == 0 || string(p.Values) == "null" {
		return nil, invalid("values 必须是 JSON 对象")
	}
	if p.Values[0] != '{' {
		return nil, invalid("values 必须是 JSON 对象")
	}
	merged := cfg.values
	if err := strictDecode(p.Values, &merged); err != nil {
		return nil, invalid("配置值非法: " + err.Error())
	}
	normalized, err := validateBackupValues(merged)
	if err != nil {
		return nil, invalid("备份设置非法: " + err.Error())
	}
	if cfg.store == nil || cfg.store.Path() == "" {
		return nil, internalErr("KORE_STATE_DIR 未设置，无法保存插件配置")
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, internalErr("序列化配置值失败: " + err.Error())
	}
	if err := cfg.store.Write(raw); err != nil {
		return nil, internalErr("保存插件配置失败: " + err.Error())
	}
	cfg.values = normalized
	cfg.backupDir = normalized.Dir
	cfg.allowRoots = normalized.AllowRoots
	cfg.source = plugincfg.SourceFile
	return encodeResult(plugincfg.SetResult{Saved: true, Applied: true, RestartRequired: false, Warnings: []string{}},
		"backup.config.set")
}

// pingResult 组装 kore.ping 结果。
func pingResult(cfg config) json.RawMessage {
	data, _ := json.Marshal(statusResult{
		Status:     "ready",
		UptimeS:    int64(time.Since(cfg.startedAt).Seconds()),
		Goroutines: runtime.NumGoroutine(),
		RSSKB:      rssKB(),
	})
	return data
}

// invalid / internalErr / encodeResult / decodeParams 是配置契约在 main 包内的
// 协议错误与编解码辅助：backupop 的同名函数未导出，且业务错误映射仍归 backupop。
func invalid(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INVALID, Message: msg}
}

func internalErr(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INTERNAL, Message: msg}
}

func encodeResult(v any, method string) (json.RawMessage, *pluginapi.Error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, internalErr("序列化 " + method + " 结果失败: " + err.Error())
	}
	return data, nil
}

func decodeParams(raw json.RawMessage, target any) *pluginapi.Error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if string(raw) == "null" {
		return invalid("params 必须是 JSON 对象")
	}
	if raw[0] != '{' {
		return invalid("params 必须是 JSON 对象")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return invalid("params 解析失败: " + err.Error())
	}
	return nil
}
