package service

import (
	"time"
	"sync"
)

type TrafficCollector struct {
	trafficService *TrafficService
	servers        map[uint]*ServerTraffic
	mu            sync.RWMutex
}

type ServerTraffic struct {
	LastInBytes  int64
	LastOutBytes int64
	LastUpdate   time.Time
}

func NewTrafficCollector(trafficService *TrafficService) *TrafficCollector {
	return &TrafficCollector{
		trafficService: trafficService,
		servers:        make(map[uint]*ServerTraffic),
	}
}

// UpdateTraffic 更新服务器流量数据
func (c *TrafficCollector) UpdateTraffic(serverID uint, inBytes, outBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	server, exists := c.servers[serverID]
	if !exists {
		server = &ServerTraffic{
			LastInBytes:  inBytes,
			LastOutBytes: outBytes,
			LastUpdate:   now,
		}
		c.servers[serverID] = server
		return
	}

	// 计算增量流量
	inDiff := inBytes - server.LastInBytes
	outDiff := outBytes - server.LastOutBytes

	// 记录流量数据
	if err := c.trafficService.RecordTraffic(serverID, inDiff, outDiff); err != nil {
		// TODO: 处理错误
		return
	}

	// 更新最后记录的数据
	server.LastInBytes = inBytes
	server.LastOutBytes = outBytes
	server.LastUpdate = now
} 