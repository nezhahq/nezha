package controller

import (
	"net"
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

// List blocked addresses
// @Summary List blocked addresses
// @Security BearerAuth
// @Schemes
// @Description List server
// @Tags auth required
// @Param limit query uint false "Page limit"
// @Param offset query uint false "Page offset"
// @Produce json
// @Success 200 {object} model.PaginatedResponse[[]model.WAFApiMock, model.WAFApiMock]
// @Router /waf [get]
func listBlockedAddress(c *gin.Context) (*model.Value[[]*model.WAFApiMock], error) {
	limit, err := strconv.Atoi(c.Query("limit"))
	if err != nil || limit < 1 {
		limit = 25
	}

	offset, err := strconv.Atoi(c.Query("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}

	var waf []*model.WAF
	if err := singleton.DB.Order("block_timestamp DESC").Limit(limit).Offset(offset).Find(&waf).Error; err != nil {
		return nil, err
	}

	var total int64
	if err := singleton.DB.Model(&model.WAF{}).Count(&total).Error; err != nil {
		return nil, err
	}

	return &model.Value[[]*model.WAFApiMock]{
		Value: slices.Collect(utils.ConvertSeq(slices.Values(waf), func(e *model.WAF) *model.WAFApiMock {
			return &model.WAFApiMock{
				IP:              net.IP(e.IP).String(),
				BlockIdentifier: e.BlockIdentifier,
				BlockReason:     e.BlockReason,
				BlockTimestamp:  e.BlockTimestamp,
				Count:           e.Count,
			}
		})),
		Pagination: model.Pagination{
			Offset: offset,
			Limit:  limit,
			Total:  total,
		},
	}, nil
}

// Batch delete blocked addresses
// @Summary Edit server
// @Security BearerAuth
// @Schemes
// @Description Edit server
// @Tags admin required
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

	if err := model.BatchUnblockIP(singleton.DB, utils.Unique(list)); err != nil {
		return nil, newGormError("%v", err)
	}

	return nil, nil
}
