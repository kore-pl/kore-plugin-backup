package backupop

import (
	"encoding/json"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// callJobs 处理 backup.jobs：校验 limit 后返回备份作业列表。
func callJobs(raw json.RawMessage, root string, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p jobsParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.Limit < 0 {
		return nil, invalid("limit 不能为负数")
	}
	jobs, err := exec.Jobs(root)
	if err != nil {
		return nil, classify(err)
	}
	if jobs == nil {
		jobs = []Job{}
	}
	if p.Limit > 0 && len(jobs) > p.Limit {
		jobs = jobs[:p.Limit]
	}
	return encode(JobsResult{Root: root, Jobs: jobs, Count: len(jobs)})
}

// noopExecutor 是骨架默认执行面：不读写任何本地或 S3 存储，只返回空结果。
// 四类故障注入测试提供自己的 exec，让 Call 走错误映射路径。
type noopExecutor struct{}

func (noopExecutor) Jobs(root string) ([]Job, error) { return nil, nil }
func (noopExecutor) Create(root, target, backend string, retentionDays int) (bool, error) {
	return false, nil
}

// decodeParams 校验 params 是 JSON 对象并反序列化到目标形状。
func decodeParams(raw json.RawMessage, target any) *pluginapi.Error {
	if len(raw) == 0 || string(raw) == "null" {
		return invalid("缺少必填参数对象")
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
