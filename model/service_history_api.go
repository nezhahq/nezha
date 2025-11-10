package model

type ServiceInfos struct {
	ServiceID   uint64    `json:"monitor_id"`
	ServerID    uint64    `json:"server_id"`
	ServiceName string    `json:"monitor_name"`
	ServerName  string    `json:"server_name"`
	CreatedAt   []int64   `json:"created_at"`
	AvgDelay    []float32 `json:"avg_delay"`
}
