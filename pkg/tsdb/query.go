package tsdb

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"

	"github.com/nezhahq/nezha/model"
)

// QueryPeriod 查询时间段
type QueryPeriod string

const (
	Period1Day   QueryPeriod = "1d"
	Period7Days  QueryPeriod = "7d"
	Period30Days QueryPeriod = "30d"
)

// ParseQueryPeriod 解析查询时间段
func ParseQueryPeriod(s string) (QueryPeriod, error) {
	switch s {
	case "1d", "":
		return Period1Day, nil
	case "7d":
		return Period7Days, nil
	case "30d":
		return Period30Days, nil
	default:
		return "", fmt.Errorf("invalid period: %s, expected 1d, 7d, or 30d", s)
	}
}

// Duration 返回时间段的时长
func (p QueryPeriod) Duration() time.Duration {
	switch p {
	case Period7Days:
		return 7 * 24 * time.Hour
	case Period30Days:
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// DownsampleInterval 返回降采样间隔
// 1d: 5分钟一个点 (288个点)
// 7d: 30分钟一个点 (336个点)
// 30d: 2小时一个点 (360个点)
func (p QueryPeriod) DownsampleInterval() time.Duration {
	switch p {
	case Period7Days:
		return 30 * time.Minute
	case Period30Days:
		return 2 * time.Hour
	default:
		return 5 * time.Minute
	}
}

// Type aliases for model types used in tsdb package
type (
	DataPoint             = model.DataPoint
	ServiceHistorySummary = model.ServiceHistorySummary
	ServerServiceStats    = model.ServerServiceStats
	ServiceHistoryResult  = model.ServiceHistoryResponse
	MetricDataPoint       = model.ServerMetricsDataPoint
)

// rawDataPoint 原始数据点（内部使用）
type rawDataPoint struct {
	timestamp int64
	value     float64
	status    float64
	hasStatus bool
}

// QueryServiceHistory 查询服务监控历史
func (db *TSDB) QueryServiceHistory(serviceID uint64, period QueryPeriod) (*ServiceHistoryResult, error) {
	if db.IsClosed() {
		return nil, fmt.Errorf("TSDB is closed")
	}

	now := time.Now()
	tr := storage.TimeRange{
		MinTimestamp: now.Add(-period.Duration()).UnixMilli(),
		MaxTimestamp: now.UnixMilli(),
	}

	serviceIDStr := fmt.Sprintf("%d", serviceID)

	// 查询延迟数据
	delayData, err := db.queryMetricByServiceID(MetricServiceDelay, serviceIDStr, tr)
	if err != nil {
		return nil, fmt.Errorf("failed to query delay data: %w", err)
	}

	// 查询状态数据
	statusData, err := db.queryMetricByServiceID(MetricServiceStatus, serviceIDStr, tr)
	if err != nil {
		return nil, fmt.Errorf("failed to query status data: %w", err)
	}

	// 合并数据并按服务器分组
	result := &ServiceHistoryResult{
		ServiceID: serviceID,
		Servers:   make([]ServerServiceStats, 0),
	}

	serverDataMap := make(map[uint64]map[int64]*rawDataPoint)

	for serverID, points := range delayData {
		if serverDataMap[serverID] == nil {
			serverDataMap[serverID] = make(map[int64]*rawDataPoint)
		}
		for _, p := range points {
			serverDataMap[serverID][p.timestamp] = &rawDataPoint{
				timestamp: p.timestamp,
				value:     p.value,
			}
		}
	}

	for serverID, points := range statusData {
		if serverDataMap[serverID] == nil {
			serverDataMap[serverID] = make(map[int64]*rawDataPoint)
		}
		for _, p := range points {
			if existing, ok := serverDataMap[serverID][p.timestamp]; ok {
				existing.status = p.value
				existing.hasStatus = true
			} else {
				serverDataMap[serverID][p.timestamp] = &rawDataPoint{
					timestamp: p.timestamp,
					status:    p.value,
					hasStatus: true,
				}
			}
		}
	}

	for serverID, pointsMap := range serverDataMap {
		points := make([]rawDataPoint, 0, len(pointsMap))
		for _, p := range pointsMap {
			points = append(points, *p)
		}
		stats := calculateStats(points, period.DownsampleInterval())
		result.Servers = append(result.Servers, ServerServiceStats{
			ServerID: serverID,
			Stats:    stats,
		})
	}

	// 按 server_id 排序
	sort.Slice(result.Servers, func(i, j int) bool {
		return result.Servers[i].ServerID < result.Servers[j].ServerID
	})

	return result, nil
}

// metricPoint 指标数据点（内部使用）
type metricPoint struct {
	timestamp int64
	value     float64
}

// queryMetricByServiceID 按 service_id 查询指标数据
func (db *TSDB) queryMetricByServiceID(metric MetricType, serviceID string, tr storage.TimeRange) (map[uint64][]metricPoint, error) {
	tfs := storage.NewTagFilters()
	// 注意：MetricGroup (__name__) 必须使用 nil 作为 key
	if err := tfs.Add(nil, []byte(metric), false, false); err != nil {
		return nil, err
	}
	if err := tfs.Add([]byte("service_id"), []byte(serviceID), false, false); err != nil {
		return nil, err
	}

	deadline := uint64(time.Now().Add(30 * time.Second).Unix())

	var search storage.Search
	search.Init(nil, db.storage, []*storage.TagFilters{tfs}, tr, 100000, deadline)
	defer search.MustClose()

	result := make(map[uint64][]metricPoint)

	for search.NextMetricBlock() {
		mbr := search.MetricBlockRef
		var block storage.Block
		mbr.BlockRef.MustReadBlock(&block)

		// 获取 server_id
		mn := storage.GetMetricName()
		if err := mn.Unmarshal(mbr.MetricName); err != nil {
			storage.PutMetricName(mn)
			continue
		}

		serverIDBytes := mn.GetTagValue("server_id")
		if len(serverIDBytes) == 0 {
			storage.PutMetricName(mn)
			continue
		}

		var serverID uint64
		fmt.Sscanf(string(serverIDBytes), "%d", &serverID)
		storage.PutMetricName(mn)

		// 解码数据块
		if err := block.UnmarshalData(); err != nil {
			continue
		}

		timestamps := make([]int64, 0)
		values := make([]float64, 0)
		timestamps, values = block.AppendRowsWithTimeRangeFilter(timestamps, values, tr)

		for i := range timestamps {
			result[serverID] = append(result[serverID], metricPoint{
				timestamp: timestamps[i],
				value:     values[i],
			})
		}
	}

	if err := search.Error(); err != nil {
		return nil, err
	}

	return result, nil
}

// calculateStats 计算统计数据并进行降采样
func calculateStats(points []rawDataPoint, downsampleInterval time.Duration) ServiceHistorySummary {
	if len(points) == 0 {
		return ServiceHistorySummary{}
	}

	// 按时间戳排序
	sort.Slice(points, func(i, j int) bool {
		return points[i].timestamp < points[j].timestamp
	})

	var totalDelay float64
	var delayCount int
	var totalUp, totalDown uint64

	for _, p := range points {
		if p.value > 0 {
			totalDelay += p.value
			delayCount++
		}
		if p.hasStatus {
			if p.status >= 0.5 {
				totalUp++
			} else {
				totalDown++
			}
		}
	}

	summary := ServiceHistorySummary{
		TotalUp:   totalUp,
		TotalDown: totalDown,
	}

	if delayCount > 0 {
		summary.AvgDelay = totalDelay / float64(delayCount)
	}

	if totalUp+totalDown > 0 {
		summary.UpPercent = float32(totalUp) / float32(totalUp+totalDown) * 100
	}

	// 降采样生成数据点
	summary.DataPoints = downsample(points, downsampleInterval)

	return summary
}

// downsample 降采样数据点
func downsample(points []rawDataPoint, interval time.Duration) []DataPoint {
	if len(points) == 0 {
		return nil
	}

	intervalMs := interval.Milliseconds()
	result := make([]DataPoint, 0)

	// 按时间窗口聚合
	buckets := make(map[int64][]rawDataPoint)
	for _, p := range points {
		bucketKey := (p.timestamp / intervalMs) * intervalMs
		buckets[bucketKey] = append(buckets[bucketKey], p)
	}

	// 获取排序的 bucket keys
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})

	// 计算每个时间窗口的平均值
	for _, ts := range keys {
		bucket := buckets[ts]
		var totalDelay float64
		var delayCount int
		var upCount int
		var statusCount int

		for _, p := range bucket {
			if p.value > 0 {
				totalDelay += p.value
				delayCount++
			}
			if p.hasStatus {
				statusCount++
				if p.status >= 0.5 {
					upCount++
				}
			}
		}

		var avgDelay float64
		if delayCount > 0 {
			avgDelay = totalDelay / float64(delayCount)
		}

		var status uint8
		if statusCount > 0 && upCount > statusCount/2 {
			status = 1
		}

		result = append(result, DataPoint{
			Timestamp: ts,
			Delay:     avgDelay,
			Status:    status,
		})
	}

	return result
}

// downsampleMetrics 纯数值降采样（不过滤零值，无状态计算）
// useLastValue 为 true 时取 bucket 内最后一个值（适用于累积型指标），否则取平均值
func downsampleMetrics(points []rawDataPoint, interval time.Duration, useLastValue bool) []MetricDataPoint {
	if len(points) == 0 {
		return nil
	}

	intervalMs := interval.Milliseconds()

	buckets := make(map[int64][]rawDataPoint)
	for _, p := range points {
		bucketKey := (p.timestamp / intervalMs) * intervalMs
		buckets[bucketKey] = append(buckets[bucketKey], p)
	}

	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})

	result := make([]MetricDataPoint, 0, len(keys))
	for _, ts := range keys {
		bucket := buckets[ts]
		var value float64
		if useLastValue {
			last := bucket[0]
			for _, p := range bucket[1:] {
				if p.timestamp >= last.timestamp {
					last = p
				}
			}
			value = last.value
		} else {
			var total float64
			for _, p := range bucket {
				total += p.value
			}
			value = total / float64(len(bucket))
		}
		result = append(result, MetricDataPoint{
			Timestamp: ts,
			Value:     value,
		})
	}

	return result
}

// isCumulativeMetric 判断指标是否为累积型（单调递增）
func isCumulativeMetric(metric MetricType) bool {
	switch metric {
	case MetricServerNetInTransfer, MetricServerNetOutTransfer, MetricServerUptime:
		return true
	default:
		return false
	}
}

// QueryServerMetrics 查询服务器指标历史
func (db *TSDB) QueryServerMetrics(serverID uint64, metric MetricType, period QueryPeriod) ([]MetricDataPoint, error) {
	if db.IsClosed() {
		return nil, fmt.Errorf("TSDB is closed")
	}

	now := time.Now()
	tr := storage.TimeRange{
		MinTimestamp: now.Add(-period.Duration()).UnixMilli(),
		MaxTimestamp: now.UnixMilli(),
	}

	serverIDStr := fmt.Sprintf("%d", serverID)

	tfs := storage.NewTagFilters()
	// 注意：MetricGroup (__name__) 必须使用 nil 作为 key
	if err := tfs.Add(nil, []byte(metric), false, false); err != nil {
		return nil, err
	}
	if err := tfs.Add([]byte("server_id"), []byte(serverIDStr), false, false); err != nil {
		return nil, err
	}

	deadline := uint64(time.Now().Add(30 * time.Second).Unix())

	var search storage.Search
	search.Init(nil, db.storage, []*storage.TagFilters{tfs}, tr, 100000, deadline)
	defer search.MustClose()

	var points []rawDataPoint

	for search.NextMetricBlock() {
		mbr := search.MetricBlockRef
		var block storage.Block
		mbr.BlockRef.MustReadBlock(&block)

		if err := block.UnmarshalData(); err != nil {
			continue
		}

		timestamps := make([]int64, 0)
		values := make([]float64, 0)
		timestamps, values = block.AppendRowsWithTimeRangeFilter(timestamps, values, tr)

		for i := range timestamps {
			points = append(points, rawDataPoint{
				timestamp: timestamps[i],
				value:     values[i],
			})
		}
	}

	if err := search.Error(); err != nil {
		return nil, err
	}

	return downsampleMetrics(points, period.DownsampleInterval(), isCumulativeMetric(metric)), nil
}

// QueryServiceHistoryByServerID 按服务器ID批量查询所有服务监控历史
func (db *TSDB) QueryServiceHistoryByServerID(serverID uint64, period QueryPeriod) (map[uint64]*ServiceHistoryResult, error) {
	if db.IsClosed() {
		return nil, fmt.Errorf("TSDB is closed")
	}

	now := time.Now()
	tr := storage.TimeRange{
		MinTimestamp: now.Add(-period.Duration()).UnixMilli(),
		MaxTimestamp: now.UnixMilli(),
	}

	serverIDStr := fmt.Sprintf("%d", serverID)

	delayData, err := db.queryMetricByServerID(MetricServiceDelay, serverIDStr, tr)
	if err != nil {
		return nil, fmt.Errorf("failed to query delay data: %w", err)
	}

	statusData, err := db.queryMetricByServerID(MetricServiceStatus, serverIDStr, tr)
	if err != nil {
		return nil, fmt.Errorf("failed to query status data: %w", err)
	}

	serviceDataMap := make(map[uint64]map[int64]*rawDataPoint)

	for serviceID, points := range delayData {
		if serviceDataMap[serviceID] == nil {
			serviceDataMap[serviceID] = make(map[int64]*rawDataPoint)
		}
		for _, p := range points {
			serviceDataMap[serviceID][p.timestamp] = &rawDataPoint{
				timestamp: p.timestamp,
				value:     p.value,
			}
		}
	}

	for serviceID, points := range statusData {
		if serviceDataMap[serviceID] == nil {
			serviceDataMap[serviceID] = make(map[int64]*rawDataPoint)
		}
		for _, p := range points {
			if existing, ok := serviceDataMap[serviceID][p.timestamp]; ok {
				existing.status = p.value
				existing.hasStatus = true
			} else {
				serviceDataMap[serviceID][p.timestamp] = &rawDataPoint{
					timestamp: p.timestamp,
					status:    p.value,
					hasStatus: true,
				}
			}
		}
	}

	results := make(map[uint64]*ServiceHistoryResult)

	for serviceID, pointsMap := range serviceDataMap {
		points := make([]rawDataPoint, 0, len(pointsMap))
		for _, p := range pointsMap {
			points = append(points, *p)
		}
		stats := calculateStats(points, period.DownsampleInterval())
		results[serviceID] = &ServiceHistoryResult{
			ServiceID: serviceID,
			Servers: []ServerServiceStats{{
				ServerID: serverID,
				Stats:    stats,
			}},
		}
	}

	return results, nil
}

// queryMetricByServerID 按 server_id 查询所有服务的指标数据
func (db *TSDB) queryMetricByServerID(metric MetricType, serverID string, tr storage.TimeRange) (map[uint64][]metricPoint, error) {
	tfs := storage.NewTagFilters()
	if err := tfs.Add(nil, []byte(metric), false, false); err != nil {
		return nil, err
	}
	if err := tfs.Add([]byte("server_id"), []byte(serverID), false, false); err != nil {
		return nil, err
	}

	deadline := uint64(time.Now().Add(30 * time.Second).Unix())

	var search storage.Search
	search.Init(nil, db.storage, []*storage.TagFilters{tfs}, tr, 100000, deadline)
	defer search.MustClose()

	result := make(map[uint64][]metricPoint)

	for search.NextMetricBlock() {
		mbr := search.MetricBlockRef
		var block storage.Block
		mbr.BlockRef.MustReadBlock(&block)

		mn := storage.GetMetricName()
		if err := mn.Unmarshal(mbr.MetricName); err != nil {
			storage.PutMetricName(mn)
			continue
		}

		serviceIDBytes := mn.GetTagValue("service_id")
		if len(serviceIDBytes) == 0 {
			storage.PutMetricName(mn)
			continue
		}

		serviceID, err := strconv.ParseUint(string(serviceIDBytes), 10, 64)
		if err != nil {
			storage.PutMetricName(mn)
			continue
		}
		storage.PutMetricName(mn)

		if err := block.UnmarshalData(); err != nil {
			continue
		}

		timestamps := make([]int64, 0)
		values := make([]float64, 0)
		timestamps, values = block.AppendRowsWithTimeRangeFilter(timestamps, values, tr)

		for i := range timestamps {
			result[serviceID] = append(result[serviceID], metricPoint{
				timestamp: timestamps[i],
				value:     values[i],
			})
		}
	}

	if err := search.Error(); err != nil {
		return nil, err
	}

	return result, nil
}
