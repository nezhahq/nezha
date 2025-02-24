package model

import (
	"time"

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

func (c *Cron) BeforeSave(tx *gorm.DB) error {
	if data, err := json.Marshal(c.Servers); err != nil {
		return err
	} else {
		c.ServersRaw = string(data)
	}
	return nil
}

func (c *Cron) AfterFind(tx *gorm.DB) error {
	return json.Unmarshal([]byte(c.ServersRaw), &c.Servers)
}
