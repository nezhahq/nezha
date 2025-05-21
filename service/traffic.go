package service

import (
	"time"
	"nezha/model"
	"gorm.io/gorm"
)

type TrafficService struct {
	db *gorm.DB
}

func NewTrafficService(db *gorm.DB) *TrafficService {
	return &TrafficService{db: db}
}

// RecordTraffic 记录流量数据
func (s *TrafficService) RecordTraffic(serverID uint, inBytes, outBytes int64) error {
	now := time.Now()
	
	// 记录日流量
	daily := &model.TrafficStats{
		ServerID:  serverID,
		Timestamp: now,
		Type:      "daily",
		InBytes:   inBytes,
		OutBytes:  outBytes,
	}
	
	if err := s.db.Create(daily).Error; err != nil {
		return err
	}
	
	// 更新周流量
	weekStart := now.AddDate(0, 0, -int(now.Weekday()))
	weekly := &model.TrafficStats{
		ServerID:  serverID,
		Timestamp: weekStart,
		Type:      "weekly",
	}
	
	if err := s.db.Where("server_id = ? AND type = ? AND timestamp = ?", 
		serverID, "weekly", weekStart).FirstOrCreate(weekly).Error; err != nil {
		return err
	}
	
	// 更新月流量
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	monthly := &model.TrafficStats{
		ServerID:  serverID,
		Timestamp: monthStart,
		Type:      "monthly",
	}
	
	if err := s.db.Where("server_id = ? AND type = ? AND timestamp = ?", 
		serverID, "monthly", monthStart).FirstOrCreate(monthly).Error; err != nil {
		return err
	}
	
	return nil
}

// GetTrafficStats 获取流量统计数据
func (s *TrafficService) GetTrafficStats(serverID uint, statType string, start, end time.Time) ([]model.TrafficStats, error) {
	var stats []model.TrafficStats
	
	err := s.db.Where("server_id = ? AND type = ? AND timestamp BETWEEN ? AND ?",
		serverID, statType, start, end).
		Order("timestamp ASC").
		Find(&stats).Error
		
	return stats, err
} 