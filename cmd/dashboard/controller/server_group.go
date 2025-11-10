package controller

import (
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// List server group
// @Summary List server group
// @Schemes
// @Description List server group
// @Security BearerAuth
// @Tags common
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.ServerGroupResponseItem]
// @Router /server-group [get]
func listServerGroup(c *gin.Context) ([]*model.ServerGroupResponseItem, error) {
	var sg []model.ServerGroup
	if err := singleton.DB.Find(&sg).Error; err != nil {
		return nil, err
	}

	groupServers := make(map[uint64][]uint64, 0)
	var sgs []model.ServerGroupServer
	if err := singleton.DB.Find(&sgs).Error; err != nil {
		return nil, err
	}
	for _, s := range sgs {
		if _, ok := groupServers[s.ServerGroupId]; !ok {
			groupServers[s.ServerGroupId] = make([]uint64, 0)
		}
		groupServers[s.ServerGroupId] = append(groupServers[s.ServerGroupId], s.ServerId)
	}

	var sgRes []*model.ServerGroupResponseItem
	for _, s := range sg {
		sgRes = append(sgRes, &model.ServerGroupResponseItem{
			Group:   s,
			Servers: groupServers[s.ID],
		})
	}

	return sgRes, nil
}

// New server group
// @Summary New server group
// @Schemes
// @Description New server group
// @Security BearerAuth
// @Tags auth required
// @Accept json
// @Param body body model.ServerGroupForm true "ServerGroupForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /server-group [post]
func createServerGroup(c *gin.Context) (uint64, error) {
	var sgf model.ServerGroupForm
	if err := c.ShouldBindJSON(&sgf); err != nil {
		return 0, err
	}
	sgf.Servers = slices.Compact(sgf.Servers)

	if !singleton.ServerShared.CheckPermission(c, slices.Values(sgf.Servers)) {
		return 0, singleton.Localizer.ErrorT("permission denied")
	}

	uid := getUid(c)

	var sg model.ServerGroup
	sg.Name = sgf.Name
	sg.UserID = uid

	var count int64
	if err := singleton.DB.Model(&model.Server{}).Where("id in (?)", sgf.Servers).Count(&count).Error; err != nil {
		return 0, newGormError("%v", err)
	}
	if count != int64(len(sgf.Servers)) {
		return 0, singleton.Localizer.ErrorT("have invalid server id")
	}

	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&sg).Error; err != nil {
			return err
		}
		for _, s := range sgf.Servers {
			if err := tx.Create(&model.ServerGroupServer{
				Common: model.Common{
					UserID: uid,
				},
				ServerGroupId: sg.ID,
				ServerId:      s,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, newGormError("%v", err)
	}

	return sg.ID, nil
}

// Edit server group
// @Summary Edit server group
// @Schemes
// @Description Edit server group
// @Security BearerAuth
// @Tags auth required
// @Accept json
// @Param id path uint true "ID"
// @Param body body model.ServerGroupForm true "ServerGroupForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /server-group/{id} [patch]
func updateServerGroup(c *gin.Context) (any, error) {
	idStr := c.Param("id")

	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	var sg model.ServerGroupForm
	if err := c.ShouldBindJSON(&sg); err != nil {
		return nil, err
	}
	sg.Servers = slices.Compact(sg.Servers)

	if !singleton.ServerShared.CheckPermission(c, slices.Values(sg.Servers)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	var sgDB model.ServerGroup
	if err := singleton.DB.First(&sgDB, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("group id %d does not exist", id)
	}

	if !sgDB.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}

	sgDB.Name = sg.Name

	var count int64
	if err := singleton.DB.Model(&model.Server{}).Where("id in (?)", sg.Servers).Count(&count).Error; err != nil {
		return nil, err
	}
	if count != int64(len(sg.Servers)) {
		return nil, singleton.Localizer.ErrorT("have invalid server id")
	}

	uid := getUid(c)

	err = singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&sgDB).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&model.ServerGroupServer{}, "server_group_id = ?", id).Error; err != nil {
			return err
		}

		for _, s := range sg.Servers {
			if err := tx.Create(&model.ServerGroupServer{
				Common: model.Common{
					UserID: uid,
				},
				ServerGroupId: sgDB.ID,
				ServerId:      s,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, newGormError("%v", err)
	}

	return nil, nil
}

// Batch delete server group
// @Summary Batch delete server group
// @Security BearerAuth
// @Schemes
// @Description Batch delete server group
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/server-group [post]
func batchDeleteServerGroup(c *gin.Context) (any, error) {
	var sgs []uint64
	if err := c.ShouldBindJSON(&sgs); err != nil {
		return nil, err
	}

	var sg []model.ServerGroup
	if err := singleton.DB.Where("id in (?)", sgs).Find(&sg).Error; err != nil {
		return nil, err
	}

	for _, s := range sg {
		if !s.HasPermission(c) {
			return nil, singleton.Localizer.ErrorT("permission denied")
		}
	}

	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Delete(&model.ServerGroup{}, "id in (?)", sgs).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&model.ServerGroupServer{}, "server_group_id in (?)", sgs).Error; err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return nil, newGormError("%v", err)
	}

	return nil, nil
}
