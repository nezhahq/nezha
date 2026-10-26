package model

import (
	"fmt"
	"log"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"

	pb "github.com/nezhahq/nezha/proto"
)

const (
	_ = iota
	TaskTypeHTTPGet
	TaskTypeICMPPing
	TaskTypeTCPPing
	TaskTypeCommand
	TaskTypeTerminal
	TaskTypeUpgrade
	TaskTypeKeepalive
	TaskTypeTerminalGRPC
	TaskTypeNAT
	TaskTypeReportHostInfoDeprecated
	TaskTypeFM
	TaskTypeReportConfig
	TaskTypeApplyConfig
	// TaskTypeServerTransferApply: per-transfer credential rotation.
	// Pre-transfer agents do not recognise this type — dashboard MUST gate
	// transfers on agent capability before pushing.
	TaskTypeServerTransferApply
	TaskTypeExec
	TaskTypeFsList
	TaskTypeFsRead
	TaskTypeFsWrite
	TaskTypeFsDelete
	TaskTypeFsTransfer
)

// IsServiceMonitorType reports whether t is a passive service probe. Service
// monitors and privileged Agent-control tasks share the protobuf Task.Type
// namespace, so every path that persists, schedules, or dispatches a Service
// must use this allowlist instead of accepting an arbitrary task integer.
func IsServiceMonitorType(t uint64) bool {
	switch t {
	case TaskTypeHTTPGet, TaskTypeICMPPing, TaskTypeTCPPing:
		return true
	default:
		return false
	}
}

// ValidateServiceMonitorType returns an actionable error at API, model, and
// scheduler boundaries. Keeping the check in model avoids a future caller
// accidentally turning a monitor-only capability into Agent command/config
// execution by copying Service.Type into pb.Task.Type.
func ValidateServiceMonitorType(t uint64) error {
	if !IsServiceMonitorType(t) {
		return fmt.Errorf("invalid service monitor type %d: allowed types are 1 (HTTP GET), 2 (ICMP ping), and 3 (TCP ping)", t)
	}
	return nil
}

// IsMCPRPCResult 判定一个 TaskResult.Type 是否属于 MCP 走 RequestTask 通道的
// 一次性 RPC 类型。dashboard 的 RequestTask 接收循环用它把这些回包路由到
// Server.inflightRPC 等待方，而不是走 ServiceSentinel。
//
// TaskTypeFsTransfer 走 IOStream 而不是 RequestTask 回包，故不在此列；agent
// 不会对它发 TaskResult。
func IsMCPRPCResult(t uint64) bool {
	switch t {
	case TaskTypeExec, TaskTypeFsList, TaskTypeFsRead, TaskTypeFsWrite, TaskTypeFsDelete:
		return true
	}
	return false
}

// ExecRequest 是 server.exec 通过 Task.Data 下发到 agent 的载荷（JSON）。
type ExecRequest struct {
	Cmd            string            `json:"cmd"`
	Args           []string          `json:"args,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds uint32            `json:"timeout_seconds,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	MaxOutputBytes uint32            `json:"max_output_bytes,omitempty"`
}

// ExecResult 是 agent 通过 TaskResult.Data 回传的执行结果（JSON）。
type ExecResult struct {
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	DurationMs      int64  `json:"duration_ms"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	Error           string `json:"error,omitempty"`
}

// FsListRequest fs.list 下发载荷。
type FsListRequest struct {
	Path       string `json:"path"`
	ShowHidden bool   `json:"show_hidden,omitempty"`
}

// FsEntry 单条目录元数据。
type FsEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Size        int64  `json:"size"`
	Mode        string `json:"mode"`
	ModTimeUnix int64  `json:"mtime"`
	IsSymlink   bool   `json:"is_symlink,omitempty"`
	LinkTarget  string `json:"link_target,omitempty"`
}

// FsListResult fs.list 回包。
type FsListResult struct {
	Entries   []FsEntry `json:"entries"`
	Truncated bool      `json:"truncated,omitempty"`
	Total     int       `json:"total,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// FsReadRequest fs.read 下发载荷。Offset/Length 单位为字节；encoding 控制返回。
type FsReadRequest struct {
	Path     string `json:"path"`
	Offset   int64  `json:"offset,omitempty"`
	Length   int64  `json:"length,omitempty"`
	Encoding string `json:"encoding,omitempty"`
}

// FsReadResult fs.read 回包。Content 按 encoding 编码（utf8 原文 / base64 二进制安全）。
type FsReadResult struct {
	Content   string `json:"content"`
	Encoding  string `json:"encoding"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}

// FsWriteRequest fs.write 下发载荷。Mode 用 unix 数字字符串如 "0644"。
type FsWriteRequest struct {
	Path          string `json:"path"`
	Content       string `json:"content"`
	Encoding      string `json:"encoding,omitempty"`
	Mode          string `json:"mode,omitempty"`
	IfMatchSHA256 string `json:"if_match_sha256,omitempty"`
	CreateDirs    bool   `json:"create_dirs,omitempty"`
}

// FsWriteResult fs.write 回包。
type FsWriteResult struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Error  string `json:"error,omitempty"`
}

// FsDeleteRequest fs.delete 下发载荷。
type FsDeleteRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// FsDeleteResult fs.delete 回包。
type FsDeleteResult struct {
	DeletedCount int    `json:"deleted_count"`
	Error        string `json:"error,omitempty"`
}

const (
	// MCPFsTransferOpUpload / Download 区分 IOStream 内的数据流向。
	MCPFsTransferOpUpload   = "upload"
	MCPFsTransferOpDownload = "download"

	// MCPFsTransferMaxSize 单次传输硬上限，dashboard 和 agent 双方都拒绝
	// 超出大小的请求。设为 100MiB 与产品语义"~100MB 大文件"对齐。
	MCPFsTransferMaxSize = 100 * 1024 * 1024
)

// FsTransferRequest 通过 Task.Data 下发到 agent；agent 据此打开本地
// IOStream，按 op 完成上/下行。streamId 用于 agent IOStream 引导帧。
type FsTransferRequest struct {
	StreamID       string `json:"stream_id"`
	Op             string `json:"op"`
	Path           string `json:"path"`
	Size           int64  `json:"size,omitempty"`
	Mode           string `json:"mode,omitempty"`
	CreateDirs     bool   `json:"create_dirs,omitempty"`
	IfMatchSHA256  string `json:"if_match_sha256,omitempty"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

// 双向 IOStream 控制帧 magic（每帧第一帧的前 4 字节）。数据帧不带 magic。
//
// 这套 magic 与 FM 协议（NZTD/NZFN/NERR/NZUP）共存而不冲突：NZTD 在 FM 表示
// "file header"，在 transfer 表示"download header"，但两条协议通过不同的
// task type（TaskTypeFM vs TaskTypeFsTransfer）分流，不会复用同一个 agent
// goroutine，所以 magic 撞名只是字面巧合，不会破坏解析。
var (
	MCPFsXferMagicUploadHdr   = []byte{0x4E, 0x5A, 0x54, 0x55} // NZTU
	MCPFsXferMagicDownloadHdr = []byte{0x4E, 0x5A, 0x54, 0x44} // NZTD
	MCPFsXferMagicOK          = []byte{0x4E, 0x5A, 0x54, 0x4F} // NZTO
	MCPFsXferMagicErr         = []byte{0x4E, 0x5A, 0x54, 0x45} // NZTE
	MCPFsXferMagicChunk       = []byte{0x4E, 0x5A, 0x54, 0x43} // NZTC: download data chunk
)

type TerminalTask struct {
	StreamID string
}

type TaskNAT struct {
	StreamID string
	Host     string
}

type TaskFM struct {
	StreamID string
}

const (
	ServiceCoverAll = iota
	ServiceCoverIgnoreAll
)

type Service struct {
	Common
	Name                string `json:"name"`
	Type                uint8  `json:"type"`
	Target              string `json:"target"`
	SkipServersRaw      string `json:"-"`
	Duration            uint64 `json:"duration"`
	DisplayIndex        int    `json:"display_index"` // 展示排序，越大越靠前
	Notify              bool   `json:"notify,omitempty"`
	NotificationGroupID uint64 `json:"notification_group_id"` // 当前服务监控所属的通知组 ID
	Cover               uint8  `json:"cover"`

	EnableTriggerTask      bool   `gorm:"default: false" json:"enable_trigger_task,omitempty"`
	HideForGuest           bool   `json:"hide_for_guest,omitempty"` // 对游客隐藏
	FailTriggerTasksRaw    string `gorm:"default:'[]'" json:"-"`
	RecoverTriggerTasksRaw string `gorm:"default:'[]'" json:"-"`

	FailTriggerTasks    []uint64 `gorm:"-" json:"fail_trigger_tasks"`    // 失败时执行的触发任务id
	RecoverTriggerTasks []uint64 `gorm:"-" json:"recover_trigger_tasks"` // 恢复时执行的触发任务id

	MinLatency    float32 `json:"min_latency"`
	MaxLatency    float32 `json:"max_latency"`
	LatencyNotify bool    `json:"latency_notify,omitempty"`

	SkipServers map[uint64]bool `gorm:"-" json:"skip_servers"`
	CronJobID   cron.EntryID    `gorm:"-" json:"-"`
}

func (m *Service) PB() *pb.Task {
	if m == nil || !IsServiceMonitorType(uint64(m.Type)) {
		return nil
	}
	return &pb.Task{
		Id:   m.ID,
		Type: uint64(m.Type),
		Data: m.Target,
	}
}

// HasPermission 扩展默认的 owner/admin 检查，让 PAT 的 server_ids 白名单
// 同样能收窄 service monitor 的列出/删除/更新路径，语义与 Cron.HasPermission
// 对齐：
//   - ServiceCoverAll：SkipServers 是 deny-set。DispatchTask 会探测 owner 在
//     deny-set 之外的所有 server，所以受限 PAT 必须保证 deny-set 已经覆盖
//     白名单外的全部 owner servers。判定与 controller 的
//     enforcePATServiceDispatchScope / rejectImplicitServiceCoverForLimitedPAT
//     共用 denyListSafeForLimitedPAT。
//   - ServiceCoverIgnoreAll：SkipServers 是 allow-set，要求每个被覆盖的
//     server 都在 PAT 白名单内。
//   - 其它情况保留旧的“PAT 按 owner 关系判定”行为。
func (m *Service) HasPermission(ctx *gin.Context) bool {
	if !m.Common.HasPermission(ctx) {
		return false
	}
	v, ok := ctx.Get(CtxKeyAPIToken)
	if !ok {
		return true
	}
	tok, _ := v.(APITokenAccessor)
	if tok == nil {
		return true
	}
	switch m.Cover {
	case ServiceCoverAll:
		return DenyListSafeForLimitedPAT(tok, m.GetUserID(), skipServersTrueIDs(m.SkipServers))
	case ServiceCoverIgnoreAll:
		for _, id := range skipServersTrueIDs(m.SkipServers) {
			if !tok.CanAccessServer(id) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

func skipServersTrueIDs(skip map[uint64]bool) []uint64 {
	if len(skip) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(skip))
	for id, mark := range skip {
		if mark {
			out = append(out, id)
		}
	}
	return out
}

// CronSpec 返回服务监控请求间隔对应的 cron 表达式
func (m *Service) CronSpec() string {
	if m.Duration == 0 {
		// 默认间隔 30 秒
		m.Duration = 30
	}
	return fmt.Sprintf("@every %ds", m.Duration)
}

// MaxServiceDuration is the largest supported probe interval, in seconds.
const MaxServiceDuration uint64 = 30 * 24 * 60 * 60

func (m *Service) BeforeSave(tx *gorm.DB) error {
	// Reject before persistence; a row that only fails cron registration can prevent restart.
	if m.Duration > MaxServiceDuration {
		return fmt.Errorf("service duration exceeds 30 days")
	}
	if err := ValidateServiceMonitorType(uint64(m.Type)); err != nil {
		return err
	}
	if data, err := json.Marshal(m.SkipServers); err != nil {
		return err
	} else {
		m.SkipServersRaw = string(data)
	}
	if data, err := json.Marshal(m.FailTriggerTasks); err != nil {
		return err
	} else {
		m.FailTriggerTasksRaw = string(data)
	}
	if data, err := json.Marshal(m.RecoverTriggerTasks); err != nil {
		return err
	} else {
		m.RecoverTriggerTasksRaw = string(data)
	}
	return nil
}

func (m *Service) AfterFind(tx *gorm.DB) error {
	m.SkipServers = make(map[uint64]bool)
	if err := json.Unmarshal([]byte(m.SkipServersRaw), &m.SkipServers); err != nil {
		log.Println("NEZHA>> Service.AfterFind:", err)
		return nil
	}

	// 加载触发任务列表
	if err := json.Unmarshal([]byte(m.FailTriggerTasksRaw), &m.FailTriggerTasks); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(m.RecoverTriggerTasksRaw), &m.RecoverTriggerTasks); err != nil {
		return err
	}

	return nil
}

// IsServiceSentinelNeeded accepts results only for the three probe types. An
// unknown or privileged task type must never enter ServiceSentinel merely
// because it was not listed in a denylist.
func IsServiceSentinelNeeded(t uint64) bool {
	return IsServiceMonitorType(t)
}
