package controller

import (
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

// List NAT Profiles
// @Summary List NAT profiles
// @Schemes
// @Description List NAT profiles
// @Security BearerAuth
// @Tags auth required
// @Param id query uint false "Resource ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.NAT]
// @Router /nat [get]
func listNAT(c *gin.Context) ([]*model.NAT, error) {
	var n []*model.NAT

	slist := singleton.NATShared.GetSortedList()

	if err := copier.Copy(&n, &slist); err != nil {
		return nil, err
	}

	return n, nil
}

// Add NAT profile
// @Summary Add NAT profile
// @Security BearerAuth
// @Schemes
// @Description Add NAT profile
// @Tags auth required
// @Accept json
// @param request body model.NATForm true "NAT Request"
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /nat [post]
func createNAT(c *gin.Context) (uint64, error) {
	var nf model.NATForm
	var n model.NAT

	if err := c.ShouldBindJSON(&nf); err != nil {
		return 0, err
	}

	if server, ok := singleton.ServerShared.Get(nf.ServerID); ok {
		if !server.HasPermission(c) {
			return 0, singleton.Localizer.ErrorT("permission denied")
		}
	}

	uid := getUid(c)

	n.UserID = uid
	n.Enabled = nf.Enabled
	n.Name = nf.Name
	n.Domain = nf.Domain
	n.Host = nf.Host
	n.ServerID = nf.ServerID

	if err := singleton.DB.Create(&n).Error; err != nil {
		return 0, newGormError("%v", err)
	}

	singleton.NATShared.Update(&n)
	return n.ID, nil
}

// Edit NAT profile
// @Summary Edit NAT profile
// @Security BearerAuth
// @Schemes
// @Description Edit NAT profile
// @Tags auth required
// @Accept json
// @param id path uint true "Profile ID"
// @param request body model.NATForm true "NAT Request"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /nat/{id} [patch]
func updateNAT(c *gin.Context) (any, error) {
	idStr := c.Param("id")

	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	var nf model.NATForm
	if err := c.ShouldBindJSON(&nf); err != nil {
		return nil, err
	}

	if server, ok := singleton.ServerShared.Get(nf.ServerID); ok {
		if !server.HasPermission(c) {
			return nil, singleton.Localizer.ErrorT("permission denied")
		}
	}

	var n model.NAT
	if err = singleton.DB.First(&n, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("profile id %d does not exist", id)
	}

	if !n.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	n.Enabled = nf.Enabled
	n.Name = nf.Name
	n.Domain = nf.Domain
	n.Host = nf.Host
	n.ServerID = nf.ServerID

	if err := singleton.DB.Save(&n).Error; err != nil {
		return 0, newGormError("%v", err)
	}

	singleton.NATShared.Update(&n)
	return nil, nil
}

// Batch delete NAT configurations
// @Summary Batch delete NAT configurations
// @Security BearerAuth
// @Schemes
// @Description Batch delete NAT configurations
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/nat [post]
func batchDeleteNAT(c *gin.Context) (any, error) {
	var n []uint64
	if err := c.ShouldBindJSON(&n); err != nil {
		return nil, err
	}

	if !singleton.NATShared.CheckPermission(c, utils.ConvertSeq(slices.Values(n),
		func(id uint64) string {
			return singleton.NATShared.GetDomain(id)
		})) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	if err := singleton.DB.Unscoped().Delete(&model.NAT{}, "id in (?)", n).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.NATShared.Delete(n)
	return nil, nil
}
