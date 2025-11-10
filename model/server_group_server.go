package model

type ServerGroupServer struct {
	Common
	ServerGroupId uint64 `json:"server_group_id" gorm:"uniqueIndex:idx_server_group_server"`
	ServerId      uint64 `json:"server_id" gorm:"uniqueIndex:idx_server_group_server"`
}
