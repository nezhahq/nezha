package model

import (
	"time"
)

// TrafficStats 流量统计模型
type TrafficStats struct {
	ID        uint      `gorm:"primarykey"`
	ServerID  uint      `gorm:"index"` // 服务器ID
	Timestamp time.Time `gorm:"index"` // 统计时间
	Type      string    // 统计类型：daily, weekly, monthly
	InBytes   int64     // 入站流量(字节)
	OutBytes  int64     // 出站流量(字节)
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 指定表名
func (TrafficStats) TableName() string {
	return "traffic_stats"
} 