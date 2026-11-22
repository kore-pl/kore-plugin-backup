// Package backupop 实现 backup 插件的方法面：真实 tar+gzip 打包、恢复、校验，
// 以及路径白名单、上限与结构化结果。本包不调用任何外部 tar/gzip 命令，
// 全部使用标准库 archive/tar + compress/gzip，避免 shell 注入与版本差异。
package backupop

import (
	"fmt"
	"time"
)

const (
	// DefaultMaxBytes 是单次备份写入的未压缩内容上限：2 GiB。
	DefaultMaxBytes int64 = 2 << 30
	// DefaultMaxEntries 是单次备份的条目上限：100 万。
	DefaultMaxEntries = 1_000_000
	// MaxEntryBytes 是恢复时单条目上限：2 GiB。
	MaxEntryBytes int64 = 2 << 30
	// MaxEntryNameLen 是恢复时单条目路径长度上限：4096 字节。
	MaxEntryNameLen = 4096
	// MaxNameLen 是备份 name 的长度上限。
	MaxNameLen = 128
)

// DefaultAllowRoots 是 KORE_BACKUP_ALLOW_ROOTS 缺省时的白名单根目录。
var DefaultAllowRoots = []string{"/var/www", "/etc/kore", "/home"}

// criticalDirs 是无论白名单如何配置都拒绝的系统关键目录（含其子目录）。
var criticalDirs = []string{"/proc", "/sys", "/dev", "/run"}

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

// ErrKind 是执行面业务错误的语义类别，classify 将其映射为协议错误码。
type ErrKind string

const (
	ErrInvalid     ErrKind = "invalid"
	ErrNotFound    ErrKind = "notfound"
	ErrConflict    ErrKind = "conflict"
	ErrInternal    ErrKind = "internal"
	ErrUnsupported ErrKind = "unsupported"
)

// OpError 是执行面返回的可分类业务错误；Message 不得包含宿主绝对路径。
type OpError struct {
	Kind    ErrKind
	Message string
}

// Error 实现 error 接口。
func (e *OpError) Error() string { return string(e.Kind) + ": " + e.Message }

func newOpError(kind ErrKind, message string) *OpError {
	return &OpError{Kind: kind, Message: message}
}

// Options 是一次方法调用的运行期配置：备份目录、路径白名单与上限。
type Options struct {
	// Dir 是备份包与 sidecar 的落盘目录（KORE_BACKUP_DIR）。
	Dir string
	// AllowRoots 是允许备份/恢复的根目录白名单。
	AllowRoots []string
	// MaxBytes / MaxEntries 覆盖默认上限（0 表示用默认值，测试用）。
	MaxBytes   int64
	MaxEntries int
}

// CreateSpec 是执行面收到的打包任务：路径与 exclude 均已在 Call 内校验。
type CreateSpec struct {
	Dir         string
	Name        string
	Paths       []string
	Exclude     []string
	Compression string // gzip | none
	MaxBytes    int64
	MaxEntries  int
}

// CreateData 是打包结果；DurationMS 由 Call 统一计时回填。
type CreateData struct {
	ID         string   `json:"id"`
	Path       string   `json:"path"`
	SizeBytes  int64    `json:"size_bytes"`
	Entries    int      `json:"entries"`
	SHA256     string   `json:"sha256"`
	CreatedAt  string   `json:"created_at"`
	DurationMS int64    `json:"duration_ms"`
	Skipped    []string `json:"skipped"`
	Warnings   []string `json:"warnings"`
}

// RestoreSpec 是执行面收到的恢复任务：TargetDir 已解析为白名单内的绝对路径。
type RestoreSpec struct {
	Dir       string
	ID        string
	TargetDir string
	Paths     []string
}

// RestoreData 是恢复结果；Failed 记录未被任何条目命中的过滤路径。
type RestoreData struct {
	ID              string   `json:"id"`
	RestoredEntries int      `json:"restored_entries"`
	TargetDir       string   `json:"target_dir"`
	Failed          []string `json:"failed"`
	DurationMS      int64    `json:"duration_ms"`
}

// BackupInfo 是 backup.list 的一条备份索引信息。
type BackupInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CreatedAt   string `json:"created_at"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256,omitempty"`
	Entries     int    `json:"entries,omitempty"`
	Compression string `json:"compression,omitempty"`
	Corrupted   bool   `json:"corrupted"`
}

// ListResult 是 backup.list 的结构化结果。
type ListResult struct {
	Backups    []BackupInfo `json:"backups"`
	Count      int          `json:"count"`
	DurationMS int64        `json:"duration_ms"`
}

// DeleteResult 是 backup.delete 的结构化结果。
type DeleteResult struct {
	ID         string `json:"id"`
	Deleted    bool   `json:"deleted"`
	DurationMS int64  `json:"duration_ms"`
}

// VerifyData 是 backup.verify 的完整校验结果。
type VerifyData struct {
	ID                string `json:"id"`
	Entries           int    `json:"entries"`
	SizeBytes         int64  `json:"size_bytes"`
	UncompressedBytes int64  `json:"uncompressed_bytes"`
	SHA256            string `json:"sha256"`
	SidecarSHA256     string `json:"sidecar_sha256,omitempty"`
	SHAMatch          bool   `json:"sha_match"`
	Corrupted         bool   `json:"corrupted"`
	DurationMS        int64  `json:"duration_ms"`
}

// Executor 是备份操作的执行面；nil 时使用真实文件系统实现（realFS）。
type Executor interface {
	Create(spec CreateSpec) (CreateData, error)
	List(dir string) ([]BackupInfo, error)
	Restore(spec RestoreSpec) (RestoreData, error)
	Delete(dir, id string) error
	Verify(dir, id string) (VerifyData, error)
}

// metaFile 是 <archive>.meta.json 的落盘结构。
type metaFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CreatedAt   string `json:"created_at"`
	Entries     int    `json:"entries"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
	Compression string `json:"compression"`
}

// parsedTime 返回可比较的创建时间；解析失败时返回零值（排序时排最后）。
func parsedTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}
