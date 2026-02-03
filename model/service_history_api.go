package model

type ServiceInfos struct {
	ServiceID    uint64    `json:"monitor_id"`
	ServerID     uint64    `json:"server_id"`
	ServiceName  string    `json:"monitor_name"`
	ServerName   string    `json:"server_name"`
	DisplayIndex int       `json:"display_index"` // 展示排序，越大越靠前
	CreatedAt    []int64   `json:"created_at"`
	AvgDelay     []float32 `json:"avg_delay"`
}
