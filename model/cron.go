package model

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

const (
	CronCoverIgnoreAll = iota
	CronCoverAll
	CronCoverAlertTrigger
	CronTypeCronTask    = 0
	CronTypeTriggerTask = 1
)

type Cron struct {
	Common
	Name                string    `json:"name"`
	TaskType            uint8     `gorm:"default:0" json:"task_type"` // 0:计划任务 1:触发任务
	Scheduler           string    `json:"scheduler"`                  // 分钟 小时 天 月 星期
	Command             string    `json:"command,omitempty"`
	Servers             []uint64  `gorm:"-" json:"servers"`
	PushSuccessful      bool      `json:"push_successful,omitempty"`  // 推送成功的通知
	NotificationGroupID uint64    `json:"notification_group_id"`      // 指定通知方式的分组
	LastExecutedAt      time.Time `json:"last_executed_at,omitempty"` // 最后一次执行时间
	LastResult          bool      `json:"last_result,omitempty"`      // 最后一次执行结果
	Cover               uint8     `json:"cover"`                      // 计划任务覆盖范围 (0:仅覆盖特定服务器 1:仅忽略特定服务器 2:由触发该计划任务的服务器执行)

	CronJobID  cron.EntryID `gorm:"-" json:"cron_job_id,omitempty"`
	ServersRaw string       `json:"-"`
}

// MaxCronCommandBytes bounds Cron.Command for the same reason
// MaxServiceTargetBytes bounds Service.Target: the command becomes pb.Task.Data
// and is encoded once per covered Agent every time the task fires.
const MaxCronCommandBytes = 32 * 1024

// ValidateCronCommand is checked wherever a Cron is persisted, scheduled, or
// dispatched.
func ValidateCronCommand(command string) error {
	if len(command) > MaxCronCommandBytes {
		return fmt.Errorf("task command exceeds %d bytes", MaxCronCommandBytes)
	}
	return nil
}

func (c *Cron) BeforeSave(tx *gorm.DB) error {
	if err := ValidateCronCommand(c.Command); err != nil {
		return err
	}
	if data, err := json.Marshal(c.Servers); err != nil {
		return err
	} else {
		c.ServersRaw = string(data)
	}
	return nil
}

func (c *Cron) AfterFind(tx *gorm.DB) error {
	if err := json.Unmarshal([]byte(c.ServersRaw), &c.Servers); err != nil {
		return err
	}
	// 收敛历史上落库的重复 server ID（GHSA-8fgr-wx8p-v6q7）。
	c.Servers = UniqueIDs(c.Servers)
	return nil
}

// HasPermission 扩展默认的 owner/admin 检查，使得 PAT 的 server_ids 白名单
// 同样能收窄 cron 的列出、触发、删除路径。
//
// 语义按 Cover 字段分流，与 dispatch 入口（CronTrigger）的 fan-out 规则严格
// 对齐——Servers 字段在不同 Cover 下含义完全相反：
//
//   - CronCoverIgnoreAll：Servers 是 allow-list；必须每个 server 都落在 PAT
//     白名单内。空 allow-list 是「matches nothing」的退化形态，安全。
//   - CronCoverAlertTrigger：Servers 是触发服务器 allow-list；与上同。
//   - CronCoverAll：Servers 是 deny-list。dispatch 时 fan out 到 owner 的
//     全部 server 再减去这个 deny-list。受限 PAT 必须保证 deny-list 已经覆
//     盖 owner 在白名单外的所有 servers——否则 CronTrigger 会把任务发到
//     PAT 没权限的 server 上。本方法和 controller 写侧 guard
//     rejectImplicitCoverForLimitedPAT* / 运行时 guard
//     enforcePATCronDispatchScope 共用 DenyListSafeForLimitedPAT，避免列表
//     视图把越界历史/旁路写入行漏给受限 PAT。
func (c *Cron) HasPermission(ctx *gin.Context) bool {
	if !c.Common.HasPermission(ctx) {
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
	switch c.Cover {
	case CronCoverAll:
		return DenyListSafeForLimitedPAT(tok, c.GetUserID(), c.Servers)
	default:
		for _, id := range c.Servers {
			if !tok.CanAccessServer(id) {
				return false
			}
		}
		return true
	}
}
