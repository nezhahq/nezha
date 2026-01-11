package singleton

import (
	"fmt"
	"time"
)

// HistoryQueryParams 历史数据查询参数
type HistoryQueryParams struct {
	ServerID   uint64
	ServiceID  uint64
	MetricType string
	Start      time.Time
	End        time.Time
}

// HostHistoryData 主机历史数据
type HostHistoryData struct {
	Timestamps     []int64   `json:"timestamps"`
	CPU            []float64 `json:"cpu,omitempty"`
	GPU            []float64 `json:"gpu,omitempty"`
	MemUsed        []float64 `json:"mem_used,omitempty"`
	SwapUsed       []float64 `json:"swap_used,omitempty"`
	DiskUsed       []float64 `json:"disk_used,omitempty"`
	NetInSpeed     []float64 `json:"net_in_speed,omitempty"`
	NetOutSpeed    []float64 `json:"net_out_speed,omitempty"`
	NetInTransfer  []float64 `json:"net_in_transfer,omitempty"`
	NetOutTransfer []float64 `json:"net_out_transfer,omitempty"`
	Load1          []float64 `json:"load_1,omitempty"`
	Load5          []float64 `json:"load_5,omitempty"`
	Load15         []float64 `json:"load_15,omitempty"`
	TCPConnCount   []float64 `json:"tcp_conn_count,omitempty"`
	UDPConnCount   []float64 `json:"udp_conn_count,omitempty"`
	ProcessCount   []float64 `json:"process_count,omitempty"`
	Uptime         []float64 `json:"uptime,omitempty"`
}

// QueryHostHistory 查询主机历史数据
func QueryHostHistory(serverID uint64, start, end time.Time) (*HostHistoryData, error) {
	if TSDBShared == nil {
		return nil, fmt.Errorf("TSDB not initialized")
	}

	duration := end.Sub(start)
	resolution := GetResolutionForRange(duration)

	data := &HostHistoryData{}

	// 查询各项指标
	metrics := []struct {
		name   string
		target *[]float64
	}{
		{MetricHostCPU, &data.CPU},
		{MetricHostMemUsed, &data.MemUsed},
		{MetricHostSwapUsed, &data.SwapUsed},
		{MetricHostDiskUsed, &data.DiskUsed},
		{MetricHostNetInSpeed, &data.NetInSpeed},
		{MetricHostNetOutSpeed, &data.NetOutSpeed},
		{MetricHostNetInTransfer, &data.NetInTransfer},
		{MetricHostNetOutTransfer, &data.NetOutTransfer},
		{MetricHostLoad1, &data.Load1},
		{MetricHostLoad5, &data.Load5},
		{MetricHostLoad15, &data.Load15},
		{MetricHostTCPConnCount, &data.TCPConnCount},
		{MetricHostUDPConnCount, &data.UDPConnCount},
		{MetricHostProcessCount, &data.ProcessCount},
		{MetricHostUptime, &data.Uptime},
	}

	for _, m := range metrics {
		points, err := TSDBShared.QueryHostHistory(serverID, m.name, start, end, resolution)
		if err != nil {
			continue // 跳过错误，其他指标继续查询
		}
		if len(points) > 0 {
			// 初始化时间戳数组（只需要一次）
			if len(data.Timestamps) == 0 {
				data.Timestamps = make([]int64, len(points))
				for i, p := range points {
					data.Timestamps[i] = p.Timestamp
				}
			}
			*m.target = make([]float64, len(points))
			for i, p := range points {
				(*m.target)[i] = p.Value
			}
		}
	}

	return data, nil
}

// QueryHostHistoryByMetric 查询单个指标的历史数据
func QueryHostHistoryByMetric(serverID uint64, metricType string, start, end time.Time) ([]DataPoint, error) {
	if TSDBShared == nil {
		return nil, fmt.Errorf("TSDB not initialized")
	}

	var metricName string
	switch metricType {
	case "cpu":
		metricName = MetricHostCPU
	case "gpu":
		metricName = MetricHostGPU
	case "memory", "mem_used":
		metricName = MetricHostMemUsed
	case "swap", "swap_used":
		metricName = MetricHostSwapUsed
	case "disk", "disk_used":
		metricName = MetricHostDiskUsed
	case "net_in_speed":
		metricName = MetricHostNetInSpeed
	case "net_out_speed":
		metricName = MetricHostNetOutSpeed
	case "net_in_transfer":
		metricName = MetricHostNetInTransfer
	case "net_out_transfer":
		metricName = MetricHostNetOutTransfer
	case "load1":
		metricName = MetricHostLoad1
	case "load5":
		metricName = MetricHostLoad5
	case "load15":
		metricName = MetricHostLoad15
	case "tcp_conn_count":
		metricName = MetricHostTCPConnCount
	case "udp_conn_count":
		metricName = MetricHostUDPConnCount
	case "process_count":
		metricName = MetricHostProcessCount
	case "uptime":
		metricName = MetricHostUptime
	case "temperature", "temp_max":
		metricName = MetricHostTemperatureMax
	default:
		return nil, fmt.Errorf("unknown metric type: %s", metricType)
	}

	duration := end.Sub(start)
	resolution := GetResolutionForRange(duration)

	return TSDBShared.QueryHostHistory(serverID, metricName, start, end, resolution)
}

// QueryServiceHistory 查询服务监控历史
func QueryServiceHistory(serviceID, serverID uint64, start, end time.Time) ([]ServiceHistoryPoint, error) {
	if TSDBShared == nil {
		return nil, fmt.Errorf("TSDB not initialized")
	}

	return TSDBShared.QueryServiceHistory(serviceID, serverID, start, end)
}

// ServiceDailySummary 服务每日汇总数据
type ServiceDailySummary struct {
	Date     string  `json:"date"`
	Up       uint64  `json:"up"`
	Down     uint64  `json:"down"`
	AvgDelay float32 `json:"avg_delay"`
}

// QueryServiceDailySummary 查询服务每日汇总 (用于30天可用性展示)
func QueryServiceDailySummary(serviceID uint64, days int) ([]ServiceDailySummary, error) {
	if TSDBShared == nil {
		return nil, fmt.Errorf("TSDB not initialized")
	}

	// Validate days to prevent excessively large allocations
	if days <= 0 || days > 90 {
		return nil, fmt.Errorf("invalid days value: %d", days)
	}

	now := time.Now()
	result := make([]ServiceDailySummary, days)

	for i := 0; i < days; i++ {
		dayEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, Loc).AddDate(0, 0, -i+1)
		dayStart := dayEnd.AddDate(0, 0, -1)

		points, err := TSDBShared.QueryServiceHistory(serviceID, 0, dayStart, dayEnd)
		if err != nil {
			continue
		}

		summary := ServiceDailySummary{
			Date: dayStart.Format("2006-01-02"),
		}

		var totalDelay float32
		var delayCount int
		for _, p := range points {
			summary.Up += p.Up
			summary.Down += p.Down
			if p.AvgDelay > 0 {
				totalDelay += p.AvgDelay
				delayCount++
			}
		}
		if delayCount > 0 {
			summary.AvgDelay = totalDelay / float32(delayCount)
		}

		result[days-1-i] = summary
	}

	return result, nil
}

// TransferHistoryData 流量历史数据
type TransferHistoryData struct {
	Timestamps []int64   `json:"timestamps"`
	In         []float64 `json:"in"`
	Out        []float64 `json:"out"`
}

// QueryTransferHistory 查询流量历史
func QueryTransferHistory(serverID uint64, start, end time.Time) (*TransferHistoryData, error) {
	if TSDBShared == nil {
		return nil, fmt.Errorf("TSDB not initialized")
	}

	duration := end.Sub(start)
	resolution := GetResolutionForRange(duration)

	serverLabel := fmt.Sprintf("%d", serverID)
	labelMatchers := map[string]string{"server_id": serverLabel}

	data := &TransferHistoryData{}

	// 查询入站流量
	inSeries, err := TSDBShared.QueryRange(MetricTransferIn, labelMatchers, start, end)
	if err == nil && len(inSeries) > 0 {
		points := TSDBShared.aggregatePoints(inSeries[0].Points, resolution)
		data.Timestamps = make([]int64, len(points))
		data.In = make([]float64, len(points))
		for i, p := range points {
			data.Timestamps[i] = p.Timestamp
			data.In[i] = p.Value
		}
	}

	// 查询出站流量
	outSeries, err := TSDBShared.QueryRange(MetricTransferOut, labelMatchers, start, end)
	if err == nil && len(outSeries) > 0 {
		points := TSDBShared.aggregatePoints(outSeries[0].Points, resolution)
		data.Out = make([]float64, len(points))
		for i, p := range points {
			data.Out[i] = p.Value
		}
	}

	return data, nil
}

// GetMetricForAlert 获取用于告警检查的指标值
func GetMetricForAlert(serverID uint64, metricType string) (float64, error) {
	if TSDBShared == nil {
		return 0, fmt.Errorf("TSDB not initialized")
	}

	// 使用最近 1 分钟的平均值
	return TSDBShared.GetMetricValueForAlert(serverID, metricType, 1*time.Minute)
}

// GetCycleTransfer 获取周期流量数据
// transferType: "in", "out", "all"
func GetCycleTransfer(serverID uint64, start time.Time, transferType string) (uint64, error) {
	if TSDBShared == nil {
		return 0, fmt.Errorf("TSDB not initialized")
	}

	return TSDBShared.GetTransferInCycle(serverID, start, transferType)
}

// QueryTransferSumInRange 查询时间范围内的流量总和
func QueryTransferSumInRange(serverID uint64, start, end time.Time, transferType string) (uint64, error) {
	if TSDBShared == nil {
		return 0, fmt.Errorf("TSDB not initialized")
	}

	serverLabel := fmt.Sprintf("%d", serverID)
	labelMatchers := map[string]string{"server_id": serverLabel}

	switch transferType {
	case "in":
		val, err := TSDBShared.QueryAggregatedRange(MetricTransferIn, labelMatchers, start, end, AggregationSum)
		return uint64(val), err
	case "out":
		val, err := TSDBShared.QueryAggregatedRange(MetricTransferOut, labelMatchers, start, end, AggregationSum)
		return uint64(val), err
	case "all":
		inVal, err := TSDBShared.QueryAggregatedRange(MetricTransferIn, labelMatchers, start, end, AggregationSum)
		if err != nil {
			return 0, err
		}
		outVal, err := TSDBShared.QueryAggregatedRange(MetricTransferOut, labelMatchers, start, end, AggregationSum)
		if err != nil {
			return 0, err
		}
		return uint64(inVal + outVal), nil
	default:
		return 0, fmt.Errorf("unknown transfer type: %s", transferType)
	}
}
