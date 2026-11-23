package backupop

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// Call 执行一个 backup 方法：归一方法名 → 校验参数与路径白名单 → 调用执行面。
// exec 为 nil 时使用真实 tar+gzip 文件系统实现（realFS）。
func Call(method string, params json.RawMessage, opts Options, exec Executor) (json.RawMessage, *pluginapi.Error) {
	verb, ok := businessVerb(method)
	if !ok {
		return nil, errNotFound(method)
	}
	applyOptionDefaults(&opts)
	if opts.Dir == "" {
		return nil, internalErr("备份目录未配置")
	}
	if exec == nil {
		exec = realFS{}
	}
	switch verb {
	case "create":
		return callCreate(params, opts, exec)
	case "list":
		return callList(params, opts, exec)
	case "restore":
		return callRestore(params, opts, exec)
	case "delete":
		return callDelete(params, opts, exec)
	case "verify":
		return callVerify(params, opts, exec)
	case "retention":
		return callRetention(params, opts, exec)
	case "stats":
		return callStats(params, opts, exec)
	case "download":
		return callDownload(params, opts, exec)
	case "jobs":
		return callJobs(params, opts, exec)
	case "schedule.set":
		return callScheduleSet(params, opts, exec)
	case "schedule.get":
		return callScheduleGet(params, opts, exec)
	}
	return nil, errNotFound(method)
}

// businessVerb 把 backup.<verb> 或裸 <verb> 归一为动词名。
func businessVerb(method string) (string, bool) {
	verb := method
	if strings.HasPrefix(method, "backup.") {
		verb = strings.TrimPrefix(method, "backup.")
	}
	switch verb {
	case "create", "list", "restore", "delete", "verify", "retention", "stats",
		"download", "jobs", "schedule.set", "schedule.get":
		return verb, true
	}
	return "", false
}

// applyOptionDefaults 补齐缺省上限与白名单；AllowRoots 为空时使用默认白名单。
func applyOptionDefaults(opts *Options) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if len(opts.AllowRoots) == 0 {
		opts.AllowRoots = append([]string(nil), DefaultAllowRoots...)
	}
}

// classify 把执行面错误映射为协议错误码：四类故障优先，其次业务错误，最后归 E_INTERNAL。
func classify(err error) *pluginapi.Error {
	var fe *FaultError
	if errors.As(err, &fe) {
		switch fe.Kind {
		case FaultKill:
			return upstream("进程被信号 9 终止: " + fe.Message)
		case FaultTimeout:
			return upstream("调用超时: " + fe.Message)
		case FaultDisk:
			return internalErr("磁盘满: " + fe.Message)
		case FaultCgroup:
			return unsupported("cgroup 限额触发: " + fe.Message)
		}
	}
	var oe *OpError
	if errors.As(err, &oe) {
		switch oe.Kind {
		case ErrInvalid:
			return invalid(oe.Message)
		case ErrNotFound:
			return notFound(oe.Message)
		case ErrConflict:
			return conflict(oe.Message)
		case ErrUnsupported:
			return unsupported(oe.Message)
		case ErrUpstream:
			return upstream(oe.Message)
		default:
			return internalErr(oe.Message)
		}
	}
	return internalErr("执行面失败: " + err.Error())
}

func invalid(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INVALID, Message: msg}
}

func notFound(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_NOTFOUND, Message: msg}
}

func conflict(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_CONFLICT, Message: msg}
}

func errNotFound(method string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_NOTFOUND, Message: "未知方法: " + method}
}

func upstream(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_UPSTREAM, Message: msg}
}

func internalErr(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INTERNAL, Message: msg}
}

func unsupported(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_UNSUPPORTED, Message: msg}
}

// decodeParams 校验 params 是 JSON 对象并反序列化到目标形状。
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

// encode 把结果编码为 json.RawMessage；序列化失败归 E_INTERNAL。
func encode(v any) (json.RawMessage, *pluginapi.Error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, internalErr("序列化结果失败: " + err.Error())
	}
	return data, nil
}

// elapsedMS 返回从 started 到现在的毫秒数。
func elapsedMS(started time.Time) int64 {
	return time.Since(started).Milliseconds()
}
