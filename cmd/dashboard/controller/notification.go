package controller

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

const notificationBatchDeleteMaxBodyBytes = 1 << 20

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
	slist := singleton.NotificationShared.GetSortedList()

	var notifications []*model.Notification
	if err := copier.Copy(&notifications, &slist); err != nil {
		return nil, err
	}

	// 列表端点不回显写入态凭据：notifications 是 copier 复制出的副本，置零安全，
	// 不影响 singleton 内原始数据。
	for _, n := range notifications {
		n.URL = ""
		n.RequestHeader = ""
		n.RequestBody = ""
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
	formatMetricUnits := nf.FormatMetricUnits
	n.FormatMetricUnits = &formatMetricUnits

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

	singleton.NotificationShared.Update(&n)
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
	verifyTLS := nf.VerifyTLS
	n.VerifyTLS = &verifyTLS
	formatMetricUnits := nf.FormatMetricUnits
	n.FormatMetricUnits = &formatMetricUnits

	// 凭据在列表接口已脱敏，前端无法回填；空值视为"不修改"，保留旧值避免误清空。
	if nf.URL != "" {
		n.URL = nf.URL
	}
	if nf.RequestHeader != "" {
		n.RequestHeader = nf.RequestHeader
	}
	if nf.RequestBody != "" {
		n.RequestBody = nf.RequestBody
	}

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

	singleton.NotificationShared.Update(&n)
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
	if c.Request != nil && c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, notificationBatchDeleteMaxBodyBytes)
	}
	if err := c.ShouldBindJSON(&n); err != nil {
		return nil, err
	}

	if !singleton.NotificationShared.CheckPermission(c, slices.Values(n)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	// Only the caller's rows: see deletableRowIDs.
	var deleted []uint64
	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if deleted, err = deletableRowIDs(tx, c, &model.Notification{}, n); err != nil || len(deleted) == 0 {
			return err
		}
		if err := tx.Unscoped().Delete(&model.Notification{}, "id in (?)", deleted).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&model.NotificationGroupNotification{}, "notification_id in (?)", deleted).Error; err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.NotificationShared.Delete(deleted)
	return nil, nil
}
