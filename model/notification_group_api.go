package model

type NotificationGroupForm struct {
	Name          string   `json:"name"`
	Notifications []uint64 `json:"notifications"`
}

type NotificationGroupResponseItem struct {
	Group         NotificationGroup `json:"group"`
	Notifications []uint64          `json:"notifications"`
}
