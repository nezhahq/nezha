package model

type CronForm struct {
	TaskType            uint8    `json:"task_type,omitempty" default:"0"` // 0:计划任务 1:触发任务
	Name                string   `json:"name,omitempty" minLength:"1"`
	Scheduler           string   `json:"scheduler,omitempty"`
	Command             string   `json:"command,omitempty" validate:"optional"`
	Servers             []uint64 `json:"servers,omitempty"`
	Cover               uint8    `json:"cover,omitempty" default:"0"`
	PushSuccessful      bool     `json:"push_successful,omitempty" validate:"optional"`
	NotificationGroupID uint64   `json:"notification_group_id,omitempty"`
}
