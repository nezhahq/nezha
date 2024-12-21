package controller

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// List blocked addresses
// @Summary List blocked addresses
// @Security BearerAuth
// @Schemes
// @Description List server
// @Tags auth required
// @Param page query uint false "Page number"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.WAFApiMock]
// @Router /waf [get]
func listBlockedAddress(c *gin.Context) ([]*model.WAF, error) {
	const pageSize = 25

	page, err := strconv.ParseUint(c.Query("page"), 10, 64)
	if err != nil || page < 1 {
		page = 1
	}
	offset := (page - 1) * pageSize

	var waf []*model.WAF
	if err := singleton.DB.Limit(pageSize).Offset(int(offset)).Find(&waf).Error; err != nil {
		return nil, err
	}

	return waf, nil
}

// Batch delete blocked addresses
// @Summary Edit server
// @Security BearerAuth
// @Schemes
// @Description Edit server
// @Tags auth required
// @Accept json
// @Param request body []string true "block list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/waf [patch]
func batchDeleteBlockedAddress(c *gin.Context) (any, error) {
	var list []string
	if err := c.ShouldBindJSON(&list); err != nil {
		return nil, err
	}

	if err := model.BatchClearIP(singleton.DB, list); err != nil {
		return nil, newGormError("%v", err)
	}

	return nil, nil
}
