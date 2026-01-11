package controller

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// HistoryQueryRequest 历史数据查询请求
type HistoryQueryRequest struct {
	Start  int64  `form:"start"`  // Unix timestamp (秒)
	End    int64  `form:"end"`    // Unix timestamp (秒)
	Metric string `form:"metric"` // 指标类型 (可选)
}

// HostHistoryResponse 主机历史数据响应
type HostHistoryResponse struct {
	ServerID   uint64                      `json:"server_id"`
	ServerName string                      `json:"server_name"`
	Data       *singleton.HostHistoryData  `json:"data"`
	Resolution string                      `json:"resolution"` // 数据分辨率说明
}

// MetricHistoryResponse 单指标历史数据响应
type MetricHistoryResponse struct {
	ServerID   uint64  `json:"server_id"`
	Metric     string  `json:"metric"`
	Timestamps []int64 `json:"timestamps"`
	Values     []float64 `json:"values"`
	Resolution string  `json:"resolution"`
}

// TransferHistoryResponse 流量历史数据响应
type TransferHistoryResponse struct {
	ServerID   uint64    `json:"server_id"`
	Timestamps []int64   `json:"timestamps"`
	In         []float64 `json:"in"`
	Out        []float64 `json:"out"`
	Resolution string    `json:"resolution"`
}

// ServiceHistoryResponse 服务监控历史数据响应
type ServiceHistoryResponse struct {
	ServiceID   uint64                          `json:"service_id"`
	ServiceName string                          `json:"service_name"`
	Timestamps  []int64                         `json:"timestamps"`
	AvgDelay    []float32                       `json:"avg_delay"`
	Up          []uint64                        `json:"up"`
	Down        []uint64                        `json:"down"`
	Resolution  string                          `json:"resolution"`
}

// Get server history
// @Summary Get server history from TSDB
// @Security BearerAuth
// @Schemes
// @Description Get server monitoring history data
// @Tags auth required
// @Param id path uint true "Server ID"
// @Param start query int64 false "Start timestamp (Unix seconds)"
// @Param end query int64 false "End timestamp (Unix seconds)"
// @Produce json
// @Success 200 {object} model.CommonResponse[HostHistoryResponse]
// @Router /server/{id}/history [get]
func getServerHistory(c *gin.Context) (*HostHistoryResponse, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	server, ok := singleton.ServerShared.Get(id)
	if !ok || server == nil {
		return nil, singleton.Localizer.ErrorT("server not found")
	}

	_, isMember := c.Get(model.CtxKeyAuthorizedUser)
	if server.HideForGuest && !isMember {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}

	var req HistoryQueryRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		return nil, err
	}

	// 默认时间范围：最近 1 小时
	end := time.Now()
	start := end.Add(-1 * time.Hour)

	if req.End > 0 {
		end = time.Unix(req.End, 0)
	}
	if req.Start > 0 {
		start = time.Unix(req.Start, 0)
	}

	// 限制最大查询范围为 90 天
	if end.Sub(start) > 90*24*time.Hour {
		start = end.Add(-90 * 24 * time.Hour)
	}

	data, err := singleton.QueryHostHistory(id, start, end)
	if err != nil {
		return nil, err
	}

	duration := end.Sub(start)
	resolution := singleton.GetResolutionForRange(duration)

	return &HostHistoryResponse{
		ServerID:   id,
		ServerName: server.Name,
		Data:       data,
		Resolution: formatResolution(resolution),
	}, nil
}

// Get server metric history
// @Summary Get server metric history from TSDB
// @Security BearerAuth
// @Schemes
// @Description Get specific metric history data for a server
// @Tags auth required
// @Param id path uint true "Server ID"
// @Param metric query string true "Metric type (cpu, memory, disk, net_in_speed, net_out_speed, load1, load5, load15, etc.)"
// @Param start query int64 false "Start timestamp (Unix seconds)"
// @Param end query int64 false "End timestamp (Unix seconds)"
// @Produce json
// @Success 200 {object} model.CommonResponse[MetricHistoryResponse]
// @Router /server/{id}/history/metric [get]
func getServerMetricHistory(c *gin.Context) (*MetricHistoryResponse, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	server, ok := singleton.ServerShared.Get(id)
	if !ok || server == nil {
		return nil, singleton.Localizer.ErrorT("server not found")
	}

	_, isMember := c.Get(model.CtxKeyAuthorizedUser)
	if server.HideForGuest && !isMember {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}

	var req HistoryQueryRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		return nil, err
	}

	if req.Metric == "" {
		return nil, singleton.Localizer.ErrorT("metric parameter is required")
	}

	end := time.Now()
	start := end.Add(-1 * time.Hour)

	if req.End > 0 {
		end = time.Unix(req.End, 0)
	}
	if req.Start > 0 {
		start = time.Unix(req.Start, 0)
	}

	if end.Sub(start) > 90*24*time.Hour {
		start = end.Add(-90 * 24 * time.Hour)
	}

	points, err := singleton.QueryHostHistoryByMetric(id, req.Metric, start, end)
	if err != nil {
		return nil, err
	}

	timestamps := make([]int64, len(points))
	values := make([]float64, len(points))
	for i, p := range points {
		timestamps[i] = p.Timestamp
		values[i] = p.Value
	}

	duration := end.Sub(start)
	resolution := singleton.GetResolutionForRange(duration)

	return &MetricHistoryResponse{
		ServerID:   id,
		Metric:     req.Metric,
		Timestamps: timestamps,
		Values:     values,
		Resolution: formatResolution(resolution),
	}, nil
}

// Get server transfer history
// @Summary Get server transfer history from TSDB
// @Security BearerAuth
// @Schemes
// @Description Get network transfer history data for a server
// @Tags auth required
// @Param id path uint true "Server ID"
// @Param start query int64 false "Start timestamp (Unix seconds)"
// @Param end query int64 false "End timestamp (Unix seconds)"
// @Produce json
// @Success 200 {object} model.CommonResponse[TransferHistoryResponse]
// @Router /server/{id}/transfer/history [get]
func getServerTransferHistory(c *gin.Context) (*TransferHistoryResponse, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	server, ok := singleton.ServerShared.Get(id)
	if !ok || server == nil {
		return nil, singleton.Localizer.ErrorT("server not found")
	}

	_, isMember := c.Get(model.CtxKeyAuthorizedUser)
	if server.HideForGuest && !isMember {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}

	var req HistoryQueryRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		return nil, err
	}

	end := time.Now()
	start := end.Add(-24 * time.Hour)

	if req.End > 0 {
		end = time.Unix(req.End, 0)
	}
	if req.Start > 0 {
		start = time.Unix(req.Start, 0)
	}

	if end.Sub(start) > 90*24*time.Hour {
		start = end.Add(-90 * 24 * time.Hour)
	}

	data, err := singleton.QueryTransferHistory(id, start, end)
	if err != nil {
		return nil, err
	}

	duration := end.Sub(start)
	resolution := singleton.GetResolutionForRange(duration)

	return &TransferHistoryResponse{
		ServerID:   id,
		Timestamps: data.Timestamps,
		In:         data.In,
		Out:        data.Out,
		Resolution: formatResolution(resolution),
	}, nil
}

// Get service history from TSDB
// @Summary Get service monitoring history from TSDB
// @Security BearerAuth
// @Schemes
// @Description Get service monitoring history data
// @Tags common
// @Param id path uint true "Service ID"
// @Param start query int64 false "Start timestamp (Unix seconds)"
// @Param end query int64 false "End timestamp (Unix seconds)"
// @Produce json
// @Success 200 {object} model.CommonResponse[ServiceHistoryResponse]
// @Router /service/{id}/history [get]
func getServiceHistory(c *gin.Context) (*ServiceHistoryResponse, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	service, ok := singleton.ServiceSentinelShared.Get(id)
	if !ok {
		return nil, singleton.Localizer.ErrorT("service not found")
	}

	var req HistoryQueryRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		return nil, err
	}

	end := time.Now()
	start := end.Add(-24 * time.Hour)

	if req.End > 0 {
		end = time.Unix(req.End, 0)
	}
	if req.Start > 0 {
		start = time.Unix(req.Start, 0)
	}

	if end.Sub(start) > 90*24*time.Hour {
		start = end.Add(-90 * 24 * time.Hour)
	}

	points, err := singleton.QueryServiceHistory(id, 0, start, end)
	if err != nil {
		return nil, err
	}

	timestamps := make([]int64, len(points))
	avgDelays := make([]float32, len(points))
	ups := make([]uint64, len(points))
	downs := make([]uint64, len(points))

	for i, p := range points {
		timestamps[i] = p.Timestamp
		avgDelays[i] = p.AvgDelay
		ups[i] = p.Up
		downs[i] = p.Down
	}

	duration := end.Sub(start)
	resolution := singleton.GetResolutionForRange(duration)

	return &ServiceHistoryResponse{
		ServiceID:   id,
		ServiceName: service.Name,
		Timestamps:  timestamps,
		AvgDelay:    avgDelays,
		Up:          ups,
		Down:        downs,
		Resolution:  formatResolution(resolution),
	}, nil
}

// Get service daily summary
// @Summary Get service daily availability summary
// @Security BearerAuth
// @Schemes
// @Description Get service daily availability summary for the last N days
// @Tags common
// @Param id path uint true "Service ID"
// @Param days query int false "Number of days (default 30, max 90)"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]singleton.ServiceDailySummary]
// @Router /service/{id}/summary [get]
func getServiceDailySummary(c *gin.Context) ([]singleton.ServiceDailySummary, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	_, ok := singleton.ServiceSentinelShared.Get(id)
	if !ok {
		return nil, singleton.Localizer.ErrorT("service not found")
	}

	days := 30
	if daysStr := c.Query("days"); daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 {
			days = d
		}
	}
	if days > 90 {
		days = 90
	}

	return singleton.QueryServiceDailySummary(id, days)
}

// formatResolution 格式化分辨率为可读字符串
func formatResolution(d time.Duration) string {
	if d == 0 {
		return "raw"
	}
	if d < time.Minute {
		return d.String()
	}
	if d < time.Hour {
		return d.String()
	}
	return d.String()
}
