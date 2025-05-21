type Monitor struct {
	trafficCollector *TrafficCollector
	// ... existing fields ...
}

func NewMonitor(trafficCollector *TrafficCollector) *Monitor {
	return &Monitor{
		trafficCollector: trafficCollector,
		// ... existing initialization ...
	}
}

func (m *Monitor) HandleState(state *pb.State, serverID uint) {
	// ... existing code ...
	
	// 更新流量数据
	m.trafficCollector.UpdateTraffic(serverID, state.NetInTransfer, state.NetOutTransfer)
	
	// ... existing code ...
}

// ... existing code ... 