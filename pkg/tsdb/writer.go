package tsdb

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/prompb"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
)

type bufferedWriter struct {
	db          *TSDB
	buffer      []storage.MetricRow
	mu          sync.Mutex
	maxSize     int
	flushTicker *time.Ticker
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

func newBufferedWriter(db *TSDB, maxSize int, flushInterval time.Duration) *bufferedWriter {
	w := &bufferedWriter{
		db:          db,
		buffer:      make([]storage.MetricRow, 0, maxSize),
		maxSize:     maxSize,
		flushTicker: time.NewTicker(flushInterval),
		stopCh:      make(chan struct{}),
	}
	w.wg.Add(1)
	go w.flushLoop()
	return w
}

func (w *bufferedWriter) flushLoop() {
	defer w.wg.Done()
	for {
		select {
		case <-w.flushTicker.C:
			w.flush()
		case <-w.stopCh:
			w.flush()
			return
		}
	}
}

func (w *bufferedWriter) write(rows []storage.MetricRow) {
	w.mu.Lock()
	w.buffer = append(w.buffer, rows...)
	if len(w.buffer) >= w.maxSize {
		rows := w.buffer
		w.buffer = make([]storage.MetricRow, 0, w.maxSize)
		w.mu.Unlock()
		w.db.storage.AddRows(rows, 64)
		return
	}
	w.mu.Unlock()
}

func (w *bufferedWriter) flush() {
	w.mu.Lock()
	if len(w.buffer) == 0 {
		w.mu.Unlock()
		return
	}
	rows := w.buffer
	w.buffer = make([]storage.MetricRow, 0, w.maxSize)
	w.mu.Unlock()

	w.db.storage.AddRows(rows, 64)
}

func (w *bufferedWriter) stop() {
	w.flushTicker.Stop()
	close(w.stopCh)
	w.wg.Wait()
}

// MetricType 指标类型
type MetricType string

const (
	// 服务器指标
	MetricServerCPU            MetricType = "nezha_server_cpu"
	MetricServerMemory         MetricType = "nezha_server_memory"
	MetricServerSwap           MetricType = "nezha_server_swap"
	MetricServerDisk           MetricType = "nezha_server_disk"
	MetricServerNetInSpeed     MetricType = "nezha_server_net_in_speed"
	MetricServerNetOutSpeed    MetricType = "nezha_server_net_out_speed"
	MetricServerNetInTransfer  MetricType = "nezha_server_net_in_transfer"
	MetricServerNetOutTransfer MetricType = "nezha_server_net_out_transfer"
	MetricServerLoad1          MetricType = "nezha_server_load1"
	MetricServerLoad5          MetricType = "nezha_server_load5"
	MetricServerLoad15         MetricType = "nezha_server_load15"
	MetricServerTCPConn        MetricType = "nezha_server_tcp_conn"
	MetricServerUDPConn        MetricType = "nezha_server_udp_conn"
	MetricServerProcessCount   MetricType = "nezha_server_process_count"
	MetricServerTemperature    MetricType = "nezha_server_temperature"
	MetricServerUptime         MetricType = "nezha_server_uptime"
	MetricServerGPU            MetricType = "nezha_server_gpu"

	// 服务监控指标
	MetricServiceDelay  MetricType = "nezha_service_delay"
	MetricServiceStatus MetricType = "nezha_service_status"
)

// ServerMetrics 服务器指标数据
type ServerMetrics struct {
	ServerID       uint64
	Timestamp      time.Time
	CPU            float64
	MemUsed        uint64
	SwapUsed       uint64
	DiskUsed       uint64
	NetInSpeed     uint64
	NetOutSpeed    uint64
	NetInTransfer  uint64
	NetOutTransfer uint64
	Load1          float64
	Load5          float64
	Load15         float64
	TCPConnCount   uint64
	UDPConnCount   uint64
	ProcessCount   uint64
	Temperature    float64
	Uptime         uint64
	GPU            float64
}

// ServiceMetrics 服务监控指标数据
type ServiceMetrics struct {
	ServiceID  uint64
	ServerID   uint64
	Timestamp  time.Time
	Delay      float64
	Successful bool
}

func (db *TSDB) WriteServerMetrics(m *ServerMetrics) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return fmt.Errorf("TSDB is closed")
	}

	ts := m.Timestamp.UnixMilli()
	serverIDStr := strconv.FormatUint(m.ServerID, 10)

	rows := []storage.MetricRow{
		makeServerMetricRow(MetricServerCPU, serverIDStr, ts, m.CPU),
		makeServerMetricRow(MetricServerMemory, serverIDStr, ts, float64(m.MemUsed)),
		makeServerMetricRow(MetricServerSwap, serverIDStr, ts, float64(m.SwapUsed)),
		makeServerMetricRow(MetricServerDisk, serverIDStr, ts, float64(m.DiskUsed)),
		makeServerMetricRow(MetricServerNetInSpeed, serverIDStr, ts, float64(m.NetInSpeed)),
		makeServerMetricRow(MetricServerNetOutSpeed, serverIDStr, ts, float64(m.NetOutSpeed)),
		makeServerMetricRow(MetricServerNetInTransfer, serverIDStr, ts, float64(m.NetInTransfer)),
		makeServerMetricRow(MetricServerNetOutTransfer, serverIDStr, ts, float64(m.NetOutTransfer)),
		makeServerMetricRow(MetricServerLoad1, serverIDStr, ts, m.Load1),
		makeServerMetricRow(MetricServerLoad5, serverIDStr, ts, m.Load5),
		makeServerMetricRow(MetricServerLoad15, serverIDStr, ts, m.Load15),
		makeServerMetricRow(MetricServerTCPConn, serverIDStr, ts, float64(m.TCPConnCount)),
		makeServerMetricRow(MetricServerUDPConn, serverIDStr, ts, float64(m.UDPConnCount)),
		makeServerMetricRow(MetricServerProcessCount, serverIDStr, ts, float64(m.ProcessCount)),
		makeServerMetricRow(MetricServerTemperature, serverIDStr, ts, m.Temperature),
		makeServerMetricRow(MetricServerUptime, serverIDStr, ts, float64(m.Uptime)),
		makeServerMetricRow(MetricServerGPU, serverIDStr, ts, m.GPU),
	}

	if db.writer != nil {
		db.writer.write(rows)
	} else {
		db.storage.AddRows(rows, 64)
	}
	return nil
}

func (db *TSDB) WriteServiceMetrics(m *ServiceMetrics) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return fmt.Errorf("TSDB is closed")
	}

	ts := m.Timestamp.UnixMilli()
	serviceIDStr := strconv.FormatUint(m.ServiceID, 10)
	serverIDStr := strconv.FormatUint(m.ServerID, 10)

	var status float64
	if m.Successful {
		status = 1
	}

	rows := []storage.MetricRow{
		makeServiceMetricRow(MetricServiceDelay, serviceIDStr, serverIDStr, ts, m.Delay),
		makeServiceMetricRow(MetricServiceStatus, serviceIDStr, serverIDStr, ts, status),
	}

	if db.writer != nil {
		db.writer.write(rows)
	} else {
		db.storage.AddRows(rows, 64)
	}
	return nil
}

func makeServerMetricRow(metric MetricType, serverID string, timestamp int64, value float64) storage.MetricRow {
	labels := []prompb.Label{
		{Name: "__name__", Value: string(metric)},
		{Name: "server_id", Value: serverID},
	}
	return storage.MetricRow{
		MetricNameRaw: storage.MarshalMetricNameRaw(nil, labels),
		Timestamp:     timestamp,
		Value:         value,
	}
}

func makeServiceMetricRow(metric MetricType, serviceID, serverID string, timestamp int64, value float64) storage.MetricRow {
	labels := []prompb.Label{
		{Name: "__name__", Value: string(metric)},
		{Name: "service_id", Value: serviceID},
		{Name: "server_id", Value: serverID},
	}
	return storage.MetricRow{
		MetricNameRaw: storage.MarshalMetricNameRaw(nil, labels),
		Timestamp:     timestamp,
		Value:         value,
	}
}

func (db *TSDB) WriteBatchServerMetrics(metrics []*ServerMetrics) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return fmt.Errorf("TSDB is closed")
	}

	rows := make([]storage.MetricRow, 0, len(metrics)*17)
	for _, m := range metrics {
		ts := m.Timestamp.UnixMilli()
		serverIDStr := strconv.FormatUint(m.ServerID, 10)
		rows = append(rows,
			makeServerMetricRow(MetricServerCPU, serverIDStr, ts, m.CPU),
			makeServerMetricRow(MetricServerMemory, serverIDStr, ts, float64(m.MemUsed)),
			makeServerMetricRow(MetricServerSwap, serverIDStr, ts, float64(m.SwapUsed)),
			makeServerMetricRow(MetricServerDisk, serverIDStr, ts, float64(m.DiskUsed)),
			makeServerMetricRow(MetricServerNetInSpeed, serverIDStr, ts, float64(m.NetInSpeed)),
			makeServerMetricRow(MetricServerNetOutSpeed, serverIDStr, ts, float64(m.NetOutSpeed)),
			makeServerMetricRow(MetricServerNetInTransfer, serverIDStr, ts, float64(m.NetInTransfer)),
			makeServerMetricRow(MetricServerNetOutTransfer, serverIDStr, ts, float64(m.NetOutTransfer)),
			makeServerMetricRow(MetricServerLoad1, serverIDStr, ts, m.Load1),
			makeServerMetricRow(MetricServerLoad5, serverIDStr, ts, m.Load5),
			makeServerMetricRow(MetricServerLoad15, serverIDStr, ts, m.Load15),
			makeServerMetricRow(MetricServerTCPConn, serverIDStr, ts, float64(m.TCPConnCount)),
			makeServerMetricRow(MetricServerUDPConn, serverIDStr, ts, float64(m.UDPConnCount)),
			makeServerMetricRow(MetricServerProcessCount, serverIDStr, ts, float64(m.ProcessCount)),
			makeServerMetricRow(MetricServerTemperature, serverIDStr, ts, m.Temperature),
			makeServerMetricRow(MetricServerUptime, serverIDStr, ts, float64(m.Uptime)),
			makeServerMetricRow(MetricServerGPU, serverIDStr, ts, m.GPU),
		)
	}

	if db.writer != nil {
		db.writer.write(rows)
	} else {
		db.storage.AddRows(rows, 64)
	}
	return nil
}

func (db *TSDB) WriteBatchServiceMetrics(metrics []*ServiceMetrics) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return fmt.Errorf("TSDB is closed")
	}

	rows := make([]storage.MetricRow, 0, len(metrics)*2)
	for _, m := range metrics {
		ts := m.Timestamp.UnixMilli()
		serviceIDStr := strconv.FormatUint(m.ServiceID, 10)
		serverIDStr := strconv.FormatUint(m.ServerID, 10)
		var status float64
		if m.Successful {
			status = 1
		}
		rows = append(rows,
			makeServiceMetricRow(MetricServiceDelay, serviceIDStr, serverIDStr, ts, m.Delay),
			makeServiceMetricRow(MetricServiceStatus, serviceIDStr, serverIDStr, ts, status),
		)
	}

	if db.writer != nil {
		db.writer.write(rows)
	} else {
		db.storage.AddRows(rows, 64)
	}
	return nil
}
