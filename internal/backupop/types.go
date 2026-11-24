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
	// ErrUpstream 表示外部依赖（mysqldump 等）执行失败，映射为协议 E_UPSTREAM。
	ErrUpstream ErrKind = "upstream"
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

// Options 是一次方法调用的运行期配置：备份目录、路径白名单、db 维度连接与上限。
type Options struct {
	// Dir 是备份包与 sidecar 的落盘目录（KORE_BACKUP_DIR）。
	Dir string
	// AllowRoots 是允许备份/恢复的根目录白名单。
	AllowRoots []string
	// DB 是 db 维度（mysqldump）的连接配置；凭据只经环境变量传给子进程。
	DB MySQLConfig
	// MaxBytes / MaxEntries 覆盖默认上限（0 表示用默认值，测试用）。
	MaxBytes   int64
	MaxEntries int
}

// MySQLConfig 是 db 维度的连接与客户端配置。
// Password 只在 config.json（0600）里落盘，绝不进 argv、日志或方法结果。
type MySQLConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	DumpBin  string
}

// DBSpec 是 db 维度要导出的库。
type DBSpec struct {
	// Database 是库名（标识符白名单：字母数字下划线美元连字符，≤64）。
	Database string
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
	// Dimensions 是显式声明的备份维度（site / db 的组合）。
	Dimensions []string
	// DB 非 nil 时先导出该库再把 .sql 打进归档（归档内路径 db/<database>.sql）。
	DB *DBSpec
	// DBConfig 是 mysqldump 的连接配置；Password 只经 MYSQL_PWD 传给子进程。
	DBConfig MySQLConfig
	// Progress 是真实进度回调（已写条目数与未压缩字节数），可为 nil；
	// 只在确有进展时被调用，不伪造百分比。
	Progress func(entries int, bytes int64)
}

// CreateData 是打包结果；DurationMS 由 Call 统一计时回填。
type CreateData struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	// UncompressedBytes 是写入 tar 的未压缩内容字节数（真实进度分母）。
	UncompressedBytes int64    `json:"uncompressed_bytes"`
	Entries           int      `json:"entries"`
	SHA256            string   `json:"sha256"`
	CreatedAt         string   `json:"created_at"`
	DurationMS        int64    `json:"duration_ms"`
	Skipped           []string `json:"skipped"`
	Warnings          []string `json:"warnings"`
	// Dimensions 回显本次实际备份的维度。
	Dimensions []string `json:"dimensions"`
	// Preset 回显本次使用的预设 id；纯自定义调用为空。
	Preset string `json:"preset,omitempty"`
	// Stages 是真实分阶段耗时（db_dump / pack），不是伪进度。
	Stages []JobStage `json:"stages"`
	// Job 是本任务的持久化作业记录（状态机与真实计数），由 Call 回填。
	Job *JobRecord `json:"job,omitempty"`
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
	// Managed 表示该包带本插件签发的 .meta.json + 匹配的 .sha256 sidecar；
	// 保留策略只删除 Managed=true 的包，手工放入的外来包一律不动。
	Managed bool `json:"managed"`
}

// RetentionItem 是保留策略将要删除（或已删除）的一份备份。
type RetentionItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	SizeBytes int64  `json:"size_bytes"`
}

// RetentionProtected 是被保留策略跳过、未删除的备份及原因。
type RetentionProtected struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// RetentionData 是 backup.retention 的结构化结果。
//
// dry_run=true 时 Deleted 是"将被删除"的清单，磁盘不变；
// FreedBytes 是删除（或预计释放）的压缩包字节数之和。
type RetentionData struct {
	Kept       int                  `json:"kept"`
	Deleted    []RetentionItem      `json:"deleted"`
	FreedBytes int64                `json:"freed_bytes"`
	DryRun     bool                 `json:"dry_run"`
	Protected  []RetentionProtected `json:"protected"`
	// Preset 回显保留策略预设 id；纯自定义 keep 调用为空。
	Preset string `json:"preset,omitempty"`
	// Schedule 是预设推荐的内核任务 cron（前端据此创建定时任务）。
	Schedule string `json:"schedule,omitempty"`
}

// StatsNameBucket 是按备份 name 汇总的一行。
type StatsNameBucket struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Bytes int64  `json:"bytes"`
}

// StatsData 是 backup.stats 的结构化结果。
type StatsData struct {
	TotalCount int               `json:"total_count"`
	TotalBytes int64             `json:"total_bytes"`
	Oldest     *string           `json:"oldest"`
	Newest     *string           `json:"newest"`
	ByName     []StatsNameBucket `json:"by_name"`
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
	ID string `json:"id"`
	// OK 是全部检查项通过的汇总；任一项失败即 false。
	OK bool `json:"ok"`
	// Checks 是逐项检查结果（exists / size / sha256 / readable / entries），失败项 detail 说明坏在哪。
	Checks []VerifyCheck `json:"checks"`
	// Legacy 表示旧备份没有记录 sha256，已降级为仅检查可读性（此时仍可能 OK=true）。
	Legacy            bool   `json:"legacy"`
	Entries           int    `json:"entries"`
	SizeBytes         int64  `json:"size_bytes"`
	UncompressedBytes int64  `json:"uncompressed_bytes"`
	SHA256            string `json:"sha256"`
	SidecarSHA256     string `json:"sidecar_sha256,omitempty"`
	SHAMatch          bool   `json:"sha_match"`
	Corrupted         bool   `json:"corrupted"`
	CreatedAt         string `json:"created_at"`
	DurationMS        int64  `json:"duration_ms"`
}

// VerifyCheck 是 backup.verify 的单条检查结果。
type VerifyCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Executor 是备份操作的执行面；nil 时使用真实文件系统实现（realFS）。
type Executor interface {
	Create(spec CreateSpec) (CreateData, error)
	List(dir string) ([]BackupInfo, error)
	Restore(spec RestoreSpec) (RestoreData, error)
	Delete(dir, id string) error
	Verify(dir, id string) (VerifyData, error)
	Download(spec DownloadSpec) (DownloadData, error)
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
