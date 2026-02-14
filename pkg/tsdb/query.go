package tsdb

import (
	"fmt"
	"log"
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

type rawDataPoint struct {
	timestamp int64
	value     float64
	status    float64
	hasDelay  bool
	hasStatus bool
}

func (db *TSDB) QueryServiceHistory(serviceID uint64, period QueryPeriod) (*ServiceHistoryResult, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, fmt.Errorf("TSDB is closed")
	}

	now := time.Now()
	tr := storage.TimeRange{
		MinTimestamp: now.Add(-period.Duration()).UnixMilli(),
		MaxTimestamp: now.UnixMilli(),
	}

	serviceIDStr := strconv.FormatUint(serviceID, 10)

	delayData, err := db.queryMetricByServiceID(MetricServiceDelay, serviceIDStr, tr)
	if err != nil {
		return nil, fmt.Errorf("failed to query delay data: %w", err)
	}

	statusData, err := db.queryMetricByServiceID(MetricServiceStatus, serviceIDStr, tr)
	if err != nil {
		return nil, fmt.Errorf("failed to query status data: %w", err)
	}

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
				hasDelay:  true,
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

	sort.Slice(result.Servers, func(i, j int) bool {
		return result.Servers[i].ServerID < result.Servers[j].ServerID
	})

	return result, nil
}

type DailyServiceStats struct {
	Up    uint64
	Down  uint64
	Delay float64
}

func (db *TSDB) QueryServiceDailyStats(serviceID uint64, today time.Time, days int) ([]DailyServiceStats, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, fmt.Errorf("TSDB is closed")
	}

	stats := make([]DailyServiceStats, days)
	serviceIDStr := strconv.FormatUint(serviceID, 10)

	start := today.AddDate(0, 0, -(days - 1))
	tr := storage.TimeRange{
		MinTimestamp: start.UnixMilli(),
		MaxTimestamp: today.UnixMilli(),
	}

	statusData, err := db.queryMetricByServiceID(MetricServiceStatus, serviceIDStr, tr)
	if err != nil {
		return nil, err
	}
	delayData, err := db.queryMetricByServiceID(MetricServiceDelay, serviceIDStr, tr)
	if err != nil {
		return nil, err
	}

	for _, points := range statusData {
		for _, p := range points {
			ts := time.UnixMilli(p.timestamp)
			dayIndex := (days - 1) - int(today.Sub(ts).Hours())/24
			if dayIndex < 0 || dayIndex >= days {
				continue
			}
			if p.value >= 0.5 {
				stats[dayIndex].Up++
			} else {
				stats[dayIndex].Down++
			}
		}
	}

	delayCount := make([]int, days)
	for _, points := range delayData {
		for _, p := range points {
			ts := time.UnixMilli(p.timestamp)
			dayIndex := (days - 1) - int(today.Sub(ts).Hours())/24
			if dayIndex < 0 || dayIndex >= days {
				continue
			}
			stats[dayIndex].Delay = (stats[dayIndex].Delay*float64(delayCount[dayIndex]) + p.value) / float64(delayCount[dayIndex]+1)
			delayCount[dayIndex]++
		}
	}

	return stats, nil
}

type metricPoint struct {
	timestamp int64
	value     float64
}

func (db *TSDB) queryMetricByServiceID(metric MetricType, serviceID string, tr storage.TimeRange) (map[uint64][]metricPoint, error) {
	tfs := storage.NewTagFilters()
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
	var timestamps []int64
	var values []float64

	for search.NextMetricBlock() {
		mbr := search.MetricBlockRef
		var block storage.Block
		mbr.BlockRef.MustReadBlock(&block)

		mn := storage.GetMetricName()
		if err := mn.Unmarshal(mbr.MetricName); err != nil {
			log.Printf("NEZHA>> TSDB: failed to unmarshal metric name: %v", err)
			storage.PutMetricName(mn)
			continue
		}

		serverIDBytes := mn.GetTagValue("server_id")
		if len(serverIDBytes) == 0 {
			storage.PutMetricName(mn)
			continue
		}

		serverID, err := strconv.ParseUint(string(serverIDBytes), 10, 64)
		if err != nil {
			log.Printf("NEZHA>> TSDB: failed to parse server_id %q: %v", string(serverIDBytes), err)
			storage.PutMetricName(mn)
			continue
		}
		storage.PutMetricName(mn)

		if err := block.UnmarshalData(); err != nil {
			log.Printf("NEZHA>> TSDB: failed to unmarshal block data: %v", err)
			continue
		}

		timestamps = timestamps[:0]
		values = values[:0]
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

func calculateStats(points []rawDataPoint, downsampleInterval time.Duration) ServiceHistorySummary {
	if len(points) == 0 {
		return ServiceHistorySummary{}
	}

	sort.Slice(points, func(i, j int) bool {
		return points[i].timestamp < points[j].timestamp
	})

	var totalDelay float64
	var delayCount int
	var totalUp, totalDown uint64

	for _, p := range points {
		if p.hasDelay {
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

	summary.DataPoints = downsample(points, downsampleInterval)

	return summary
}

func downsample(points []rawDataPoint, interval time.Duration) []DataPoint {
	if len(points) == 0 {
		return nil
	}

	intervalMs := interval.Milliseconds()
	result := make([]DataPoint, 0)

	// points 已排序，线性扫描分桶
	bucketStart := (points[0].timestamp / intervalMs) * intervalMs
	var totalDelay float64
	var delayCount, upCount, statusCount int

	flushBucket := func() {
		var avgDelay float64
		if delayCount > 0 {
			avgDelay = totalDelay / float64(delayCount)
		}
		var status uint8
		if statusCount > 0 && upCount > statusCount/2 {
			status = 1
		}
		result = append(result, DataPoint{
			Timestamp: bucketStart,
			Delay:     avgDelay,
			Status:    status,
		})
	}

	for _, p := range points {
		key := (p.timestamp / intervalMs) * intervalMs
		if key != bucketStart {
			flushBucket()
			bucketStart = key
			totalDelay = 0
			delayCount = 0
			upCount = 0
			statusCount = 0
		}
		if p.hasDelay {
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
	flushBucket()

	return result
}

func downsampleMetrics(points []rawDataPoint, interval time.Duration, useLastValue bool) []MetricDataPoint {
	if len(points) == 0 {
		return nil
	}

	sort.Slice(points, func(i, j int) bool {
		return points[i].timestamp < points[j].timestamp
	})

	intervalMs := interval.Milliseconds()
	result := make([]MetricDataPoint, 0)

	bucketStart := (points[0].timestamp / intervalMs) * intervalMs
	var total float64
	var count int
	var last rawDataPoint

	flushBucket := func() {
		var value float64
		if useLastValue {
			value = last.value
		} else if count > 0 {
			value = total / float64(count)
		}
		result = append(result, MetricDataPoint{
			Timestamp: bucketStart,
			Value:     value,
		})
	}

	for _, p := range points {
		key := (p.timestamp / intervalMs) * intervalMs
		if key != bucketStart {
			flushBucket()
			bucketStart = key
			total = 0
			count = 0
		}
		total += p.value
		count++
		last = p
	}
	flushBucket()

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

func (db *TSDB) QueryServerMetrics(serverID uint64, metric MetricType, period QueryPeriod) ([]MetricDataPoint, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, fmt.Errorf("TSDB is closed")
	}

	now := time.Now()
	tr := storage.TimeRange{
		MinTimestamp: now.Add(-period.Duration()).UnixMilli(),
		MaxTimestamp: now.UnixMilli(),
	}

	serverIDStr := strconv.FormatUint(serverID, 10)

	tfs := storage.NewTagFilters()
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
	var timestamps []int64
	var values []float64

	for search.NextMetricBlock() {
		mbr := search.MetricBlockRef
		var block storage.Block
		mbr.BlockRef.MustReadBlock(&block)

		if err := block.UnmarshalData(); err != nil {
			log.Printf("NEZHA>> TSDB: failed to unmarshal block data: %v", err)
			continue
		}

		timestamps = timestamps[:0]
		values = values[:0]
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

func (db *TSDB) QueryServiceHistoryByServerID(serverID uint64, period QueryPeriod) (map[uint64]*ServiceHistoryResult, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, fmt.Errorf("TSDB is closed")
	}

	now := time.Now()
	tr := storage.TimeRange{
		MinTimestamp: now.Add(-period.Duration()).UnixMilli(),
		MaxTimestamp: now.UnixMilli(),
	}

	serverIDStr := strconv.FormatUint(serverID, 10)

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
				hasDelay:  true,
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
	var timestamps []int64
	var values []float64

	for search.NextMetricBlock() {
		mbr := search.MetricBlockRef
		var block storage.Block
		mbr.BlockRef.MustReadBlock(&block)

		mn := storage.GetMetricName()
		if err := mn.Unmarshal(mbr.MetricName); err != nil {
			log.Printf("NEZHA>> TSDB: failed to unmarshal metric name: %v", err)
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
			log.Printf("NEZHA>> TSDB: failed to parse service_id %q: %v", string(serviceIDBytes), err)
			storage.PutMetricName(mn)
			continue
		}
		storage.PutMetricName(mn)

		if err := block.UnmarshalData(); err != nil {
			log.Printf("NEZHA>> TSDB: failed to unmarshal block data: %v", err)
			continue
		}

		timestamps = timestamps[:0]
		values = values[:0]
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
