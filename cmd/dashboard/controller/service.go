package controller

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/tsdb"
	"github.com/nezhahq/nezha/service/singleton"
)

// Show service
// @Summary Show service
// @Security BearerAuth
// @Schemes
// @Description Show service
// @Tags common
// @Produce json
// @Success 200 {object} model.CommonResponse[model.ServiceResponse]
// @Router /service [get]
func showService(c *gin.Context) (*model.ServiceResponse, error) {
	res, err, _ := requestGroup.Do(serviceResponseCacheKey(c), func() (any, error) {
		singleton.AlertsLock.RLock()
		defer singleton.AlertsLock.RUnlock()
		stats := filterServiceStatsForViewer(c, singleton.ServiceSentinelShared.CopyStats())
		var cycleTransferStats map[uint64]model.CycleTransferStats
		copier.Copy(&cycleTransferStats, singleton.AlertsCycleTransferStatsStore)
		return []any{
			stats, filterCycleTransferStatsForViewer(c, cycleTransferStats),
		}, nil
	})
	if err != nil {
		return nil, err
	}

	return &model.ServiceResponse{
		Services:           res.([]any)[0].(map[uint64]model.ServiceResponseItem),
		CycleTransferStats: res.([]any)[1].(map[uint64]model.CycleTransferStats),
	}, nil
}

func filterServiceStatsForViewer(c *gin.Context, stats map[uint64]model.ServiceResponseItem) map[uint64]model.ServiceResponseItem {
	if len(stats) == 0 {
		return stats
	}
	services := singleton.ServiceSentinelShared.GetList()
	filteredStats := make(map[uint64]model.ServiceResponseItem, len(stats))
	for serviceID, stat := range stats {
		service, ok := services[serviceID]
		if !ok || !userCanViewService(c, service) {
			continue
		}
		filteredStats[serviceID] = stat
	}
	return filteredStats
}

func serviceResponseCacheKey(c *gin.Context) string {
	auth, ok := c.Get(model.CtxKeyAuthorizedUser)
	if !ok {
		return "list-service::guest"
	}
	user, ok := auth.(*model.User)
	if !ok || user == nil {
		return "list-service::guest"
	}
	base := fmt.Sprintf("list-service::%t::%d", user.Role.IsAdmin(), user.ID)
	tok := APITokenFromContext(c)
	if tok == nil {
		return base + "::jwt"
	}
	ids := tok.ServerIDs()
	slices.Sort(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatUint(id, 10))
	}
	return fmt.Sprintf("%s::pat:%d::servers:%s", base, tok.ID, strings.Join(parts, ","))
}

func filterCycleTransferStatsForViewer(c *gin.Context, stats map[uint64]model.CycleTransferStats) map[uint64]model.CycleTransferStats {
	if len(stats) == 0 {
		return stats
	}
	servers := singleton.ServerShared.GetList()
	filteredStats := make(map[uint64]model.CycleTransferStats, len(stats))
	for id, cycleStats := range stats {
		cycleStats.ServerName = filterServerMapForViewer(c, cycleStats.ServerName, servers)
		cycleStats.Transfer = filterServerMapForViewer(c, cycleStats.Transfer, servers)
		cycleStats.NextUpdate = filterServerMapForViewer(c, cycleStats.NextUpdate, servers)
		if len(cycleStats.ServerName) == 0 && len(cycleStats.Transfer) == 0 && len(cycleStats.NextUpdate) == 0 {
			continue
		}
		filteredStats[id] = cycleStats
	}
	return filteredStats
}

func filterServerMapForViewer[T any](c *gin.Context, values map[uint64]T, servers map[uint64]*model.Server) map[uint64]T {
	if len(values) == 0 {
		return values
	}
	filteredValues := make(map[uint64]T, len(values))
	for serverID, value := range values {
		server, ok := servers[serverID]
		if !ok || !userCanViewServer(c, server) {
			continue
		}
		filteredValues[serverID] = value
	}
	return filteredValues
}

// List service
// @Summary List service
// @Security BearerAuth
// @Schemes
// @Description List service
// @Tags auth required
// @Param id query uint false "Resource ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.Service]
// @Router /service/list [get]
func listService(c *gin.Context) ([]*model.Service, error) {
	var ss []*model.Service
	ssl := singleton.ServiceSentinelShared.GetSortedList()
	if err := copier.Copy(&ss, &ssl); err != nil {
		return nil, err
	}

	return ss, nil
}

// Get service history
// @Summary Get service history by service ID
// @Security BearerAuth
// @Schemes
// @Description Get service monitoring history for a specific service
// @Tags common
// @param id path uint true "Service ID"
// @param period query string false "Time period: 1d, 7d, 30d (default: 1d)"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.ServiceHistoryResponse]
// @Router /service/{id}/history [get]
func getServiceHistory(c *gin.Context) (*model.ServiceHistoryResponse, error) {
	idStr := c.Param("id")
	serviceID, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	service, ok := singleton.ServiceSentinelShared.Get(serviceID)
	if !ok || service == nil || !userCanViewService(c, service) {
		return nil, singleton.Localizer.ErrorT("service not found")
	}

	// 解析时间范围
	periodStr := c.DefaultQuery("period", "1d")
	period, err := tsdb.ParseQueryPeriod(periodStr)
	if err != nil {
		return nil, err
	}

	// 权限检查：未登录用户只能查看 1d 数据
	_, isMember := c.Get(model.CtxKeyAuthorizedUser)
	if !isMember && period != tsdb.Period1Day {
		return nil, singleton.Localizer.ErrorT("unauthorized: only 1d data available for guests")
	}

	response := &model.ServiceHistoryResponse{
		ServiceID:   serviceID,
		ServiceName: service.Name,
		Servers:     make([]model.ServerServiceStats, 0),
	}

	if !singleton.TSDBEnabled() {
		return queryServiceHistoryFromDB(c, serviceID, period, response)
	}

	result, err := singleton.TSDBShared.QueryServiceHistory(serviceID, period)
	if err != nil {
		return nil, err
	}

	serverMap := singleton.ServerShared.GetList()

	filtered := result.Servers[:0]
	for i := range result.Servers {
		server, ok := serverMap[result.Servers[i].ServerID]
		if !ok || !userCanViewServer(c, server) {
			continue
		}
		result.Servers[i].ServerName = server.Name
		filtered = append(filtered, result.Servers[i])
	}
	response.Servers = filtered

	return response, nil
}

func queryServiceHistoryFromDB(c *gin.Context, serviceID uint64, period tsdb.QueryPeriod, response *model.ServiceHistoryResponse) (*model.ServiceHistoryResponse, error) {
	since := time.Now().Add(-period.Duration())

	var histories []model.ServiceHistory
	if err := singleton.DB.Where("service_id = ? AND server_id != 0 AND created_at >= ?", serviceID, since).
		Order("server_id, created_at").Find(&histories).Error; err != nil {
		return nil, err
	}

	serverMap := singleton.ServerShared.GetList()
	grouped := make(map[uint64][]model.ServiceHistory)
	for _, h := range histories {
		grouped[h.ServerID] = append(grouped[h.ServerID], h)
	}

	for serverID, records := range grouped {
		server, ok := serverMap[serverID]
		if !ok || !userCanViewServer(c, server) {
			continue
		}
		stats := model.ServerServiceStats{
			ServerID:   serverID,
			ServerName: server.Name,
		}

		var totalDelay float64
		var totalUp, totalDown uint64
		dps := make([]model.DataPoint, 0, len(records))
		for _, r := range records {
			status := uint8(1)
			if r.Down > 0 && r.Up == 0 {
				status = 0
			}
			dps = append(dps, model.DataPoint{
				Timestamp: r.CreatedAt.Unix() * 1000,
				Delay:     r.AvgDelay,
				Status:    status,
			})
			totalDelay += r.AvgDelay
			totalUp += r.Up
			totalDown += r.Down
		}

		var avgDelay float64
		if len(records) > 0 {
			avgDelay = totalDelay / float64(len(records))
		}
		var upPercent float32
		if totalUp+totalDown > 0 {
			upPercent = float32(totalUp) / float32(totalUp+totalDown) * 100
		}
		stats.Stats = model.ServiceHistorySummary{
			AvgDelay:   avgDelay,
			UpPercent:  upPercent,
			TotalUp:    totalUp,
			TotalDown:  totalDown,
			DataPoints: dps,
		}
		response.Servers = append(response.Servers, stats)
	}

	return response, nil
}

// List server services
// @Summary List service histories by server id
// @Security BearerAuth
// @Schemes
// @Description List service histories for a specific server
// @Tags common
// @param id path uint true "Server ID"
// @param period query string false "Time period: 1d, 7d, 30d (default: 1d)"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.ServiceInfos]
// @Router /server/{id}/service [get]
func listServerServices(c *gin.Context) ([]*model.ServiceInfos, error) {
	idStr := c.Param("id")
	serverID, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	m := singleton.ServerShared.GetList()
	server, ok := m[serverID]
	// Hidden and unknown IDs deliberately share one response so sequential
	// server IDs cannot be enumerated by callers who may not view them.
	if !ok || server == nil || !userCanViewServer(c, server) {
		return nil, singleton.Localizer.ErrorT("server not found")
	}
	_, isMember := c.Get(model.CtxKeyAuthorizedUser)

	// 解析时间范围
	periodStr := c.DefaultQuery("period", "1d")
	period, err := tsdb.ParseQueryPeriod(periodStr)
	if err != nil {
		return nil, err
	}

	// 权限检查：未登录用户只能查看 1d 数据
	if !isMember && period != tsdb.Period1Day {
		return nil, singleton.Localizer.ErrorT("unauthorized: only 1d data available for guests")
	}

	allServices := singleton.ServiceSentinelShared.GetSortedList()
	services := make([]*model.Service, 0, len(allServices))
	for _, s := range allServices {
		if userCanViewService(c, s) {
			services = append(services, s)
		}
	}

	var result []*model.ServiceInfos

	if !singleton.TSDBEnabled() {
		return queryServerServicesFromDB(serverID, server.Name, period, services)
	}

	historyResults, err := singleton.TSDBShared.QueryServiceHistoryByServerID(serverID, period)
	if err != nil {
		return nil, err
	}

	for _, service := range services {
		if service.Cover == model.ServiceCoverAll {
			if service.SkipServers[serverID] {
				continue
			}
		} else {
			if !service.SkipServers[serverID] {
				continue
			}
		}

		historyResult, ok := historyResults[service.ID]
		if !ok || len(historyResult.Servers) == 0 {
			continue
		}

		serverStats := historyResult.Servers[0]

		infos := &model.ServiceInfos{
			ServiceID:    service.ID,
			ServerID:     serverID,
			ServiceName:  service.Name,
			ServerName:   server.Name,
			DisplayIndex: service.DisplayIndex,
			CreatedAt:    make([]int64, len(serverStats.Stats.DataPoints)),
			AvgDelay:     make([]float64, len(serverStats.Stats.DataPoints)),
		}

		for i, dp := range serverStats.Stats.DataPoints {
			infos.CreatedAt[i] = dp.Timestamp
			infos.AvgDelay[i] = dp.Delay
		}

		result = append(result, infos)
	}

	return result, nil
}

func queryServerServicesFromDB(serverID uint64, serverName string, period tsdb.QueryPeriod, services []*model.Service) ([]*model.ServiceInfos, error) {
	since := time.Now().Add(-period.Duration())

	var histories []model.ServiceHistory
	if err := singleton.DB.Where("server_id = ? AND created_at >= ?", serverID, since).
		Order("service_id, created_at").Find(&histories).Error; err != nil {
		return nil, err
	}

	grouped := make(map[uint64][]model.ServiceHistory)
	for _, h := range histories {
		grouped[h.ServiceID] = append(grouped[h.ServiceID], h)
	}

	var result []*model.ServiceInfos
	for _, service := range services {
		if service.Cover == model.ServiceCoverAll {
			if service.SkipServers[serverID] {
				continue
			}
		} else {
			if !service.SkipServers[serverID] {
				continue
			}
		}

		records, ok := grouped[service.ID]
		if !ok {
			continue
		}

		infos := &model.ServiceInfos{
			ServiceID:    service.ID,
			ServerID:     serverID,
			ServiceName:  service.Name,
			ServerName:   serverName,
			DisplayIndex: service.DisplayIndex,
			CreatedAt:    make([]int64, 0, len(records)),
			AvgDelay:     make([]float64, 0, len(records)),
		}

		for _, r := range records {
			infos.CreatedAt = append(infos.CreatedAt, r.CreatedAt.Truncate(time.Minute).Unix()*1000)
			infos.AvgDelay = append(infos.AvgDelay, r.AvgDelay)
		}

		result = append(result, infos)
	}

	return result, nil
}

// List server with service
// @Summary List server with service
// @Security BearerAuth
// @Schemes
// @Description List servers that have service monitoring data
// @Tags common
// @Produce json
// @Success 200 {object} model.CommonResponse[[]uint64]
// @Router /service/server [get]
func listServerWithServices(c *gin.Context) ([]uint64, error) {
	// 从内存中获取有服务监控配置的服务器列表
	services := singleton.ServiceSentinelShared.GetList()
	serverMap := singleton.ServerShared.GetList()

	serverIDSet := make(map[uint64]bool)

	for _, service := range services {
		if service.Cover == model.ServiceCoverAll {
			// 除了跳过的服务器，其他都包含
			for serverID := range serverMap {
				if !service.SkipServers[serverID] {
					serverIDSet[serverID] = true
				}
			}
		} else {
			// 只包含指定的服务器
			for serverID, enabled := range service.SkipServers {
				if enabled {
					serverIDSet[serverID] = true
				}
			}
		}
	}

	var ret []uint64
	for id := range serverIDSet {
		server, ok := serverMap[id]
		if !ok || server == nil {
			continue
		}
		if userCanViewServer(c, server) {
			ret = append(ret, id)
		}
	}

	return ret, nil
}

// Create service
// @Summary Create service
// @Security BearerAuth
// @Schemes
// @Description Create service
// @Tags auth required
// @Accept json
// @param request body model.ServiceForm true "Service Request"
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /service [post]
func createService(c *gin.Context) (uint64, error) {
	var mf model.ServiceForm
	if err := c.ShouldBindJSON(&mf); err != nil {
		return 0, err
	}
	if err := model.ValidateServiceMonitorType(uint64(mf.Type)); err != nil {
		return 0, err
	}
	if err := model.ValidateServiceLatency(mf.MinLatency, mf.MaxLatency); err != nil {
		return 0, err
	}
	if err := model.ValidateServiceTarget(strings.TrimSpace(mf.Target)); err != nil {
		return 0, err
	}

	if !isValidServiceCover(mf.Cover) {
		return 0, singleton.Localizer.ErrorT("permission denied")
	}

	uid := getUid(c)

	var m model.Service
	m.UserID = uid
	m.Name = mf.Name
	m.Target = strings.TrimSpace(mf.Target)
	m.Type = mf.Type
	m.SkipServers = mf.SkipServers
	m.Cover = mf.Cover
	m.DisplayIndex = mf.DisplayIndex
	m.Notify = mf.Notify
	m.NotificationGroupID = mf.NotificationGroupID
	m.Duration = mf.Duration
	m.LatencyNotify = mf.LatencyNotify
	m.MinLatency = mf.MinLatency
	m.MaxLatency = mf.MaxLatency
	m.HideForGuest = mf.HideForGuest
	m.EnableTriggerTask = mf.EnableTriggerTask
	m.RecoverTriggerTasks = mf.RecoverTriggerTasks
	m.FailTriggerTasks = mf.FailTriggerTasks

	normalizeServiceReferences(&m)
	if err := validateServers(c, &m); err != nil {
		return 0, err
	}

	if err := singleton.DB.Create(&m).Error; err != nil {
		return 0, newGormError("%v", err)
	}

	if err := singleton.ServiceSentinelShared.Update(&m); err != nil {
		return 0, err
	}

	singleton.ServiceSentinelShared.UpdateServiceList()
	return m.ID, nil
}

// Update service
// @Summary Update service
// @Security BearerAuth
// @Schemes
// @Description Update service
// @Tags auth required
// @Accept json
// @param id path uint true "Service ID"
// @param request body model.ServiceForm true "Service Request"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /service/{id} [patch]
func updateService(c *gin.Context) (any, error) {
	strID := c.Param("id")
	id, err := strconv.ParseUint(strID, 10, 64)
	if err != nil {
		return nil, err
	}
	var mf model.ServiceForm
	if err := c.ShouldBindJSON(&mf); err != nil {
		return nil, err
	}
	if err := model.ValidateServiceMonitorType(uint64(mf.Type)); err != nil {
		return nil, err
	}
	if err := model.ValidateServiceLatency(mf.MinLatency, mf.MaxLatency); err != nil {
		return nil, err
	}
	if err := model.ValidateServiceTarget(strings.TrimSpace(mf.Target)); err != nil {
		return nil, err
	}

	if !isValidServiceCover(mf.Cover) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	var m model.Service
	if err := singleton.DB.First(&m, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("service id %d does not exist", id)
	}

	if !m.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	m.Name = mf.Name
	m.Target = strings.TrimSpace(mf.Target)
	m.Type = mf.Type
	m.SkipServers = mf.SkipServers
	m.Cover = mf.Cover
	m.DisplayIndex = mf.DisplayIndex
	m.Notify = mf.Notify
	m.NotificationGroupID = mf.NotificationGroupID
	m.Duration = mf.Duration
	m.LatencyNotify = mf.LatencyNotify
	m.MinLatency = mf.MinLatency
	m.MaxLatency = mf.MaxLatency
	m.HideForGuest = mf.HideForGuest
	m.EnableTriggerTask = mf.EnableTriggerTask
	m.RecoverTriggerTasks = mf.RecoverTriggerTasks
	m.FailTriggerTasks = mf.FailTriggerTasks

	normalizeServiceReferences(&m)
	if err := validateServers(c, &m); err != nil {
		return 0, err
	}

	if err := singleton.DB.Save(&m).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	if err := singleton.ServiceSentinelShared.Update(&m); err != nil {
		return nil, err
	}

	singleton.ServiceSentinelShared.UpdateServiceList()
	return nil, nil
}

// Batch delete service
// @Summary Batch delete service
// @Security BearerAuth
// @Schemes
// @Description Batch delete service
// @Tags auth required
// @Accept json
// @param request body []uint true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/service [post]
func batchDeleteService(c *gin.Context) (any, error) {
	var ids []uint64
	if err := c.ShouldBindJSON(&ids); err != nil {
		return nil, err
	}

	if !singleton.ServiceSentinelShared.CheckPermission(c, slices.Values(ids)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	// 与 batchDeleteCron 对称：DispatchTask 没有 PAT 上下文，这里是阻止
	// 受限 PAT 通过删除 ServiceCoverAll + 不充分 SkipServers 间接影响
	// 白名单外 owner servers 探测状态的唯一同步入口。
	for _, id := range ids {
		existing, ok := singleton.ServiceSentinelShared.Get(id)
		if !ok || existing == nil {
			continue
		}
		if err := enforcePATServiceDispatchScope(c, existing); err != nil {
			return nil, err
		}
	}

	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		return tx.Unscoped().Delete(&model.Service{}, "id in (?)", ids).Error
	})
	if err != nil {
		return nil, err
	}
	singleton.ServiceSentinelShared.Delete(ids)
	singleton.ServiceSentinelShared.UpdateServiceList()
	return nil, nil
}

func validateServers(c *gin.Context, ss *model.Service) error {
	if err := checkServiceSkipServerPermission(c, ss.Cover, ss.SkipServers, ss.GetUserID()); err != nil {
		return err
	}

	if err := rejectImplicitServiceCoverForLimitedPAT(c, ss.Cover, ss.SkipServers, ss.GetUserID()); err != nil {
		return err
	}

	if !singleton.CronShared.CheckPermission(c, slices.Values(ss.FailTriggerTasks)) {
		return singleton.Localizer.ErrorT("permission denied")
	}
	if !singleton.CronShared.CheckPermission(c, slices.Values(ss.RecoverTriggerTasks)) {
		return singleton.Localizer.ErrorT("permission denied")
	}
	if err := enforcePATTriggerTaskScope(c, ss.FailTriggerTasks, ss.RecoverTriggerTasks); err != nil {
		return err
	}

	if err := assertOwnsNotificationGroup(c, ss.NotificationGroupID); err != nil {
		return err
	}

	return nil
}
