package singleton

import (
	"fmt"
	"time"

	"github.com/nezhahq/nezha/model"
)

// WAFStats WAF 统计数据
type WAFStats struct {
	TotalBlocks    uint64           `json:"total_blocks"`     // 总封禁次数
	UniqueIPs      int              `json:"unique_ips"`       // 独立 IP 数量
	BlocksByReason map[uint8]uint64 `json:"blocks_by_reason"` // 按原因分类的封禁次数
	RecentBlocks   []WAFBlockEvent  `json:"recent_blocks"`    // 最近的封禁事件
	HourlyTrend    []WAFHourlyStats `json:"hourly_trend"`     // 每小时趋势
	TopBlockedIPs  []WAFIPStats     `json:"top_blocked_ips"`  // 被封禁次数最多的 IP
}

// WAFBlockEvent WAF 封禁事件
type WAFBlockEvent struct {
	IP        string    `json:"ip"`
	Reason    uint8     `json:"reason"`
	Count     uint64    `json:"count"`
	Timestamp time.Time `json:"timestamp"`
}

// WAFHourlyStats 每小时统计
type WAFHourlyStats struct {
	Hour   string `json:"hour"`   // 格式: "2006-01-02 15:00"
	Blocks uint64 `json:"blocks"` // 封禁次数
}

// WAFIPStats IP 统计
type WAFIPStats struct {
	IP     string `json:"ip"`
	Blocks uint64 `json:"blocks"`
}

// CheckIP 检查 IP 是否被封禁 (使用数据库)
func CheckIP(ip string) error {
	if ip == "" {
		return nil
	}
	var w model.WAF
	return w.CheckIP(DB, ip)
}

// BlockIP 封禁 IP (写入数据库 + TSDB)
func BlockIP(ip string, reason uint8, uid int64) error {
	if ip == "" {
		return nil
	}

	// 先查询当前状态
	var w model.WAF
	DB.Where("ip = ?", ip).First(&w)

	// 写入数据库
	if err := w.BlockIP(DB, ip, reason); err != nil {
		return fmt.Errorf("failed to block IP in database: %w", err)
	}

	// 同时写入 TSDB 用于统计
	if TSDBShared != nil {
		count := uint64(1)
		if reason == model.WAFBlockReasonTypeManual {
			count = 99999
		}
		if err := TSDBShared.WriteWAFBlock(ip, reason, count); err != nil {
			// TSDB 写入失败不影响主功能，只记录日志
			fmt.Printf("NEZHA>> WAF: failed to write to TSDB: %v\n", err)
		}
	}

	return nil
}

// UnblockIP 解除 IP 封禁
func UnblockIP(ip string, uid int64) error {
	return model.UnblockIP(DB, ip)
}

// BatchUnblockIP 批量解除 IP 封禁
func BatchUnblockIP(ips []string) error {
	return model.BatchUnblockIP(DB, ips)
}

// ListBlockedIPs 列出当前被封禁的 IP (从数据库查询)
func ListBlockedIPs(limit, offset int) ([]*model.WAFApiMock, int64, error) {
	var wafs []model.WAF
	var total int64

	now := uint64(time.Now().Unix())

	// 查询所有记录
	if err := DB.Model(&model.WAF{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	if err := DB.Order("block_timestamp DESC").Offset(offset).Limit(limit).Find(&wafs).Error; err != nil {
		return nil, 0, err
	}

	// 过滤出仍处于封禁状态的 IP
	result := make([]*model.WAFApiMock, 0, len(wafs))
	for _, w := range wafs {
		if model.PowAdd(w.Count, 4, w.BlockTimestamp) > now {
			result = append(result, &model.WAFApiMock{
				IP:             w.IP,
				BlockReason:    w.BlockReason,
				BlockTimestamp: time.Unix(int64(w.BlockTimestamp), 0),
				Count:          w.Count,
			})
		}
	}

	return result, total, nil
}

// QueryWAFStats 查询 WAF 统计数据 (从 TSDB)
func QueryWAFStats(start, end time.Time, limit int) (*WAFStats, error) {
	if TSDBShared == nil {
		return nil, fmt.Errorf("TSDB not initialized")
	}

	stats := &WAFStats{
		BlocksByReason: make(map[uint8]uint64),
		RecentBlocks:   make([]WAFBlockEvent, 0),
		HourlyTrend:    make([]WAFHourlyStats, 0),
		TopBlockedIPs:  make([]WAFIPStats, 0),
	}

	// 查询所有 WAF 封禁事件
	series, err := TSDBShared.QueryRange(MetricWAFBlock, nil, start, end)
	if err != nil {
		return nil, fmt.Errorf("failed to query WAF blocks: %w", err)
	}

	// 用于统计的临时数据结构
	ipBlocks := make(map[string]uint64)
	hourlyBlocks := make(map[string]uint64)
	var allEvents []WAFBlockEvent

	for _, ts := range series {
		ip := ts.Labels["ip"]
		reasonStr := ts.Labels["reason"]
		var reason uint8
		fmt.Sscanf(reasonStr, "%d", &reason)

		for _, point := range ts.Points {
			count := uint64(point.Value)
			timestamp := time.UnixMilli(point.Timestamp)

			// 总计数
			stats.TotalBlocks += count

			// 按 IP 统计
			ipBlocks[ip] += count

			// 按原因统计
			stats.BlocksByReason[reason] += count

			// 按小时统计
			hourKey := timestamp.Format("2006-01-02 15:00")
			hourlyBlocks[hourKey] += count

			// 收集事件
			allEvents = append(allEvents, WAFBlockEvent{
				IP:        ip,
				Reason:    reason,
				Count:     count,
				Timestamp: timestamp,
			})
		}
	}

	// 独立 IP 数量
	stats.UniqueIPs = len(ipBlocks)

	// 获取最近的封禁事件（按时间降序）
	if len(allEvents) > 0 {
		// 按时间降序排序
		for i := 0; i < len(allEvents)-1; i++ {
			for j := i + 1; j < len(allEvents); j++ {
				if allEvents[j].Timestamp.After(allEvents[i].Timestamp) {
					allEvents[i], allEvents[j] = allEvents[j], allEvents[i]
				}
			}
		}
		// 取前 limit 个
		if len(allEvents) > limit {
			stats.RecentBlocks = allEvents[:limit]
		} else {
			stats.RecentBlocks = allEvents
		}
	}

	// 每小时趋势（按时间排序）
	for hour, blocks := range hourlyBlocks {
		stats.HourlyTrend = append(stats.HourlyTrend, WAFHourlyStats{
			Hour:   hour,
			Blocks: blocks,
		})
	}
	// 按时间排序
	for i := 0; i < len(stats.HourlyTrend)-1; i++ {
		for j := i + 1; j < len(stats.HourlyTrend); j++ {
			if stats.HourlyTrend[i].Hour > stats.HourlyTrend[j].Hour {
				stats.HourlyTrend[i], stats.HourlyTrend[j] = stats.HourlyTrend[j], stats.HourlyTrend[i]
			}
		}
	}

	// Top 被封禁 IP（按封禁次数降序）
	for ip, blocks := range ipBlocks {
		stats.TopBlockedIPs = append(stats.TopBlockedIPs, WAFIPStats{
			IP:     ip,
			Blocks: blocks,
		})
	}
	// 按封禁次数降序排序
	for i := 0; i < len(stats.TopBlockedIPs)-1; i++ {
		for j := i + 1; j < len(stats.TopBlockedIPs); j++ {
			if stats.TopBlockedIPs[j].Blocks > stats.TopBlockedIPs[i].Blocks {
				stats.TopBlockedIPs[i], stats.TopBlockedIPs[j] = stats.TopBlockedIPs[j], stats.TopBlockedIPs[i]
			}
		}
	}
	// 限制返回数量
	if len(stats.TopBlockedIPs) > limit {
		stats.TopBlockedIPs = stats.TopBlockedIPs[:limit]
	}

	return stats, nil
}

// QueryWAFBlocksByIP 查询指定 IP 的封禁记录 (从 TSDB)
func QueryWAFBlocksByIP(ip string, start, end time.Time) ([]WAFBlockEvent, error) {
	if TSDBShared == nil {
		return nil, fmt.Errorf("TSDB not initialized")
	}

	series, err := TSDBShared.QueryRange(MetricWAFBlock, map[string]string{"ip": ip}, start, end)
	if err != nil {
		return nil, fmt.Errorf("failed to query WAF blocks for IP %s: %w", ip, err)
	}

	var events []WAFBlockEvent
	for _, ts := range series {
		reasonStr := ts.Labels["reason"]
		var reason uint8
		fmt.Sscanf(reasonStr, "%d", &reason)

		for _, point := range ts.Points {
			events = append(events, WAFBlockEvent{
				IP:        ip,
				Reason:    reason,
				Count:     uint64(point.Value),
				Timestamp: time.UnixMilli(point.Timestamp),
			})
		}
	}

	return events, nil
}
