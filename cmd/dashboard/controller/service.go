package controller

import (
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"gorm.io/gorm"
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
	res, err, _ := requestGroup.Do("list-service", func() (interface{}, error) {
		singleton.AlertsLock.RLock()
		defer singleton.AlertsLock.RUnlock()
		stats := singleton.ServiceSentinelShared.CopyStats()
		var cycleTransferStats map[uint64]model.CycleTransferStats
		copier.Copy(&cycleTransferStats, singleton.AlertsCycleTransferStatsStore)
		return []interface {
		}{
			stats, cycleTransferStats,
		}, nil
	})
	if err != nil {
		return nil, err
	}

	return &model.ServiceResponse{
		Services:           res.([]interface{})[0].(map[uint64]model.ServiceResponseItem),
		CycleTransferStats: res.([]interface{})[1].(map[uint64]model.CycleTransferStats),
	}, nil
}

// List service
// @Summary List service
// @Security BearerAuth
// @Schemes
// @Description List service
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.Service]
// @Router /service [get]
func listService(c *gin.Context) ([]*model.Service, error) {
	singleton.ServiceSentinelShared.ServicesLock.RLock()
	defer singleton.ServiceSentinelShared.ServicesLock.RUnlock()

	var ss []*model.Service
	if err := copier.Copy(&ss, singleton.ServiceSentinelShared.ServiceList); err != nil {
		return nil, err
	}

	return ss, nil
}

// List service histories by server id
// @Summary List service histories by server id
// @Security BearerAuth
// @Schemes
// @Description List service histories by server id
// @Tags common
// @param id path uint true "Server ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.ServiceInfos]
// @Router /service/{id} [get]
func listServiceHistory(c *gin.Context) ([]*model.ServiceInfos, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	singleton.ServerLock.RLock()
	server, ok := singleton.ServerList[id]
	if !ok {
		return nil, singleton.Localizer.ErrorT("server not found")
	}

	_, isMember := c.Get(model.CtxKeyAuthorizedUser)
	authorized := isMember // TODO || isViewPasswordVerfied

	if server.HideForGuest && !authorized {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}
	singleton.ServerLock.RUnlock()

	var serviceHistories []*model.ServiceHistory
	if err := singleton.DB.Model(&model.ServiceHistory{}).Select("service_id, created_at, server_id, avg_delay").
		Where("server_id = ?", id).Where("created_at >= ?", time.Now().Add(-24*time.Hour)).Order("service_id, created_at").
		Scan(&serviceHistories).Error; err != nil {
		return nil, err
	}

	singleton.ServiceSentinelShared.ServicesLock.RLock()
	defer singleton.ServiceSentinelShared.ServicesLock.RUnlock()
	singleton.ServerLock.RLock()
	defer singleton.ServerLock.RUnlock()

	var sortedServiceIDs []uint64
	resultMap := make(map[uint64]*model.ServiceInfos)
	for _, history := range serviceHistories {
		infos, ok := resultMap[history.ServiceID]
		if !ok {
			infos = &model.ServiceInfos{
				ServiceID:   history.ServiceID,
				ServerID:    history.ServerID,
				ServiceName: singleton.ServiceSentinelShared.Services[history.ServiceID].Name,
				ServerName:  singleton.ServerList[history.ServerID].Name,
			}
			resultMap[history.ServiceID] = infos
			sortedServiceIDs = append(sortedServiceIDs, history.ServiceID)
		}
		infos.CreatedAt = append(infos.CreatedAt, history.CreatedAt.Truncate(time.Minute).Unix()*1000)
		infos.AvgDelay = append(infos.AvgDelay, history.AvgDelay)
	}

	ret := make([]*model.ServiceInfos, 0, len(sortedServiceIDs))
	for _, id := range sortedServiceIDs {
		ret = append(ret, resultMap[id])
	}

	return ret, nil
}

// List server with service
// @Summary List server with service
// @Security BearerAuth
// @Schemes
// @Description List server with service
// @Tags common
// @Produce json
// @Success 200 {object} model.CommonResponse[[]uint64]
// @Router /service/server [get]
func listServerWithServices(c *gin.Context) ([]uint64, error) {
	var serverIdsWithService []uint64
	if err := singleton.DB.Model(&model.ServiceHistory{}).
		Select("distinct(server_id)").
		Where("server_id != 0").
		Find(&serverIdsWithService).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	_, isMember := c.Get(model.CtxKeyAuthorizedUser)
	authorized := isMember // TODO || isViewPasswordVerfied

	var ret []uint64
	for _, id := range serverIdsWithService {
		singleton.ServerLock.RLock()
		server, ok := singleton.ServerList[id]
		if !ok {
			singleton.ServerLock.RUnlock()
			return nil, singleton.Localizer.ErrorT("server not found")
		}

		if !server.HideForGuest || authorized {
			ret = append(ret, id)
		}
		singleton.ServerLock.RUnlock()
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

	var skipServers []uint64
	for k := range m.SkipServers {
		skipServers = append(skipServers, k)
	}

	var err error
	if m.Cover == 0 {
		err = singleton.DB.Unscoped().Delete(&model.ServiceHistory{}, "service_id = ? and server_id in (?)", m.ID, skipServers).Error
	} else {
		err = singleton.DB.Unscoped().Delete(&model.ServiceHistory{}, "service_id = ? and server_id not in (?)", m.ID, skipServers).Error
	}
	if err != nil {
		return 0, err
	}

	if err := singleton.ServiceSentinelShared.OnServiceUpdate(m); err != nil {
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

	var skipServers []uint64
	for k := range m.SkipServers {
		skipServers = append(skipServers, k)
	}

	if m.Cover == 0 {
		err = singleton.DB.Unscoped().Delete(&model.ServiceHistory{}, "service_id = ? and server_id in (?)", m.ID, skipServers).Error
	} else {
		err = singleton.DB.Unscoped().Delete(&model.ServiceHistory{}, "service_id = ? and server_id not in (?)", m.ID, skipServers).Error
	}
	if err != nil {
		return nil, err
	}

	if err := singleton.ServiceSentinelShared.OnServiceUpdate(m); err != nil {
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

	singleton.ServiceSentinelShared.ServicesLock.RLock()
	for _, id := range ids {
		if ss, ok := singleton.ServiceSentinelShared.Services[id]; ok {
			if !ss.HasPermission(c) {
				singleton.ServiceSentinelShared.ServicesLock.RUnlock()
				return nil, singleton.Localizer.ErrorT("permission denied")
			}
		}
	}
	singleton.ServiceSentinelShared.ServicesLock.RUnlock()

	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Delete(&model.Service{}, "id in (?)", ids).Error; err != nil {
			return err
		}
		return tx.Unscoped().Delete(&model.ServiceHistory{}, "service_id in (?)", ids).Error
	})
	if err != nil {
		return nil, err
	}
	singleton.ServiceSentinelShared.OnServiceDelete(ids)
	singleton.ServiceSentinelShared.UpdateServiceList()
	return nil, nil
}

func validateServers(c *gin.Context, ss *model.Service) error {
	singleton.ServerLock.RLock()
	defer singleton.ServerLock.RUnlock()

	for s := range ss.SkipServers {
		if server, ok := singleton.ServerList[s]; ok {
			if !server.HasPermission(c) {
				return singleton.Localizer.ErrorT("permission denied")
			}
		}
	}

	return nil
}
