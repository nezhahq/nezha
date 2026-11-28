package controller

import (
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// List notification group
// @Summary List notification group
// @Schemes
// @Description List notification group
// @Security BearerAuth
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.NotificationGroupResponseItem]
// @Router /notification-group [get]
func listNotificationGroup(c *gin.Context) ([]*model.NotificationGroupResponseItem, error) {
	var ng []model.NotificationGroup
	if err := singleton.DB.Find(&ng).Error; err != nil {
		return nil, err
	}

	var ngn []model.NotificationGroupNotification
	if err := singleton.DB.Find(&ngn).Error; err != nil {
		return nil, err
	}

	groupNotifications := make(map[uint64][]uint64, len(ng))
	for _, n := range ngn {
		if _, ok := groupNotifications[n.NotificationGroupID]; !ok {
			groupNotifications[n.NotificationGroupID] = make([]uint64, 0)
		}
		groupNotifications[n.NotificationGroupID] = append(groupNotifications[n.NotificationGroupID], n.NotificationID)
	}

	isAdmin := callerIsAdmin(c)
	ngRes := make([]*model.NotificationGroupResponseItem, 0, len(ng))
	for _, n := range ng {
		if !isAdmin && !n.HasPermission(c) {
			continue
		}
		ngRes = append(ngRes, &model.NotificationGroupResponseItem{
			Group:         n,
			Notifications: groupNotifications[n.ID],
		})
	}

	return ngRes, nil
}

// New notification group
// @Summary New notification group
// @Schemes
// @Description New notification group
// @Security BearerAuth
// @Tags auth required
// @Accept json
// @Param body body model.NotificationGroupForm true "NotificationGroupForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /notification-group [post]
func createNotificationGroup(c *gin.Context) (uint64, error) {
	var ngf model.NotificationGroupForm
	if err := c.ShouldBindJSON(&ngf); err != nil {
		return 0, err
	}
	// Compact removes adjacent duplicates only.
	slices.Sort(ngf.Notifications)
	ngf.Notifications = slices.Compact(ngf.Notifications)

	if !singleton.NotificationShared.CheckBindPermission(c, slices.Values(ngf.Notifications)) {
		return 0, singleton.Localizer.ErrorT("permission denied")
	}

	uid := getUid(c)

	var ng model.NotificationGroup
	ng.Name = ngf.Name
	ng.UserID = uid

	var count int64
	if err := singleton.DB.Model(&model.Notification{}).Where("id in (?)", ngf.Notifications).Count(&count).Error; err != nil {
		return 0, newGormError("%v", err)
	}

	if count != int64(len(ngf.Notifications)) {
		return 0, singleton.Localizer.ErrorT("have invalid notification id")
	}

	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&ng).Error; err != nil {
			return err
		}
		for _, n := range ngf.Notifications {
			if err := tx.Create(&model.NotificationGroupNotification{
				Common: model.Common{
					UserID: uid,
				},
				NotificationGroupID: ng.ID,
				NotificationID:      n,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, newGormError("%v", err)
	}

	singleton.NotificationShared.UpdateGroup(&ng, ngf.Notifications)
	return ng.ID, nil
}

// Edit notification group
// @Summary Edit notification group
// @Schemes
// @Description Edit notification group
// @Security BearerAuth
// @Tags auth required
// @Accept json
// @Param id path uint true "ID"
// @Param body body model.NotificationGroupForm true "NotificationGroupForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /notification-group/{id} [patch]
func updateNotificationGroup(c *gin.Context) (any, error) {
	idStr := c.Param("id")

	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	var ngf model.NotificationGroupForm
	if err := c.ShouldBindJSON(&ngf); err != nil {
		return nil, err
	}

	// Compact removes adjacent duplicates only.
	slices.Sort(ngf.Notifications)
	ngf.Notifications = slices.Compact(ngf.Notifications)

	if !singleton.NotificationShared.CheckBindPermission(c, slices.Values(ngf.Notifications)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	var ngDB model.NotificationGroup
	if err := singleton.DB.First(&ngDB, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("group id %d does not exist", id)
	}

	if !ngDB.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	ngDB.Name = ngf.Name

	var count int64
	if err := singleton.DB.Model(&model.Notification{}).Where("id in (?)", ngf.Notifications).Count(&count).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	if count != int64(len(ngf.Notifications)) {
		return nil, singleton.Localizer.ErrorT("have invalid notification id")
	}

	uid := getUid(c)

	err = singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&ngDB).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&model.NotificationGroupNotification{}, "notification_group_id = ?", id).Error; err != nil {
			return err
		}

		for _, n := range ngf.Notifications {
			if err := tx.Create(&model.NotificationGroupNotification{
				Common: model.Common{
					UserID: uid,
				},
				NotificationGroupID: ngDB.ID,
				NotificationID:      n,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.NotificationShared.UpdateGroup(&ngDB, ngf.Notifications)
	return nil, nil
}

// Batch delete notification group
// @Summary Batch delete notification group
// @Security BearerAuth
// @Schemes
// @Description Batch delete notification group
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/notification-group [post]
func batchDeleteNotificationGroup(c *gin.Context) (any, error) {
	var ngn []uint64
	if err := c.ShouldBindJSON(&ngn); err != nil {
		return nil, err
	}

	var ng []model.NotificationGroup
	if err := singleton.DB.Where("id in (?)", ngn).Find(&ng).Error; err != nil {
		return nil, err
	}

	for _, n := range ng {
		if !n.HasPermission(c) {
			return nil, singleton.Localizer.ErrorT("permission denied")
		}
	}

	// Only the caller's rows: see deletableRowIDs. This endpoint was missed
	// by the fix of GHSA-wx5f-wmgm-ghf4; the SELECT above cannot see a group
	// committed after it, so an unscoped DELETE removed other users' new
	// groups (GHSA-rx7f-phgv-7w2g).
	var deleted []uint64
	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if deleted, err = deletableRowIDs(tx, c, &model.NotificationGroup{}, ngn); err != nil || len(deleted) == 0 {
			return err
		}
		if err := tx.Unscoped().Delete(&model.NotificationGroup{}, "id in (?)", deleted).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&model.NotificationGroupNotification{}, "notification_group_id in (?)", deleted).Error; err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.NotificationShared.DeleteGroup(deleted)
	return nil, nil
}
