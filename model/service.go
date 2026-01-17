package model

import (
	"fmt"
	"log"

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

const (
	ServiceGroupCoverAll       = iota // 覆盖选中组的所有服务器
	ServiceGroupCoverIgnoreAll        // 忽略选中组的所有服务器
)

type Service struct {
	Common
	Name                 string `json:"name"`
	Type                 uint8  `json:"type"`
	Target               string `json:"target"`
	SkipServersRaw       string `json:"-"`
	CoverServerGroupsRaw string `json:"-"`
	Duration             uint64 `json:"duration"`
	Notify               bool   `json:"notify,omitempty"`
	NotificationGroupID  uint64 `json:"notification_group_id"` // 当前服务监控所属的通知组 ID
	Cover                uint8  `json:"cover"`
	GroupCover           uint8  `json:"group_cover" gorm:"default:0"` // 服务器分组模式 0=禁用 1=覆盖选中组 2=忽略选中组

	EnableTriggerTask      bool   `gorm:"default: false" json:"enable_trigger_task,omitempty"`
	EnableShowInService    bool   `gorm:"default: false" json:"enable_show_in_service,omitempty"`
	FailTriggerTasksRaw    string `gorm:"default:'[]'" json:"-"`
	RecoverTriggerTasksRaw string `gorm:"default:'[]'" json:"-"`

	FailTriggerTasks    []uint64 `gorm:"-" json:"fail_trigger_tasks"`    // 失败时执行的触发任务id
	RecoverTriggerTasks []uint64 `gorm:"-" json:"recover_trigger_tasks"` // 恢复时执行的触发任务id

	MinLatency    float32 `json:"min_latency"`
	MaxLatency    float32 `json:"max_latency"`
	LatencyNotify bool    `json:"latency_notify,omitempty"`

	SkipServers       map[uint64]bool `gorm:"-" json:"skip_servers"`
	CoverServerGroups []uint64        `gorm:"-" json:"cover_server_groups"`
	CronJobID         cron.EntryID    `gorm:"-" json:"-"`
}

func (m *Service) PB() *pb.Task {
	return &pb.Task{
		Id:   m.ID,
		Type: uint64(m.Type),
		Data: m.Target,
	}
}

// CronSpec 返回服务监控请求间隔对应的 cron 表达式
func (m *Service) CronSpec() string {
	if m.Duration == 0 {
		// 默认间隔 30 秒
		m.Duration = 30
	}
	return fmt.Sprintf("@every %ds", m.Duration)
}

func (m *Service) BeforeSave(tx *gorm.DB) error {
	if data, err := json.Marshal(m.SkipServers); err != nil {
		return err
	} else {
		m.SkipServersRaw = string(data)
	}
	if data, err := json.Marshal(m.CoverServerGroups); err != nil {
		return err
	} else {
		m.CoverServerGroupsRaw = string(data)
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
		return err
	}
	if err := json.Unmarshal([]byte(m.CoverServerGroupsRaw), &m.CoverServerGroups); err != nil {
		log.Println("NEZHA>> Service.AfterFind:", err)
		return err
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

// IsServiceSentinelNeeded 判断该任务类型是否需要进行服务监控 需要则返回true
func IsServiceSentinelNeeded(t uint64) bool {
	switch t {
	case TaskTypeCommand, TaskTypeTerminalGRPC, TaskTypeUpgrade,
		TaskTypeKeepalive, TaskTypeNAT, TaskTypeFM,
		TaskTypeReportConfig, TaskTypeApplyConfig:
		return false
	default:
		return true
	}
}
