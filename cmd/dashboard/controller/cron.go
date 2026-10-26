package controller

import (
	"slices"
	"strconv"
	"sync/atomic"

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
	// Scheduled commands execute autonomously; write scope cannot imply execution.
	if tok := APITokenFromContext(c); tok != nil && !tok.HasScope(model.ScopeCronExec) {
		return 0, singleton.Localizer.ErrorT("permission denied")
	}

	var cf model.CronForm
	var cr model.Cron

	if err := c.ShouldBindJSON(&cf); err != nil {
		return 0, err
	}

	if !isValidCronCover(cf.Cover) {
		return 0, singleton.Localizer.ErrorT("permission denied")
	}

	if err := checkCronServerListPermission(c, cf.Cover, cf.Servers, getUid(c)); err != nil {
		return 0, err
	}

	if err := rejectImplicitCoverForLimitedPAT(c, cf.Cover, cf.Servers); err != nil {
		return 0, err
	}

	if err := assertOwnsNotificationGroup(c, cf.NotificationGroupID); err != nil {
		return 0, err
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

	var err error
	var committed atomic.Bool
	if cf.TaskType == model.CronTypeCronTask {
		if cr.CronJobID, err = singleton.CronShared.AddFunc(cr.Scheduler, func() {
			// Scheduler callbacks must not observe an uncommitted row or its mutable ID.
			if committed.Load() {
				singleton.CronTrigger(&cr)()
			}
		}); err != nil {
			return 0, err
		}
	}

	if err = singleton.DB.Create(&cr).Error; err != nil {
		// Registration is provisional until persistence succeeds.
		if cr.CronJobID != 0 {
			singleton.CronShared.Remove(cr.CronJobID)
		}
		return 0, newGormError("%v", err)
	}

	singleton.CronShared.Update(&cr)
	committed.Store(true)
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
	// Scheduled commands execute autonomously; write scope cannot imply execution.
	if tok := APITokenFromContext(c); tok != nil && !tok.HasScope(model.ScopeCronExec) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	var cf model.CronForm
	if err := c.ShouldBindJSON(&cf); err != nil {
		return 0, err
	}

	if !isValidCronCover(cf.Cover) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	var cr model.Cron
	if err := singleton.DB.First(&cr, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("task id %d does not exist", id)
	}

	if !cr.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	if err := checkCronServerListPermission(c, cf.Cover, cf.Servers, cr.GetUserID()); err != nil {
		return nil, err
	}

	if err := rejectImplicitCoverForLimitedPATWithOwner(c, cf.Cover, cf.Servers, cr.GetUserID()); err != nil {
		return nil, err
	}

	if err := assertOwnsNotificationGroup(c, cf.NotificationGroupID); err != nil {
		return nil, err
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

	var committed atomic.Bool
	cr.CronJobID = 0 // Only remove the newly registered job on failure, never the prior live job.
	// 对于计划任务类型，需要更新CronJob
	if cf.TaskType == model.CronTypeCronTask {
		if cr.CronJobID, err = singleton.CronShared.AddFunc(cr.Scheduler, func() {
			// Scheduler callbacks must not observe an uncommitted row or its mutable ID.
			if committed.Load() {
				singleton.CronTrigger(&cr)()
			}
		}); err != nil {
			return nil, err
		}
	}

	if err = singleton.DB.Save(&cr).Error; err != nil {
		if cr.CronJobID != 0 {
			singleton.CronShared.Remove(cr.CronJobID)
		}
		return nil, newGormError("%v", err)
	}

	singleton.CronShared.Update(&cr)
	committed.Store(true)
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
// @Router /cron/{id}/manual [post]
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

	// 运行时回放写侧 rejectImplicitCoverForLimitedPAT* 同一条 PAT 收口：
	// 历史脏数据 / 旁路写入的 cron 仍可能携带「CronCoverAll + 不充分 deny-list」
	// 的配置；CronTrigger 没有 PAT 上下文，manualTrigger 这里是唯一阻止
	// 受限 PAT 触发 fan-out 到白名单外 owner servers 的同步入口。
	if err := enforcePATCronDispatchScope(c, cr); err != nil {
		return nil, err
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

	// 与 manualTriggerCron 对称：删除会改变 fan-out 范围本身，受限 PAT 不
	// 应通过删除一个白名单内的「掩护」cron 间接放大对白名单外 owner servers
	// 的影响。回放同一条 cover-fanout 收口。
	for _, id := range cr {
		existing, ok := singleton.CronShared.Get(id)
		if !ok || existing == nil {
			continue
		}
		if err := enforcePATCronDispatchScope(c, existing); err != nil {
			return nil, err
		}
	}

	if err := singleton.DB.Unscoped().Delete(&model.Cron{}, "id in (?)", cr).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.CronShared.Delete(cr)
	return nil, nil
}
