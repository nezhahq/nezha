package model

type AlertRuleForm struct {
	Name                string   `json:"name" minLength:"1"`
	Rules               []*Rule  `json:"rules"`
	FailTriggerTasks    []uint64 `json:"fail_trigger_tasks"`    // 失败时触发的任务id
	RecoverTriggerTasks []uint64 `json:"recover_trigger_tasks"` // 恢复时触发的任务id
	NotificationGroupID uint64   `json:"notification_group_id"`
	TriggerMode         uint8    `json:"trigger_mode" default:"0"`
	Enable              bool     `json:"enable" validate:"optional"`
}
