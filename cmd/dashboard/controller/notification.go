package controller

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"gorm.io/gorm"
)

// List notification
// @Summary List notification
// @Security BearerAuth
// @Schemes
// @Description List notification
// @Tags auth required
// @Param id query uint false "Resource ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.Notification]
// @Router /notification [get]
func listNotification(c *gin.Context) ([]*model.Notification, error) {
	singleton.NotificationSortedLock.RLock()
	defer singleton.NotificationSortedLock.RUnlock()

	var notifications []*model.Notification
	if err := copier.Copy(&notifications, &singleton.NotificationListSorted); err != nil {
		return nil, err
	}
	return notifications, nil
}

// Add notification
// @Summary Add notification
// @Security BearerAuth
// @Schemes
// @Description Add notification
// @Tags auth required
// @Accept json
// @param request body model.NotificationForm true "NotificationForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /notification [post]
func createNotification(c *gin.Context) (uint64, error) {
	var nf model.NotificationForm
	if err := c.ShouldBindJSON(&nf); err != nil {
		return 0, err
	}

	var n model.Notification
	n.UserID = getUid(c)
	n.Name = nf.Name
	n.RequestMethod = nf.RequestMethod
	n.RequestType = nf.RequestType
	n.RequestHeader = nf.RequestHeader
	n.RequestBody = nf.RequestBody
	n.URL = nf.URL
	verifyTLS := nf.VerifyTLS
	n.VerifyTLS = &verifyTLS

	ns := model.NotificationServerBundle{
		Notification: &n,
		Server:       nil,
		Loc:          singleton.Loc,
	}
	// 未勾选跳过检查
	if !nf.SkipCheck {
		if err := ns.Send(singleton.Localizer.T("a test message")); err != nil {
			return 0, err
		}
	}

	if err := singleton.DB.Create(&n).Error; err != nil {
		return 0, newGormError("%v", err)
	}

	singleton.OnRefreshOrAddNotification(&n)
	singleton.UpdateNotificationList()
	return n.ID, nil
}

// Edit notification
// @Summary Edit notification
// @Security BearerAuth
// @Schemes
// @Description Edit notification
// @Tags auth required
// @Accept json
// @Param id path uint true "Notification ID"
// @Param body body model.NotificationForm true "NotificationForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /notification/{id} [patch]
func updateNotification(c *gin.Context) (any, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}
	var nf model.NotificationForm
	if err := c.ShouldBindJSON(&nf); err != nil {
		return nil, err
	}

	var n model.Notification
	if err := singleton.DB.First(&n, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("notification id %d does not exist", id)
	}

	if !n.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	n.Name = nf.Name
	n.RequestMethod = nf.RequestMethod
	n.RequestType = nf.RequestType
	n.RequestHeader = nf.RequestHeader
	n.RequestBody = nf.RequestBody
	n.URL = nf.URL
	verifyTLS := nf.VerifyTLS
	n.VerifyTLS = &verifyTLS

	ns := model.NotificationServerBundle{
		Notification: &n,
		Server:       nil,
		Loc:          singleton.Loc,
	}
	// 未勾选跳过检查
	if !nf.SkipCheck {
		if err := ns.Send(singleton.Localizer.T("a test message")); err != nil {
			return nil, err
		}
	}

	if err := singleton.DB.Save(&n).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.OnRefreshOrAddNotification(&n)
	singleton.UpdateNotificationList()
	return nil, nil
}

// Batch delete notifications
// @Summary Batch delete notifications
// @Security BearerAuth
// @Schemes
// @Description Batch delete notifications
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/notification [post]
func batchDeleteNotification(c *gin.Context) (any, error) {
	var n []uint64
	if err := c.ShouldBindJSON(&n); err != nil {
		return nil, err
	}

	singleton.NotificationsLock.RLock()
	for _, nid := range n {
		if ns, ok := singleton.NotificationMap[nid]; ok {
			if !ns.HasPermission(c) {
				return nil, singleton.Localizer.ErrorT("permission denied")
			}
		}
	}
	singleton.NotificationsLock.RUnlock()

	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Delete(&model.Notification{}, "id in (?)", n).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&model.NotificationGroupNotification{}, "notification_id in (?)", n).Error; err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.OnDeleteNotification(n)
	singleton.UpdateNotificationList()
	return nil, nil
}
