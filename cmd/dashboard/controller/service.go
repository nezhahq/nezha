package controller

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"

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
	res, err, _ := requestGroup.Do("list-service", func() (any, error) {
		singleton.AlertsLock.RLock()
		defer singleton.AlertsLock.RUnlock()
		stats := singleton.ServiceSentinelShared.CopyStats()
		var cycleTransferStats map[uint64]model.CycleTransferStats
		copier.Copy(&cycleTransferStats, singleton.AlertsCycleTransferStatsStore)
		return []any{
			stats, cycleTransferStats,
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

	// 检查服务是否存在
	service, ok := singleton.ServiceSentinelShared.Get(serviceID)
	if !ok || service == nil {
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
		return response, nil
	}

	result, err := singleton.TSDBShared.QueryServiceHistory(serviceID, period)
	if err != nil {
		return nil, err
	}

	serverMap := singleton.ServerShared.GetList()

	for i := range result.Servers {
		if server, ok := serverMap[result.Servers[i].ServerID]; ok {
			result.Servers[i].ServerName = server.Name
		}
	}
	response.Servers = result.Servers

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
	if !ok || server == nil {
		return nil, singleton.Localizer.ErrorT("server not found")
	}

	_, isMember := c.Get(model.CtxKeyAuthorizedUser)
	authorized := isMember

	if server.HideForGuest && !authorized {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}

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

	services := singleton.ServiceSentinelShared.GetList()

	var result []*model.ServiceInfos

	if !singleton.TSDBEnabled() {
		return result, nil
	}

	historyResults, err := singleton.TSDBShared.QueryServiceHistoryByServerID(serverID, period)
	if err != nil {
		return nil, err
	}

	for serviceID, service := range services {
		if service.Cover == model.ServiceCoverAll {
			if service.SkipServers[serverID] {
				continue
			}
		} else {
			if !service.SkipServers[serverID] {
				continue
			}
		}

		historyResult, ok := historyResults[serviceID]
		if !ok || len(historyResult.Servers) == 0 {
			continue
		}

		serverStats := historyResult.Servers[0]

		infos := &model.ServiceInfos{
			ServiceID:    serviceID,
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

	slices.SortFunc(result, func(a, b *model.ServiceInfos) int {
		if a.DisplayIndex != b.DisplayIndex {
			return cmp.Compare(b.DisplayIndex, a.DisplayIndex)
		}
		return cmp.Compare(a.ServiceID, b.ServiceID)
	})

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

	_, isMember := c.Get(model.CtxKeyAuthorizedUser)
	authorized := isMember

	var ret []uint64
	for id := range serverIDSet {
		server, ok := serverMap[id]
		if !ok || server == nil {
			continue
		}
		if !server.HideForGuest || authorized {
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
	m.EnableShowInService = mf.EnableShowInService
	m.EnableTriggerTask = mf.EnableTriggerTask
	m.RecoverTriggerTasks = mf.RecoverTriggerTasks
	m.FailTriggerTasks = mf.FailTriggerTasks

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
	m.EnableShowInService = mf.EnableShowInService
	m.EnableTriggerTask = mf.EnableTriggerTask
	m.RecoverTriggerTasks = mf.RecoverTriggerTasks
	m.FailTriggerTasks = mf.FailTriggerTasks

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
	if !singleton.ServerShared.CheckPermission(c, maps.Keys(ss.SkipServers)) {
		return singleton.Localizer.ErrorT("permission denied")
	}

	return nil
}
