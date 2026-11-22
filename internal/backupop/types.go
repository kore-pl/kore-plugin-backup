// Package backupop 实现 backup 插件的方法面：参数校验、路径白名单与结构化结果。
// 本包只做逻辑校验与执行面调用，不真写本地或 S3 存储（骨架口径）。
package backupop

import "fmt"

// minRetention / maxRetention 是骨架口径下 retention_days 的合法区间（含端点）。
const (
	minRetention = 1
	maxRetention = 3650
)

// FaultKind 标识一次故障注入的类别。
type FaultKind string

const (
	FaultNone    FaultKind = ""
	FaultKill    FaultKind = "kill"
	FaultTimeout FaultKind = "timeout"
	FaultDisk    FaultKind = "disk"
	FaultCgroup  FaultKind = "cgroup"
)

// FaultError 是执行面注入的故障错误，Call 将其映射为协议错误码。
type FaultError struct {
	Kind    FaultKind
	Message string
}

// Error 实现 error 接口，输出 "<类别>: <说明>"。
func (e *FaultError) Error() string { return fmt.Sprintf("%s: %s", e.Kind, e.Message) }

// NewFaultError 构造给定类别的故障错误。
func NewFaultError(kind FaultKind, message string) *FaultError {
	return &FaultError{Kind: kind, Message: message}
}

// Job 是备份作业列表中的一项逻辑条目。
type Job struct {
	ID       string `json:"id"`
	Target   string `json:"target"`
	Backend  string `json:"backend"`
	Schedule string `json:"schedule"`
	Enabled  bool   `json:"enabled"`
}

// Executor 是备份操作的执行面；骨架实现为 noop，测试可注入故障执行器。
type Executor interface {
	Jobs(root string) ([]Job, error)
	Create(root, target, backend string, retentionDays int) (bool, error)
}

// jobsParams / createParams 是两个方法的显式入参形状。
type jobsParams struct {
	Limit int `json:"limit"`
}

type createParams struct {
	Target        string `json:"target"`
	Backend       string `json:"backend"`
	RetentionDays int    `json:"retention_days"`
}

// JobsResult / CreateResult 是两个方法的结构化结果。
type JobsResult struct {
	Root  string `json:"root"`
	Jobs  []Job  `json:"jobs"`
	Count int    `json:"count"`
}

type CreateResult struct {
	Target        string `json:"target"`
	Backend       string `json:"backend"`
	RetentionDays int    `json:"retention_days"`
	Applied       bool   `json:"applied"`
}
