package controller

import (
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// List schedule tasks
// @Summary List schedule tasks
// @Security BearerAuth
// @Schemes
// @Description List schedule tasks
// @Tags auth required
// @Param id query uint false "Resource ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.Cron]
// @Router /cron [get]
func listCron(c *gin.Context) ([]*model.Cron, error) {
	slist := singleton.CronShared.GetSortedList()

	var cr []*model.Cron
	if err := copier.Copy(&cr, &slist); err != nil {
		return nil, err
	}
	return cr, nil
}

// Create new schedule task
// @Summary Create new schedule task
// @Security BearerAuth
// @Schemes
// @Description Create new schedule task
// @Tags auth required
// @Accept json
// @param request body model.CronForm true "CronForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /cron [post]
func createCron(c *gin.Context) (uint64, error) {
	var cf model.CronForm
	var cr model.Cron

	if err := c.ShouldBindJSON(&cf); err != nil {
		return 0, err
	}

	if !singleton.ServerShared.CheckPermission(c, slices.Values(cf.Servers)) {
		return 0, singleton.Localizer.ErrorT("permission denied")
	}

	cr.UserID = getUid(c)
	cr.TaskType = cf.TaskType
	cr.Name = cf.Name
	cr.Scheduler = cf.Scheduler
	cr.Command = cf.Command
	cr.Servers = cf.Servers
	cr.PushSuccessful = cf.PushSuccessful
	cr.NotificationGroupID = cf.NotificationGroupID
	cr.Cover = cf.Cover

	if cr.TaskType == model.CronTypeCronTask && cr.Cover == model.CronCoverAlertTrigger {
		return 0, singleton.Localizer.ErrorT("scheduled tasks cannot be triggered by alarms")
	}

	// 对于计划任务类型，需要更新CronJob
	var err error
	if cf.TaskType == model.CronTypeCronTask {
		if cr.CronJobID, err = singleton.CronShared.AddFunc(cr.Scheduler, singleton.CronTrigger(&cr)); err != nil {
			return 0, err
		}
	}

	if err = singleton.DB.Create(&cr).Error; err != nil {
		return 0, newGormError("%v", err)
	}

	singleton.CronShared.Update(&cr)
	return cr.ID, nil
}

// Update schedule task
// @Summary Update schedule task
// @Security BearerAuth
// @Schemes
// @Description Update schedule task
// @Tags auth required
// @Accept json
// @param id path uint true "Task ID"
// @param request body model.CronForm true "CronForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /cron/{id} [patch]
func updateCron(c *gin.Context) (any, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	var cf model.CronForm
	if err := c.ShouldBindJSON(&cf); err != nil {
		return 0, err
	}

	if !singleton.ServerShared.CheckPermission(c, slices.Values(cf.Servers)) {
		return 0, singleton.Localizer.ErrorT("permission denied")
	}

	var cr model.Cron
	if err := singleton.DB.First(&cr, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("task id %d does not exist", id)
	}

	if !cr.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	cr.TaskType = cf.TaskType
	cr.Name = cf.Name
	cr.Scheduler = cf.Scheduler
	cr.Command = cf.Command
	cr.Servers = cf.Servers
	cr.PushSuccessful = cf.PushSuccessful
	cr.NotificationGroupID = cf.NotificationGroupID
	cr.Cover = cf.Cover

	if cr.TaskType == model.CronTypeCronTask && cr.Cover == model.CronCoverAlertTrigger {
		return nil, singleton.Localizer.ErrorT("scheduled tasks cannot be triggered by alarms")
	}

	// 对于计划任务类型，需要更新CronJob
	if cf.TaskType == model.CronTypeCronTask {
		if cr.CronJobID, err = singleton.CronShared.AddFunc(cr.Scheduler, singleton.CronTrigger(&cr)); err != nil {
			return nil, err
		}
	}

	if err = singleton.DB.Save(&cr).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.CronShared.Update(&cr)
	return nil, nil
}

// Trigger schedule task
// @Summary Trigger schedule task
// @Security BearerAuth
// @Schemes
// @Description Trigger schedule task
// @Tags auth required
// @Accept json
// @param id path uint true "Task ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /cron/{id}/manual [get]
func manualTriggerCron(c *gin.Context) (any, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	cr, ok := singleton.CronShared.Get(id)
	if !ok {
		return nil, singleton.Localizer.ErrorT("task id %d does not exist", id)
	}

	if !cr.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	singleton.ManualTrigger(cr)
	return nil, nil
}

// Batch delete schedule tasks
// @Summary Batch delete schedule tasks
// @Security BearerAuth
// @Schemes
// @Description Batch delete schedule tasks
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/cron [post]
func batchDeleteCron(c *gin.Context) (any, error) {
	var cr []uint64
	if err := c.ShouldBindJSON(&cr); err != nil {
		return nil, err
	}

	if !singleton.CronShared.CheckPermission(c, slices.Values(cr)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	if err := singleton.DB.Unscoped().Delete(&model.Cron{}, "id in (?)", cr).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.CronShared.Delete(cr)
	return nil, nil
}
