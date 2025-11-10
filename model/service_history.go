package model

import (
	"time"
)

type ServiceHistory struct {
	ID        uint64    `gorm:"primaryKey" json:"id,omitempty"`
	CreatedAt time.Time `gorm:"index;<-:create;index:idx_server_id_created_at_service_id_avg_delay" json:"created_at,omitempty"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at,omitempty"`
	ServiceID uint64    `gorm:"index:idx_server_id_created_at_service_id_avg_delay" json:"service_id,omitempty"`
	ServerID  uint64    `gorm:"index:idx_server_id_created_at_service_id_avg_delay" json:"server_id,omitempty"`
	AvgDelay  float32   `gorm:"index:idx_server_id_created_at_service_id_avg_delay" json:"avg_delay,omitempty"` // 平均延迟，毫秒
	Up        uint64    `json:"up,omitempty"`                                                                   // 检查状态良好计数
	Down      uint64    `json:"down,omitempty"`                                                                 // 检查状态异常计数
	Data      string    `json:"data,omitempty"`
}
