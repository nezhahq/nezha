package singleton

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestTSDB(t *testing.T) (string, func()) {
	tmpDir, err := os.MkdirTemp("", "nezha-tsdb-test-*")
	require.NoError(t, err)

	// 初始化时区（部分测试需要）
	if Loc == nil {
		Loc = time.Local
	}

	err = InitTSDB(tmpDir)
	require.NoError(t, err)

	cleanup := func() {
		if TSDBShared != nil {
			TSDBShared.Close()
			TSDBShared = nil
		}
		os.RemoveAll(tmpDir)
	}

	return tmpDir, cleanup
}

func TestInitTSDB(t *testing.T) {
	tmpDir, cleanup := setupTestTSDB(t)
	defer cleanup()

	assert.NotNil(t, TSDBShared)
	assert.NotNil(t, TSDBShared.db)
	assert.Equal(t, filepath.Join(tmpDir, "tsdb"), TSDBShared.dbPath)
}

func TestWriteAndQueryHostState(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)
	state := &model.HostState{
		CPU:            55.5,
		MemUsed:        1024 * 1024 * 1024,
		SwapUsed:       512 * 1024 * 1024,
		DiskUsed:       50 * 1024 * 1024 * 1024,
		NetInTransfer:  1000000,
		NetOutTransfer: 2000000,
		NetInSpeed:     1000,
		NetOutSpeed:    2000,
		Load1:          1.5,
		Load5:          1.2,
		Load15:         0.8,
		Uptime:         86400,
		TcpConnCount:   100,
		UdpConnCount:   50,
		ProcessCount:   200,
		GPU:            []float64{30.0, 45.0},
		Temperatures: []model.SensorTemperature{
			{Name: "CPU", Temperature: 65.0},
			{Name: "GPU", Temperature: 70.0},
		},
	}

	err := TSDBShared.WriteHostState(serverID, state)
	require.NoError(t, err)

	err = TSDBShared.flushBuffer()
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)
	series, err := TSDBShared.QueryRange(MetricHostCPU, map[string]string{"server_id": "1"}, start, end)
	require.NoError(t, err)
	assert.NotEmpty(t, series)
	if len(series) > 0 && len(series[0].Points) > 0 {
		assert.InDelta(t, 55.5, series[0].Points[0].Value, 0.1)
	}

	cpu, _, err := TSDBShared.QueryLatestValue(MetricHostCPU, map[string]string{"server_id": "1"})
	require.NoError(t, err)
	assert.InDelta(t, 55.5, cpu, 0.1)
}

func TestWriteAndQueryServiceHistory(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serviceID := uint64(1)
	serverID := uint64(0)

	err := TSDBShared.WriteServiceHistory(serviceID, serverID, 15.5, 10, 2)
	require.NoError(t, err)

	err = TSDBShared.flushBuffer()
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)
	series, err := TSDBShared.QueryRange(MetricServiceDelay, map[string]string{
		"service_id": "1",
		"server_id":  "0",
	}, start, end)
	require.NoError(t, err)
	assert.NotEmpty(t, series)
}

func TestWriteAndQueryTransfer(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)

	err := TSDBShared.WriteTransfer(serverID, 1000000, 2000000)
	require.NoError(t, err)

	err = TSDBShared.flushBuffer()
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)
	series, err := TSDBShared.QueryRange(MetricTransferIn, map[string]string{"server_id": "1"}, start, end)
	require.NoError(t, err)
	assert.NotEmpty(t, series)
}

func TestGetResolutionForRange(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected time.Duration
	}{
		{"30 minutes - raw data", 30 * time.Minute, 0},
		{"1 hour - raw data", 1 * time.Hour, 0},
		{"3 hours - 30 second aggregation", 3 * time.Hour, 30 * time.Second},
		{"6 hours - 30 second aggregation", 6 * time.Hour, 30 * time.Second},
		{"12 hours - 2 minute aggregation", 12 * time.Hour, 2 * time.Minute},
		{"1 day - 2 minute aggregation", 24 * time.Hour, 2 * time.Minute},
		{"3 days - 10 minute aggregation", 3 * 24 * time.Hour, 10 * time.Minute},
		{"7 days - 10 minute aggregation", 7 * 24 * time.Hour, 10 * time.Minute},
		{"14 days - 1 hour aggregation", 14 * 24 * time.Hour, 1 * time.Hour},
		{"30 days - 1 hour aggregation", 30 * 24 * time.Hour, 1 * time.Hour},
		{"60 days - 6 hour aggregation", 60 * 24 * time.Hour, 6 * time.Hour},
		{"90 days - 6 hour aggregation", 90 * 24 * time.Hour, 6 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetResolutionForRange(tt.duration)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestQueryAggregatedRange(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)

	for i := 0; i < 5; i++ {
		state := &model.HostState{
			CPU: float64(50 + i*10),
		}
		err := TSDBShared.WriteHostState(serverID, state)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	err := TSDBShared.flushBuffer()
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)
	labelMatchers := map[string]string{"server_id": "1"}

	avg, err := TSDBShared.QueryAggregatedRange(MetricHostCPU, labelMatchers, start, end, AggregationAvg)
	require.NoError(t, err)
	assert.InDelta(t, 70.0, avg, 1.0)

	max, err := TSDBShared.QueryAggregatedRange(MetricHostCPU, labelMatchers, start, end, AggregationMax)
	require.NoError(t, err)
	assert.InDelta(t, 90.0, max, 1.0)

	min, err := TSDBShared.QueryAggregatedRange(MetricHostCPU, labelMatchers, start, end, AggregationMin)
	require.NoError(t, err)
	assert.InDelta(t, 50.0, min, 1.0)

	sum, err := TSDBShared.QueryAggregatedRange(MetricHostCPU, labelMatchers, start, end, AggregationSum)
	require.NoError(t, err)
	assert.InDelta(t, 350.0, sum, 5.0)
}

func TestAggregatePoints(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	baseTime := time.Now().UnixMilli()
	points := []DataPoint{
		{Timestamp: baseTime, Value: 10},
		{Timestamp: baseTime + 1000, Value: 20},
		{Timestamp: baseTime + 2000, Value: 30},
		{Timestamp: baseTime + 60000, Value: 40},
		{Timestamp: baseTime + 61000, Value: 50},
		{Timestamp: baseTime + 120000, Value: 60},
	}

	result := TSDBShared.aggregatePoints(points, 1*time.Minute)

	assert.Equal(t, 3, len(result))
	if len(result) >= 3 {
		assert.InDelta(t, 20.0, result[0].Value, 0.1)
		assert.InDelta(t, 45.0, result[1].Value, 0.1)
		assert.InDelta(t, 60.0, result[2].Value, 0.1)
	}
}

func TestMultipleServersWrite(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	for serverID := uint64(1); serverID <= 3; serverID++ {
		state := &model.HostState{
			CPU:     float64(serverID * 20),
			MemUsed: serverID * 1024 * 1024 * 1024,
		}
		err := TSDBShared.WriteHostState(serverID, state)
		require.NoError(t, err)
	}

	err := TSDBShared.flushBuffer()
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	for serverID := uint64(1); serverID <= 3; serverID++ {
		cpu, _, err := TSDBShared.QueryLatestValue(MetricHostCPU, map[string]string{
			"server_id": string('0' + byte(serverID)),
		})
		require.NoError(t, err)
		assert.InDelta(t, float64(serverID*20), cpu, 0.1)
	}

	_, _, err = TSDBShared.QueryLatestValue(MetricHostCPU, map[string]string{"server_id": "999"})
	assert.Error(t, err)
}

func TestGetHostState(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)
	originalState := &model.HostState{
		CPU:            75.5,
		MemUsed:        2 * 1024 * 1024 * 1024,
		SwapUsed:       256 * 1024 * 1024,
		DiskUsed:       100 * 1024 * 1024 * 1024,
		NetInSpeed:     5000,
		NetOutSpeed:    3000,
		Load1:          2.5,
		Load5:          2.0,
		Load15:         1.5,
		TcpConnCount:   500,
		UdpConnCount:   100,
		ProcessCount:   300,
	}

	err := TSDBShared.WriteHostState(serverID, originalState)
	require.NoError(t, err)

	err = TSDBShared.flushBuffer()
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	state, err := TSDBShared.GetHostState(serverID)
	require.NoError(t, err)
	assert.NotNil(t, state)

	assert.InDelta(t, originalState.CPU, state.CPU, 0.1)
	assert.Equal(t, originalState.MemUsed, state.MemUsed)
	assert.InDelta(t, originalState.Load1, state.Load1, 0.1)
}

func TestDataRetentionConfig(t *testing.T) {
	assert.Equal(t, 90*24*time.Hour, DataRetentionDuration)
	assert.Equal(t, 1*time.Hour, QueryRangeRaw)
	assert.Equal(t, 6*time.Hour, QueryRange30Sec)
	assert.Equal(t, 24*time.Hour, QueryRange2Min)
	assert.Equal(t, 7*24*time.Hour, QueryRange10Min)
	assert.Equal(t, 30*24*time.Hour, QueryRange1Hour)
}

func TestBufferFlush(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	state := &model.HostState{CPU: 50.0}
	err := TSDBShared.WriteHostState(1, state)
	require.NoError(t, err)

	TSDBShared.writeMu.Lock()
	bufferLen := len(TSDBShared.writeBuffer)
	TSDBShared.writeMu.Unlock()
	assert.Greater(t, bufferLen, 0)

	err = TSDBShared.flushBuffer()
	require.NoError(t, err)

	TSDBShared.writeMu.Lock()
	bufferLen = len(TSDBShared.writeBuffer)
	TSDBShared.writeMu.Unlock()
	assert.Equal(t, 0, bufferLen)
}

func TestQueryEmptyData(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	end := time.Now()
	start := end.Add(-5 * time.Minute)

	series, err := TSDBShared.QueryRange(MetricHostCPU, map[string]string{"server_id": "999"}, start, end)
	require.NoError(t, err)
	assert.Empty(t, series)

	_, _, err = TSDBShared.QueryLatestValue(MetricHostCPU, map[string]string{"server_id": "999"})
	assert.Error(t, err)
}

func TestQueryHostHistory(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)

	// 写入多个时间点的数据
	for i := 0; i < 5; i++ {
		state := &model.HostState{
			CPU:        float64(50 + i*5),
			MemUsed:    uint64(1024 * 1024 * 1024 * (i + 1)),
			NetInSpeed: uint64(1000 * (i + 1)),
		}
		err := TSDBShared.WriteHostState(serverID, state)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	err := TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)

	data, err := QueryHostHistory(serverID, start, end)
	require.NoError(t, err)
	assert.NotNil(t, data)
	assert.NotEmpty(t, data.Timestamps)
	assert.NotEmpty(t, data.CPU)
}

func TestQueryHostHistoryByMetric(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)
	state := &model.HostState{
		CPU:   75.5,
		Load1: 2.5,
	}

	err := TSDBShared.WriteHostState(serverID, state)
	require.NoError(t, err)
	err = TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)

	// 测试 CPU 查询
	points, err := QueryHostHistoryByMetric(serverID, "cpu", start, end)
	require.NoError(t, err)
	assert.NotEmpty(t, points)
	if len(points) > 0 {
		assert.InDelta(t, 75.5, points[0].Value, 0.1)
	}

	// 测试 Load1 查询
	points, err = QueryHostHistoryByMetric(serverID, "load1", start, end)
	require.NoError(t, err)
	assert.NotEmpty(t, points)

	// 测试无效指标
	_, err = QueryHostHistoryByMetric(serverID, "invalid_metric", start, end)
	assert.Error(t, err)
}

func TestQueryServiceHistory(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serviceID := uint64(1)
	serverID := uint64(0)

	// 写入多个数据点
	for i := 0; i < 3; i++ {
		err := TSDBShared.WriteServiceHistory(serviceID, serverID, float32(10+i*5), uint64(5+i), uint64(i))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	err := TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)

	points, err := QueryServiceHistory(serviceID, serverID, start, end)
	require.NoError(t, err)
	assert.NotEmpty(t, points)
}

func TestQueryTransferHistory(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)

	// 写入流量数据
	for i := 0; i < 3; i++ {
		err := TSDBShared.WriteTransfer(serverID, uint64(1000*(i+1)), uint64(2000*(i+1)))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	err := TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)

	data, err := QueryTransferHistory(serverID, start, end)
	require.NoError(t, err)
	assert.NotNil(t, data)
	assert.NotEmpty(t, data.Timestamps)
	assert.NotEmpty(t, data.In)
	assert.NotEmpty(t, data.Out)
}

func TestGetCycleTransfer(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)

	// 写入流量数据
	err := TSDBShared.WriteTransfer(serverID, 1000, 2000)
	require.NoError(t, err)
	err = TSDBShared.WriteTransfer(serverID, 500, 800)
	require.NoError(t, err)

	err = TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	start := time.Now().Add(-1 * time.Hour)

	// 测试入站流量
	inTransfer, err := GetCycleTransfer(serverID, start, "in")
	require.NoError(t, err)
	assert.Greater(t, inTransfer, uint64(0))

	// 测试出站流量
	outTransfer, err := GetCycleTransfer(serverID, start, "out")
	require.NoError(t, err)
	assert.Greater(t, outTransfer, uint64(0))

	// 测试总流量
	allTransfer, err := GetCycleTransfer(serverID, start, "all")
	require.NoError(t, err)
	assert.Equal(t, inTransfer+outTransfer, allTransfer)
}

func TestGetMetricForAlert(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)
	state := &model.HostState{
		CPU:     85.5,
		MemUsed: 2 * 1024 * 1024 * 1024,
		Load1:   3.5,
	}

	err := TSDBShared.WriteHostState(serverID, state)
	require.NoError(t, err)
	err = TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	// 测试 CPU 告警查询
	cpu, err := GetMetricForAlert(serverID, "cpu")
	require.NoError(t, err)
	assert.InDelta(t, 85.5, cpu, 0.1)

	// 测试 Load 告警查询
	load, err := GetMetricForAlert(serverID, "load1")
	require.NoError(t, err)
	assert.InDelta(t, 3.5, load, 0.1)
}

func TestQueryTransferSumInRange(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)

	// 写入多个流量数据点
	err := TSDBShared.WriteTransfer(serverID, 1000, 2000)
	require.NoError(t, err)
	err = TSDBShared.WriteTransfer(serverID, 1500, 2500)
	require.NoError(t, err)

	err = TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)

	// 测试入站总和
	inSum, err := QueryTransferSumInRange(serverID, start, end, "in")
	require.NoError(t, err)
	assert.Greater(t, inSum, uint64(0))

	// 测试出站总和
	outSum, err := QueryTransferSumInRange(serverID, start, end, "out")
	require.NoError(t, err)
	assert.Greater(t, outSum, uint64(0))

	// 测试总流量
	allSum, err := QueryTransferSumInRange(serverID, start, end, "all")
	require.NoError(t, err)
	assert.Equal(t, inSum+outSum, allSum)

	// 测试无效类型
	_, err = QueryTransferSumInRange(serverID, start, end, "invalid")
	assert.Error(t, err)
}

func TestTSDBNotInitialized(t *testing.T) {
	// 确保 TSDB 未初始化时返回错误
	oldTSDB := TSDBShared
	TSDBShared = nil
	defer func() { TSDBShared = oldTSDB }()

	_, err := QueryHostHistory(1, time.Now().Add(-1*time.Hour), time.Now())
	assert.Error(t, err)

	_, err = QueryHostHistoryByMetric(1, "cpu", time.Now().Add(-1*time.Hour), time.Now())
	assert.Error(t, err)

	_, err = QueryServiceHistory(1, 0, time.Now().Add(-1*time.Hour), time.Now())
	assert.Error(t, err)

	_, err = QueryTransferHistory(1, time.Now().Add(-1*time.Hour), time.Now())
	assert.Error(t, err)

	_, err = GetCycleTransfer(1, time.Now().Add(-1*time.Hour), "in")
	assert.Error(t, err)

	_, err = GetMetricForAlert(1, "cpu")
	assert.Error(t, err)
}

func TestAlertTSDBIntegration(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serverID := uint64(1)

	// 写入流量数据用于周期流量告警测试
	for i := 0; i < 5; i++ {
		err := TSDBShared.WriteTransfer(serverID, uint64(1000*(i+1)), uint64(2000*(i+1)))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	err := TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	// 创建 TSDB 查询函数
	tsdbQuery := func(sID uint64, start time.Time, transferType string) (uint64, error) {
		return GetCycleTransfer(sID, start, transferType)
	}

	// 测试入站流量查询
	startTime := time.Now().Add(-1 * time.Hour)
	inTransfer, err := tsdbQuery(serverID, startTime, "in")
	require.NoError(t, err)
	assert.Greater(t, inTransfer, uint64(0))

	// 测试出站流量查询
	outTransfer, err := tsdbQuery(serverID, startTime, "out")
	require.NoError(t, err)
	assert.Greater(t, outTransfer, uint64(0))

	// 测试总流量查询
	allTransfer, err := tsdbQuery(serverID, startTime, "all")
	require.NoError(t, err)
	assert.Equal(t, inTransfer+outTransfer, allTransfer)
}

func TestQueryServiceDailySummary(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	serviceID := uint64(1)
	serverID := uint64(0)

	// 写入多个数据点
	for i := 0; i < 10; i++ {
		err := TSDBShared.WriteServiceHistory(serviceID, serverID, float32(10+i), uint64(8+i%3), uint64(i%2))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	err := TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	summary, err := QueryServiceDailySummary(serviceID, 7)
	require.NoError(t, err)
	assert.NotNil(t, summary)
	assert.Len(t, summary, 7)
}

func TestAggregatePointsWithZeroResolution(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	// 测试当 resolution 为 0 时，aggregatePoints 应该返回原始数据
	points := []DataPoint{
		{Timestamp: 1000, Value: 10.0},
		{Timestamp: 2000, Value: 20.0},
		{Timestamp: 3000, Value: 30.0},
	}

	// 用 0 resolution 测试
	result := TSDBShared.aggregatePoints(points, 0)
	assert.Equal(t, points, result)

	// 用负 resolution 测试
	result = TSDBShared.aggregatePoints(points, -1*time.Second)
	assert.Equal(t, points, result)
}

func TestWriteReadMultipleServers(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	// 写入多个服务器的数据
	for serverID := uint64(1); serverID <= 3; serverID++ {
		state := &model.HostState{
			CPU:     float64(50 + serverID*10),
			MemUsed: uint64(1024 * 1024 * 1024 * serverID),
		}
		err := TSDBShared.WriteHostState(serverID, state)
		require.NoError(t, err)
	}

	err := TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)

	// 分别读取每个服务器的数据
	for serverID := uint64(1); serverID <= 3; serverID++ {
		data, err := QueryHostHistory(serverID, start, end)
		require.NoError(t, err)
		assert.NotNil(t, data)
		assert.NotEmpty(t, data.CPU)

		// 验证 CPU 值与写入的匹配
		if len(data.CPU) > 0 {
			expectedCPU := float64(50 + serverID*10)
			assert.InDelta(t, expectedCPU, data.CPU[0], 0.1)
		}
	}
}

func TestWriteWAFBlock(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	// 写入多个 WAF 封禁事件
	testIPs := []string{"192.168.1.1", "10.0.0.1", "192.168.1.1"} // 192.168.1.1 出现两次
	reasons := []uint8{1, 2, 1}

	for i, ip := range testIPs {
		err := TSDBShared.WriteWAFBlock(ip, reasons[i], 1)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	err := TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)

	// 查询 WAF 数据
	series, err := TSDBShared.QueryRange(MetricWAFBlock, nil, start, end)
	require.NoError(t, err)
	assert.NotEmpty(t, series)
}

func TestQueryWAFStats(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	// 写入多个 WAF 封禁事件
	testCases := []struct {
		ip     string
		reason uint8
		count  uint64
	}{
		{"192.168.1.1", 1, 1},
		{"192.168.1.1", 1, 1},
		{"10.0.0.1", 2, 1},
		{"172.16.0.1", 1, 1},
	}

	for _, tc := range testCases {
		err := TSDBShared.WriteWAFBlock(tc.ip, tc.reason, tc.count)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	err := TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-1 * time.Hour)

	stats, err := QueryWAFStats(start, end, 10)
	require.NoError(t, err)
	assert.NotNil(t, stats)

	// 验证统计数据
	assert.Equal(t, uint64(4), stats.TotalBlocks)
	assert.Equal(t, 3, stats.UniqueIPs) // 3 个不同 IP

	// 验证按原因分类
	assert.Equal(t, uint64(3), stats.BlocksByReason[1]) // reason 1 出现 3 次
	assert.Equal(t, uint64(1), stats.BlocksByReason[2]) // reason 2 出现 1 次

	// 验证 Top IP
	assert.NotEmpty(t, stats.TopBlockedIPs)
	assert.Equal(t, "192.168.1.1", stats.TopBlockedIPs[0].IP)
	assert.Equal(t, uint64(2), stats.TopBlockedIPs[0].Blocks)
}

func TestQueryWAFBlocksByIP(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	targetIP := "192.168.1.100"

	// 写入多个封禁事件
	for i := 0; i < 3; i++ {
		err := TSDBShared.WriteWAFBlock(targetIP, uint8(i%2+1), 1)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	// 写入其他 IP 的事件
	err := TSDBShared.WriteWAFBlock("10.0.0.1", 1, 1)
	require.NoError(t, err)

	err = TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-1 * time.Hour)

	events, err := QueryWAFBlocksByIP(targetIP, start, end)
	require.NoError(t, err)
	assert.Len(t, events, 3) // 只有 targetIP 的 3 个事件

	for _, event := range events {
		assert.Equal(t, targetIP, event.IP)
	}
}

func TestWriteWAFBlockSingle(t *testing.T) {
	_, cleanup := setupTestTSDB(t)
	defer cleanup()

	// 直接使用 TSDBShared.WriteWAFBlock 测试 TSDB 写入
	err := TSDBShared.WriteWAFBlock("192.168.1.1", 1, 1)
	require.NoError(t, err)

	err = TSDBShared.flushBuffer()
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	end := time.Now()
	start := end.Add(-5 * time.Minute)

	events, err := QueryWAFBlocksByIP("192.168.1.1", start, end)
	require.NoError(t, err)
	assert.Len(t, events, 1)
}

func TestQueryWAFStatsNotInitialized(t *testing.T) {
	// 确保 TSDB 未初始化时返回错误
	oldTSDB := TSDBShared
	TSDBShared = nil
	defer func() { TSDBShared = oldTSDB }()

	_, err := QueryWAFStats(time.Now().Add(-1*time.Hour), time.Now(), 10)
	assert.Error(t, err)

	_, err = QueryWAFBlocksByIP("192.168.1.1", time.Now().Add(-1*time.Hour), time.Now())
	assert.Error(t, err)
}

