package singleton

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"

	"github.com/nezhahq/nezha/model"
)

// TSDB 指标名称常量
const (
	MetricHostCPU            = "nezha_host_cpu"
	MetricHostGPU            = "nezha_host_gpu"
	MetricHostMemUsed        = "nezha_host_mem_used"
	MetricHostSwapUsed       = "nezha_host_swap_used"
	MetricHostDiskUsed       = "nezha_host_disk_used"
	MetricHostNetInTransfer  = "nezha_host_net_in_transfer"
	MetricHostNetOutTransfer = "nezha_host_net_out_transfer"
	MetricHostNetInSpeed     = "nezha_host_net_in_speed"
	MetricHostNetOutSpeed    = "nezha_host_net_out_speed"
	MetricHostLoad1          = "nezha_host_load1"
	MetricHostLoad5          = "nezha_host_load5"
	MetricHostLoad15         = "nezha_host_load15"
	MetricHostUptime         = "nezha_host_uptime"
	MetricHostTCPConnCount   = "nezha_host_tcp_conn"
	MetricHostUDPConnCount   = "nezha_host_udp_conn"
	MetricHostProcessCount   = "nezha_host_process_count"
	MetricHostTemperatureMax = "nezha_host_temp_max"

	MetricServiceDelay  = "nezha_service_delay"
	MetricServiceUp     = "nezha_service_up"
	MetricServiceDown   = "nezha_service_down"
	MetricServiceStatus = "nezha_service_status"

	MetricTransferIn  = "nezha_transfer_in"
	MetricTransferOut = "nezha_transfer_out"

	// WAF 相关指标
	MetricWAFBlock = "nezha_waf_block" // WAF 封禁事件
)

// 数据保留策略
// Prometheus TSDB 会保存所有原始数据，通过 RetentionDuration 控制保留时间
// 查询时根据时间范围自动选择合适的聚合粒度
const (
	// 数据保留时间：90 天
	// 这是一个平衡存储空间和历史查询需求的值
	// 如果每个服务器每 3 秒上报一次，每个数据点约 15 个指标
	// 90 天约 2592000 个数据点/服务器，压缩后约 50-100MB/服务器
	DataRetentionDuration = 90 * 24 * time.Hour

	// 查询时的聚合策略阈值
	// <= 1 小时：返回原始数据（约 1200 个点）
	// <= 6 小时：30 秒聚合（约 720 个点）
	// <= 1 天：2 分钟聚合（约 720 个点）
	// <= 7 天：10 分钟聚合（约 1008 个点）
	// <= 30 天：1 小时聚合（约 720 个点）
	// > 30 天：6 小时聚合（约 360 个点/90天）
	QueryRangeRaw       = 1 * time.Hour
	QueryRange30Sec     = 6 * time.Hour
	QueryRange2Min      = 24 * time.Hour
	QueryRange10Min     = 7 * 24 * time.Hour
	QueryRange1Hour     = 30 * 24 * time.Hour
)

// TSDBStorage TSDB 存储管理器
type TSDBStorage struct {
	db     *tsdb.DB
	dbPath string
	mu     sync.RWMutex

	// 用于批量写入的缓冲
	writeMu     sync.Mutex
	writeBuffer []sample
	lastFlush   time.Time
}

type sample struct {
	labels labels.Labels
	t      int64
	v      float64
}

var (
	// TSDBShared 全局 TSDB 实例
	TSDBShared *TSDBStorage
)

// InitTSDB 初始化 TSDB 存储
func InitTSDB(dataDir string) error {
	dbPath := filepath.Join(dataDir, "tsdb")

	// 确保目录存在
	if err := os.MkdirAll(dbPath, 0755); err != nil {
		return fmt.Errorf("failed to create tsdb directory: %w", err)
	}

	opts := tsdb.DefaultOptions()
	// 设置数据保留时间为 90 天
	opts.RetentionDuration = int64(DataRetentionDuration / time.Millisecond)
	// 设置块范围 - 优化压缩和查询性能
	opts.MinBlockDuration = int64(2 * time.Hour / time.Millisecond)
	opts.MaxBlockDuration = int64(72 * time.Hour / time.Millisecond) // 3天一个大块，便于长期查询
	// 启用内存映射和快照
	opts.EnableMemorySnapshotOnShutdown = true

	db, err := tsdb.Open(dbPath, nil, nil, opts, nil)
	if err != nil {
		return fmt.Errorf("failed to open tsdb: %w", err)
	}

	TSDBShared = &TSDBStorage{
		db:          db,
		dbPath:      dbPath,
		writeBuffer: make([]sample, 0, 1000),
		lastFlush:   time.Now(),
	}

	// 启动后台降采样任务
	go TSDBShared.startDownsampleWorker()
	// 启动后台刷新任务
	go TSDBShared.startFlushWorker()

	log.Println("NEZHA>> TSDB initialized at", dbPath)
	return nil
}

// Close 关闭 TSDB
func (t *TSDBStorage) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 刷新缓冲区
	t.flushBuffer()

	if t.db != nil {
		return t.db.Close()
	}
	return nil
}

// WriteHostState 写入主机状态数据
func (t *TSDBStorage) WriteHostState(serverID uint64, state *model.HostState) error {
	if state == nil {
		return nil
	}

	now := time.Now().UnixMilli()
	serverLabel := fmt.Sprintf("%d", serverID)

	samples := []sample{
		{labels: labels.FromStrings("__name__", MetricHostCPU, "server_id", serverLabel), t: now, v: state.CPU},
		{labels: labels.FromStrings("__name__", MetricHostMemUsed, "server_id", serverLabel), t: now, v: float64(state.MemUsed)},
		{labels: labels.FromStrings("__name__", MetricHostSwapUsed, "server_id", serverLabel), t: now, v: float64(state.SwapUsed)},
		{labels: labels.FromStrings("__name__", MetricHostDiskUsed, "server_id", serverLabel), t: now, v: float64(state.DiskUsed)},
		{labels: labels.FromStrings("__name__", MetricHostNetInTransfer, "server_id", serverLabel), t: now, v: float64(state.NetInTransfer)},
		{labels: labels.FromStrings("__name__", MetricHostNetOutTransfer, "server_id", serverLabel), t: now, v: float64(state.NetOutTransfer)},
		{labels: labels.FromStrings("__name__", MetricHostNetInSpeed, "server_id", serverLabel), t: now, v: float64(state.NetInSpeed)},
		{labels: labels.FromStrings("__name__", MetricHostNetOutSpeed, "server_id", serverLabel), t: now, v: float64(state.NetOutSpeed)},
		{labels: labels.FromStrings("__name__", MetricHostLoad1, "server_id", serverLabel), t: now, v: state.Load1},
		{labels: labels.FromStrings("__name__", MetricHostLoad5, "server_id", serverLabel), t: now, v: state.Load5},
		{labels: labels.FromStrings("__name__", MetricHostLoad15, "server_id", serverLabel), t: now, v: state.Load15},
		{labels: labels.FromStrings("__name__", MetricHostUptime, "server_id", serverLabel), t: now, v: float64(state.Uptime)},
		{labels: labels.FromStrings("__name__", MetricHostTCPConnCount, "server_id", serverLabel), t: now, v: float64(state.TcpConnCount)},
		{labels: labels.FromStrings("__name__", MetricHostUDPConnCount, "server_id", serverLabel), t: now, v: float64(state.UdpConnCount)},
		{labels: labels.FromStrings("__name__", MetricHostProcessCount, "server_id", serverLabel), t: now, v: float64(state.ProcessCount)},
	}

	// 写入 GPU 数据
	for i, gpuUsage := range state.GPU {
		samples = append(samples, sample{
			labels: labels.FromStrings("__name__", MetricHostGPU, "server_id", serverLabel, "gpu_index", fmt.Sprintf("%d", i)),
			t:      now,
			v:      gpuUsage,
		})
	}

	// 写入温度数据 (最大值)
	if len(state.Temperatures) > 0 {
		maxTemp := 0.0
		for _, temp := range state.Temperatures {
			if temp.Temperature > maxTemp {
				maxTemp = temp.Temperature
			}
		}
		samples = append(samples, sample{
			labels: labels.FromStrings("__name__", MetricHostTemperatureMax, "server_id", serverLabel),
			t:      now,
			v:      maxTemp,
		})
	}

	return t.writeSamples(samples)
}

// WriteServiceHistory 写入服务监控历史数据
func (t *TSDBStorage) WriteServiceHistory(serviceID, serverID uint64, delay float32, up, down uint64) error {
	now := time.Now().UnixMilli()
	serviceLabel := fmt.Sprintf("%d", serviceID)
	serverLabel := fmt.Sprintf("%d", serverID)

	samples := []sample{
		{labels: labels.FromStrings("__name__", MetricServiceDelay, "service_id", serviceLabel, "server_id", serverLabel), t: now, v: float64(delay)},
		{labels: labels.FromStrings("__name__", MetricServiceUp, "service_id", serviceLabel, "server_id", serverLabel), t: now, v: float64(up)},
		{labels: labels.FromStrings("__name__", MetricServiceDown, "service_id", serviceLabel, "server_id", serverLabel), t: now, v: float64(down)},
	}

	// 计算状态：1 = 在线, 0 = 离线
	status := 0.0
	if up > 0 && down == 0 {
		status = 1.0
	} else if up > 0 {
		status = float64(up) / float64(up+down)
	}
	samples = append(samples, sample{
		labels: labels.FromStrings("__name__", MetricServiceStatus, "service_id", serviceLabel, "server_id", serverLabel),
		t:      now,
		v:      status,
	})

	return t.writeSamples(samples)
}

// WriteTransfer 写入流量数据
func (t *TSDBStorage) WriteTransfer(serverID uint64, in, out uint64) error {
	now := time.Now().UnixMilli()
	serverLabel := fmt.Sprintf("%d", serverID)

	samples := []sample{
		{labels: labels.FromStrings("__name__", MetricTransferIn, "server_id", serverLabel), t: now, v: float64(in)},
		{labels: labels.FromStrings("__name__", MetricTransferOut, "server_id", serverLabel), t: now, v: float64(out)},
	}

	return t.writeSamples(samples)
}

// WriteWAFBlock 写入 WAF 封禁事件
// ip: 被封禁的 IP 地址
// reason: 封禁原因类型
// count: 本次触发的计数
func (t *TSDBStorage) WriteWAFBlock(ip string, reason uint8, count uint64) error {
	now := time.Now().UnixMilli()
	reasonLabel := fmt.Sprintf("%d", reason)

	samples := []sample{
		{labels: labels.FromStrings("__name__", MetricWAFBlock, "ip", ip, "reason", reasonLabel), t: now, v: float64(count)},
	}

	return t.writeSamples(samples)
}

// writeSamples 批量写入采样数据
func (t *TSDBStorage) writeSamples(samples []sample) error {
	t.writeMu.Lock()
	t.writeBuffer = append(t.writeBuffer, samples...)
	shouldFlush := len(t.writeBuffer) >= 1000 || time.Since(t.lastFlush) > 5*time.Second
	t.writeMu.Unlock()

	if shouldFlush {
		return t.flushBuffer()
	}
	return nil
}

// flushBuffer 刷新写入缓冲区
func (t *TSDBStorage) flushBuffer() error {
	t.writeMu.Lock()
	if len(t.writeBuffer) == 0 {
		t.writeMu.Unlock()
		return nil
	}
	samples := t.writeBuffer
	t.writeBuffer = make([]sample, 0, 1000)
	t.lastFlush = time.Now()
	t.writeMu.Unlock()

	t.mu.Lock()
	defer t.mu.Unlock()

	app := t.db.Appender(context.Background())
	for _, s := range samples {
		if _, err := app.Append(0, s.labels, s.t, s.v); err != nil {
			app.Rollback()
			return fmt.Errorf("failed to append sample: %w", err)
		}
	}

	if err := app.Commit(); err != nil {
		return fmt.Errorf("failed to commit samples: %w", err)
	}

	return nil
}

// startFlushWorker 启动后台刷新任务
func (t *TSDBStorage) startFlushWorker() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if err := t.flushBuffer(); err != nil {
			log.Printf("NEZHA>> TSDB flush error: %v", err)
		}
	}
}

// QueryRange 查询时间范围内的数据
func (t *TSDBStorage) QueryRange(metricName string, labelMatchers map[string]string, start, end time.Time) ([]TimeSeriesData, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// 构建标签匹配器
	matchers := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, "__name__", metricName),
	}
	for k, v := range labelMatchers {
		matchers = append(matchers, labels.MustNewMatcher(labels.MatchEqual, k, v))
	}

	querier, err := t.db.Querier(start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("failed to create querier: %w", err)
	}
	defer querier.Close()

	seriesSet := querier.Select(context.Background(), false, nil, matchers...)

	var result []TimeSeriesData
	for seriesSet.Next() {
		series := seriesSet.At()
		ts := TimeSeriesData{
			Labels: series.Labels().Map(),
			Points: make([]DataPoint, 0),
		}

		it := series.Iterator(nil)
		for it.Next() == chunkenc.ValFloat {
			timestamp, value := it.At()
			ts.Points = append(ts.Points, DataPoint{
				Timestamp: timestamp,
				Value:     value,
			})
		}
		if it.Err() != nil {
			return nil, fmt.Errorf("iterator error: %w", it.Err())
		}

		result = append(result, ts)
	}

	if seriesSet.Err() != nil {
		return nil, fmt.Errorf("series set error: %w", seriesSet.Err())
	}

	return result, nil
}

// TimeSeriesData 时间序列数据
type TimeSeriesData struct {
	Labels map[string]string
	Points []DataPoint
}

// DataPoint 数据点
type DataPoint struct {
	Timestamp int64
	Value     float64
}

// QueryLatestValue 查询最新值
func (t *TSDBStorage) QueryLatestValue(metricName string, labelMatchers map[string]string) (float64, time.Time, error) {
	end := time.Now()
	start := end.Add(-5 * time.Minute)

	series, err := t.QueryRange(metricName, labelMatchers, start, end)
	if err != nil {
		return 0, time.Time{}, err
	}

	if len(series) == 0 || len(series[0].Points) == 0 {
		return 0, time.Time{}, fmt.Errorf("no data found")
	}

	// 返回最后一个数据点
	lastPoint := series[0].Points[len(series[0].Points)-1]
	return lastPoint.Value, time.UnixMilli(lastPoint.Timestamp), nil
}

// QueryAggregatedRange 查询聚合数据 (用于告警等场景)
func (t *TSDBStorage) QueryAggregatedRange(metricName string, labelMatchers map[string]string, start, end time.Time, aggType AggregationType) (float64, error) {
	series, err := t.QueryRange(metricName, labelMatchers, start, end)
	if err != nil {
		return 0, err
	}

	if len(series) == 0 || len(series[0].Points) == 0 {
		return 0, nil
	}

	points := series[0].Points
	switch aggType {
	case AggregationAvg:
		sum := 0.0
		for _, p := range points {
			sum += p.Value
		}
		return sum / float64(len(points)), nil
	case AggregationMax:
		maxVal := points[0].Value
		for _, p := range points {
			if p.Value > maxVal {
				maxVal = p.Value
			}
		}
		return maxVal, nil
	case AggregationMin:
		minVal := points[0].Value
		for _, p := range points {
			if p.Value < minVal {
				minVal = p.Value
			}
		}
		return minVal, nil
	case AggregationSum:
		sum := 0.0
		for _, p := range points {
			sum += p.Value
		}
		return sum, nil
	case AggregationLast:
		return points[len(points)-1].Value, nil
	default:
		return 0, fmt.Errorf("unknown aggregation type")
	}
}

// AggregationType 聚合类型
type AggregationType int

const (
	AggregationAvg AggregationType = iota
	AggregationMax
	AggregationMin
	AggregationSum
	AggregationLast
)

// startDownsampleWorker 启动降采样工作协程
func (t *TSDBStorage) startDownsampleWorker() {
	// 每小时执行一次降采样
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		t.performDownsample()
	}
}

// performDownsample 执行数据降采样
// 注意：Prometheus TSDB 本身不直接支持降采样
// 这里我们通过定期压缩来管理数据，实际的降采样需要通过查询时聚合实现
func (t *TSDBStorage) performDownsample() {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 触发 TSDB 压缩
	if err := t.db.Compact(context.Background()); err != nil {
		log.Printf("NEZHA>> TSDB compact error: %v", err)
	}
}

// GetHostState 获取服务器的最新状态 (用于告警检查)
func (t *TSDBStorage) GetHostState(serverID uint64) (*model.HostState, error) {
	serverLabel := fmt.Sprintf("%d", serverID)
	labelMatchers := map[string]string{"server_id": serverLabel}

	state := &model.HostState{}
	end := time.Now()
	start := end.Add(-1 * time.Minute)

	// 查询各项指标的最新值
	if cpu, _, err := t.QueryLatestValue(MetricHostCPU, labelMatchers); err == nil {
		state.CPU = cpu
	}

	if memUsed, _, err := t.QueryLatestValue(MetricHostMemUsed, labelMatchers); err == nil {
		state.MemUsed = uint64(memUsed)
	}

	if swapUsed, _, err := t.QueryLatestValue(MetricHostSwapUsed, labelMatchers); err == nil {
		state.SwapUsed = uint64(swapUsed)
	}

	if diskUsed, _, err := t.QueryLatestValue(MetricHostDiskUsed, labelMatchers); err == nil {
		state.DiskUsed = uint64(diskUsed)
	}

	if netInTransfer, _, err := t.QueryLatestValue(MetricHostNetInTransfer, labelMatchers); err == nil {
		state.NetInTransfer = uint64(netInTransfer)
	}

	if netOutTransfer, _, err := t.QueryLatestValue(MetricHostNetOutTransfer, labelMatchers); err == nil {
		state.NetOutTransfer = uint64(netOutTransfer)
	}

	if netInSpeed, _, err := t.QueryLatestValue(MetricHostNetInSpeed, labelMatchers); err == nil {
		state.NetInSpeed = uint64(netInSpeed)
	}

	if netOutSpeed, _, err := t.QueryLatestValue(MetricHostNetOutSpeed, labelMatchers); err == nil {
		state.NetOutSpeed = uint64(netOutSpeed)
	}

	if load1, _, err := t.QueryLatestValue(MetricHostLoad1, labelMatchers); err == nil {
		state.Load1 = load1
	}

	if load5, _, err := t.QueryLatestValue(MetricHostLoad5, labelMatchers); err == nil {
		state.Load5 = load5
	}

	if load15, _, err := t.QueryLatestValue(MetricHostLoad15, labelMatchers); err == nil {
		state.Load15 = load15
	}

	if uptime, _, err := t.QueryLatestValue(MetricHostUptime, labelMatchers); err == nil {
		state.Uptime = uint64(uptime)
	}

	if tcpConn, _, err := t.QueryLatestValue(MetricHostTCPConnCount, labelMatchers); err == nil {
		state.TcpConnCount = uint64(tcpConn)
	}

	if udpConn, _, err := t.QueryLatestValue(MetricHostUDPConnCount, labelMatchers); err == nil {
		state.UdpConnCount = uint64(udpConn)
	}

	if processCount, _, err := t.QueryLatestValue(MetricHostProcessCount, labelMatchers); err == nil {
		state.ProcessCount = uint64(processCount)
	}

	// 查询 GPU 数据
	gpuSeries, err := t.QueryRange(MetricHostGPU, labelMatchers, start, end)
	if err == nil && len(gpuSeries) > 0 {
		state.GPU = make([]float64, len(gpuSeries))
		for _, s := range gpuSeries {
			if len(s.Points) > 0 {
				// 获取 gpu_index 并设置值
				if idxStr, ok := s.Labels["gpu_index"]; ok {
					var idx int
					fmt.Sscanf(idxStr, "%d", &idx)
					if idx < len(state.GPU) {
						state.GPU[idx] = s.Points[len(s.Points)-1].Value
					}
				}
			}
		}
	}

	return state, nil
}

// GetTransferInCycle 获取周期内的流量数据
func (t *TSDBStorage) GetTransferInCycle(serverID uint64, start time.Time, metricType string) (uint64, error) {
	serverLabel := fmt.Sprintf("%d", serverID)
	labelMatchers := map[string]string{"server_id": serverLabel}

	var metricName string
	switch metricType {
	case "in":
		metricName = MetricTransferIn
	case "out":
		metricName = MetricTransferOut
	case "all":
		// 需要查询两个指标
		inVal, err := t.QueryAggregatedRange(MetricTransferIn, labelMatchers, start, time.Now(), AggregationLast)
		if err != nil {
			return 0, err
		}
		outVal, err := t.QueryAggregatedRange(MetricTransferOut, labelMatchers, start, time.Now(), AggregationLast)
		if err != nil {
			return 0, err
		}
		return uint64(inVal + outVal), nil
	default:
		return 0, fmt.Errorf("unknown transfer type: %s", metricType)
	}

	val, err := t.QueryAggregatedRange(metricName, labelMatchers, start, time.Now(), AggregationLast)
	if err != nil {
		return 0, err
	}
	return uint64(val), nil
}

// QueryHostHistory 查询主机历史数据，支持不同时间粒度
func (t *TSDBStorage) QueryHostHistory(serverID uint64, metricName string, start, end time.Time, resolution time.Duration) ([]DataPoint, error) {
	serverLabel := fmt.Sprintf("%d", serverID)
	labelMatchers := map[string]string{"server_id": serverLabel}

	series, err := t.QueryRange(metricName, labelMatchers, start, end)
	if err != nil {
		return nil, err
	}

	if len(series) == 0 {
		return nil, nil
	}

	points := series[0].Points

	// 如果 resolution 为 0，返回原始数据
	if resolution == 0 {
		return points, nil
	}

	// 按 resolution 进行聚合
	return t.aggregatePoints(points, resolution), nil
}

// aggregatePoints 按指定分辨率聚合数据点
func (t *TSDBStorage) aggregatePoints(points []DataPoint, resolution time.Duration) []DataPoint {
	if len(points) == 0 {
		return nil
	}

	// 如果 resolution 为 0，直接返回原始数据
	if resolution <= 0 {
		return points
	}

	resMs := resolution.Milliseconds()
	result := make([]DataPoint, 0)

	var currentBucket int64 = -1
	var bucketSum float64
	var bucketCount int

	for _, p := range points {
		bucket := p.Timestamp / resMs * resMs
		if bucket != currentBucket {
			if currentBucket >= 0 && bucketCount > 0 {
				result = append(result, DataPoint{
					Timestamp: currentBucket,
					Value:     bucketSum / float64(bucketCount),
				})
			}
			currentBucket = bucket
			bucketSum = p.Value
			bucketCount = 1
		} else {
			bucketSum += p.Value
			bucketCount++
		}
	}

	// 最后一个桶
	if bucketCount > 0 {
		result = append(result, DataPoint{
			Timestamp: currentBucket,
			Value:     bucketSum / float64(bucketCount),
		})
	}

	return result
}

// GetResolutionForRange 根据时间范围自动选择合适的聚合分辨率
// 目标是保持返回的数据点数量在 300-1500 之间，确保前端展示流畅
func GetResolutionForRange(duration time.Duration) time.Duration {
	switch {
	case duration <= QueryRangeRaw:
		// <= 1 小时：返回原始数据
		return 0
	case duration <= QueryRange30Sec:
		// <= 6 小时：30 秒聚合
		return 30 * time.Second
	case duration <= QueryRange2Min:
		// <= 1 天：2 分钟聚合
		return 2 * time.Minute
	case duration <= QueryRange10Min:
		// <= 7 天：10 分钟聚合
		return 10 * time.Minute
	case duration <= QueryRange1Hour:
		// <= 30 天：1 小时聚合
		return 1 * time.Hour
	default:
		// > 30 天：6 小时聚合
		return 6 * time.Hour
	}
}

// QueryServiceHistory 查询服务监控历史
func (t *TSDBStorage) QueryServiceHistory(serviceID, serverID uint64, start, end time.Time) ([]ServiceHistoryPoint, error) {
	serviceLabel := fmt.Sprintf("%d", serviceID)
	serverLabel := fmt.Sprintf("%d", serverID)
	labelMatchers := map[string]string{
		"service_id": serviceLabel,
		"server_id":  serverLabel,
	}

	delaySeries, err := t.QueryRange(MetricServiceDelay, labelMatchers, start, end)
	if err != nil {
		return nil, err
	}

	upSeries, err := t.QueryRange(MetricServiceUp, labelMatchers, start, end)
	if err != nil {
		return nil, err
	}

	downSeries, err := t.QueryRange(MetricServiceDown, labelMatchers, start, end)
	if err != nil {
		return nil, err
	}

	// 合并数据
	result := make([]ServiceHistoryPoint, 0)

	delayMap := make(map[int64]float64)
	upMap := make(map[int64]float64)
	downMap := make(map[int64]float64)

	if len(delaySeries) > 0 {
		for _, p := range delaySeries[0].Points {
			delayMap[p.Timestamp] = p.Value
		}
	}
	if len(upSeries) > 0 {
		for _, p := range upSeries[0].Points {
			upMap[p.Timestamp] = p.Value
		}
	}
	if len(downSeries) > 0 {
		for _, p := range downSeries[0].Points {
			downMap[p.Timestamp] = p.Value
		}
	}

	// 收集所有时间戳
	timestamps := make(map[int64]struct{})
	for ts := range delayMap {
		timestamps[ts] = struct{}{}
	}
	for ts := range upMap {
		timestamps[ts] = struct{}{}
	}
	for ts := range downMap {
		timestamps[ts] = struct{}{}
	}

	for ts := range timestamps {
		point := ServiceHistoryPoint{
			Timestamp: ts,
			AvgDelay:  float32(delayMap[ts]),
			Up:        uint64(upMap[ts]),
			Down:      uint64(downMap[ts]),
		}
		result = append(result, point)
	}

	return result, nil
}

// ServiceHistoryPoint 服务历史数据点
type ServiceHistoryPoint struct {
	Timestamp int64
	AvgDelay  float32
	Up        uint64
	Down      uint64
}

// GetMetricValueForAlert 获取告警检查所需的指标值
func (t *TSDBStorage) GetMetricValueForAlert(serverID uint64, metricType string, duration time.Duration) (float64, error) {
	serverLabel := fmt.Sprintf("%d", serverID)
	labelMatchers := map[string]string{"server_id": serverLabel}

	end := time.Now()
	start := end.Add(-duration)

	var metricName string
	switch metricType {
	case "cpu":
		metricName = MetricHostCPU
	case "memory":
		// 需要计算百分比，这里返回原始值
		metricName = MetricHostMemUsed
	case "swap":
		metricName = MetricHostSwapUsed
	case "disk":
		metricName = MetricHostDiskUsed
	case "net_in_speed":
		metricName = MetricHostNetInSpeed
	case "net_out_speed":
		metricName = MetricHostNetOutSpeed
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
	case "temperature_max":
		metricName = MetricHostTemperatureMax
	case "gpu_max":
		// 查询所有 GPU 并返回最大值
		gpuSeries, err := t.QueryRange(MetricHostGPU, labelMatchers, start, end)
		if err != nil {
			return 0, err
		}
		maxGPU := 0.0
		for _, s := range gpuSeries {
			if len(s.Points) > 0 {
				lastVal := s.Points[len(s.Points)-1].Value
				if lastVal > maxGPU {
					maxGPU = lastVal
				}
			}
		}
		return maxGPU, nil
	default:
		return 0, fmt.Errorf("unknown metric type: %s", metricType)
	}

	return t.QueryAggregatedRange(metricName, labelMatchers, start, end, AggregationAvg)
}

// 辅助函数：安全地获取最大值
func safeMax(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	maxVal := values[0]
	for _, v := range values[1:] {
		if v > maxVal {
			maxVal = v
		}
	}
	return maxVal
}

// 辅助函数：判断是否为 NaN
func isNaN(v float64) bool {
	return math.IsNaN(v)
}
