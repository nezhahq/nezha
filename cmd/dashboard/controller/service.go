package controller

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"

	"github.com/nezhahq/nezha/model"
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
// @Router /service [get]
func listService(c *gin.Context) ([]*model.Service, error) {
	var ss []*model.Service
	ssl := singleton.ServiceSentinelShared.GetSortedList()
	if err := copier.Copy(&ss, &ssl); err != nil {
		return nil, err
	}

	return ss, nil
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

	if err := singleton.DB.Unscoped().Delete(&model.Service{}, "id in (?)", ids).Error; err != nil {
		return nil, newGormError("%v", err)
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
