package controller

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

// List blocked addresses
// @Summary List blocked addresses
// @Security BearerAuth
// @Schemes
// @Description List blocked IP addresses
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

	list, total, err := singleton.ListBlockedIPs(limit, offset)
	if err != nil {
		return nil, err
	}

	return &model.Value[[]*model.WAFApiMock]{
		Value: list,
		Pagination: model.Pagination{
			Offset: offset,
			Limit:  limit,
			Total:  total,
		},
	}, nil
}

// Batch delete blocked addresses
// @Summary Unblock IP addresses
// @Security BearerAuth
// @Schemes
// @Description Unblock IP addresses (in TSDB mode, blocks expire automatically)
// @Tags admin required
// @Accept json
// @Param request body []string true "IP list to unblock"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/waf [patch]
func batchDeleteBlockedAddress(c *gin.Context) (any, error) {
	var list []string
	if err := c.ShouldBindJSON(&list); err != nil {
		return nil, err
	}

	if err := singleton.BatchUnblockIP(utils.Unique(list)); err != nil {
		return nil, err
	}

	return nil, nil
}

// WAF Stats response
// @Summary Get WAF attack statistics
// @Security BearerAuth
// @Schemes
// @Description Get WAF attack statistics and trends
// @Tags auth required
// @Param hours query int false "Hours to look back (default 24)"
// @Param limit query int false "Limit for top IPs and recent events (default 10)"
// @Produce json
// @Success 200 {object} model.CommonResponse[singleton.WAFStats]
// @Router /waf/stats [get]
func getWAFStats(c *gin.Context) (*singleton.WAFStats, error) {
	hours, err := strconv.Atoi(c.Query("hours"))
	if err != nil || hours < 1 {
		hours = 24
	}
	if hours > 720 { // 最多30天
		hours = 720
	}

	limit, err := strconv.Atoi(c.Query("limit"))
	if err != nil || limit < 1 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	end := time.Now()
	start := end.Add(-time.Duration(hours) * time.Hour)

	stats, err := singleton.QueryWAFStats(start, end, limit)
	if err != nil {
		return nil, err
	}

	return stats, nil
}
