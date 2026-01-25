package tsdb

import (
	"fmt"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/prompb"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
)

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
	Delay      float32
	Successful bool
}

// WriteServerMetrics 写入服务器指标
func (db *TSDB) WriteServerMetrics(m *ServerMetrics) error {
	if db.IsClosed() {
		return fmt.Errorf("TSDB is closed")
	}

	ts := m.Timestamp.UnixMilli()
	serverIDStr := fmt.Sprintf("%d", m.ServerID)

	rows := []storage.MetricRow{
		db.makeMetricRow(MetricServerCPU, serverIDStr, "", ts, m.CPU),
		db.makeMetricRow(MetricServerMemory, serverIDStr, "", ts, float64(m.MemUsed)),
		db.makeMetricRow(MetricServerSwap, serverIDStr, "", ts, float64(m.SwapUsed)),
		db.makeMetricRow(MetricServerDisk, serverIDStr, "", ts, float64(m.DiskUsed)),
		db.makeMetricRow(MetricServerNetInSpeed, serverIDStr, "", ts, float64(m.NetInSpeed)),
		db.makeMetricRow(MetricServerNetOutSpeed, serverIDStr, "", ts, float64(m.NetOutSpeed)),
		db.makeMetricRow(MetricServerNetInTransfer, serverIDStr, "", ts, float64(m.NetInTransfer)),
		db.makeMetricRow(MetricServerNetOutTransfer, serverIDStr, "", ts, float64(m.NetOutTransfer)),
		db.makeMetricRow(MetricServerLoad1, serverIDStr, "", ts, m.Load1),
		db.makeMetricRow(MetricServerLoad5, serverIDStr, "", ts, m.Load5),
		db.makeMetricRow(MetricServerLoad15, serverIDStr, "", ts, m.Load15),
		db.makeMetricRow(MetricServerTCPConn, serverIDStr, "", ts, float64(m.TCPConnCount)),
		db.makeMetricRow(MetricServerUDPConn, serverIDStr, "", ts, float64(m.UDPConnCount)),
		db.makeMetricRow(MetricServerProcessCount, serverIDStr, "", ts, float64(m.ProcessCount)),
		db.makeMetricRow(MetricServerTemperature, serverIDStr, "", ts, m.Temperature),
		db.makeMetricRow(MetricServerUptime, serverIDStr, "", ts, float64(m.Uptime)),
		db.makeMetricRow(MetricServerGPU, serverIDStr, "", ts, m.GPU),
	}

	db.storage.AddRows(rows, 64)
	return nil
}

// WriteServiceMetrics 写入服务监控指标
func (db *TSDB) WriteServiceMetrics(m *ServiceMetrics) error {
	if db.IsClosed() {
		return fmt.Errorf("TSDB is closed")
	}

	ts := m.Timestamp.UnixMilli()
	serviceIDStr := fmt.Sprintf("%d", m.ServiceID)
	serverIDStr := fmt.Sprintf("%d", m.ServerID)

	var status float64
	if m.Successful {
		status = 1
	}

	rows := []storage.MetricRow{
		db.makeMetricRow(MetricServiceDelay, serviceIDStr, serverIDStr, ts, float64(m.Delay)),
		db.makeMetricRow(MetricServiceStatus, serviceIDStr, serverIDStr, ts, status),
	}

	db.storage.AddRows(rows, 64)
	return nil
}

// makeMetricRow 创建指标行
func (db *TSDB) makeMetricRow(metric MetricType, serviceOrServerID, serverID string, timestamp int64, value float64) storage.MetricRow {
	labels := []prompb.Label{
		{Name: "__name__", Value: string(metric)},
	}

	// 根据指标类型添加不同的标签
	if serverID != "" {
		// 服务监控指标：service_id + server_id
		labels = append(labels,
			prompb.Label{Name: "service_id", Value: serviceOrServerID},
			prompb.Label{Name: "server_id", Value: serverID},
		)
	} else {
		// 服务器指标：仅 server_id
		labels = append(labels,
			prompb.Label{Name: "server_id", Value: serviceOrServerID},
		)
	}

	return storage.MetricRow{
		MetricNameRaw: storage.MarshalMetricNameRaw(nil, labels),
		Timestamp:     timestamp,
		Value:         value,
	}
}

// WriteBatchServerMetrics 批量写入服务器指标
func (db *TSDB) WriteBatchServerMetrics(metrics []*ServerMetrics) error {
	for _, m := range metrics {
		if err := db.WriteServerMetrics(m); err != nil {
			return err
		}
	}
	return nil
}

// WriteBatchServiceMetrics 批量写入服务监控指标
func (db *TSDB) WriteBatchServiceMetrics(metrics []*ServiceMetrics) error {
	for _, m := range metrics {
		if err := db.WriteServiceMetrics(m); err != nil {
			return err
		}
	}
	return nil
}
