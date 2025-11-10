package model

type NotificationGroupNotification struct {
	Common
	NotificationGroupID uint64 `json:"notification_group_id" gorm:"uniqueIndex:idx_notification_group_notification"`
	NotificationID      uint64 `json:"notification_id" gorm:"uniqueIndex:idx_notification_group_notification"`
}
