package controller

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// Get server online history
// @Summary Get server online history
// @Security BearerAuth
// @Schemes
// @Description Get online periods and aggregated uptime statistics of a specific server
// @Tags common
// @Param id path uint true "Server ID"
// @Param days query int false "Query window in days (default: configured online_stats_days, clamped to it)"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.OnlineHistoryResponse]
// @Router /server/{id}/online-history [get]
func getServerOnlineHistory(c *gin.Context) (*model.OnlineHistoryResponse, error) {
	idStr := c.Param("id")
	serverID, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	server, ok := singleton.ServerShared.Get(serverID)
	// Hidden and unknown IDs deliberately share one response so sequential
	// server IDs cannot be enumerated by callers who may not view them.
	if !ok || server == nil || !userCanViewServer(c, server) {
		return nil, singleton.Localizer.ErrorT("server not found")
	}
	_, isMember := c.Get(model.CtxKeyAuthorizedUser)

	maxDays := singleton.Conf.OnlineHistoryRetentionDays()
	if maxDays < model.MinOnlineStatsDays || maxDays > model.MaxOnlineStatsDays {
		maxDays = model.DefaultOnlineStatsDays
	}
	days := maxDays
	if daysStr := c.Query("days"); daysStr != "" {
		d, err := strconv.Atoi(daysStr)
		if err != nil || d < 1 {
			return nil, singleton.Localizer.ErrorT("invalid days parameter")
		}
		// 查询窗口收敛到后台设置的统计范围内
		if d > maxDays {
			d = maxDays
		}
		days = d
	}

	if !isMember && days > 1 {
		return nil, singleton.Localizer.ErrorT("unauthorized: only 1d data available for guests")
	}

	now := time.Now()
	windowEnd := now
	windowStart := model.OnlineHistoryWindowStart(now, days, singleton.Loc)

	sessions, err := singleton.OnlineHistoryShared.Sessions(serverID, windowStart, windowEnd)
	if err != nil {
		return nil, newGormError("%v", err)
	}

	periods := model.ClipOnlineSessions(sessions, windowStart, windowEnd)
	daily := model.DailyOnlineBreakdown(periods, windowStart, windowEnd, singleton.Loc)
	// 总时长取各日之和，与 daily[] 严格对账（每日累计原始时长后统一取整，
	// 口径见 DailyOnlineBreakdown）
	var totalSeconds uint64
	for _, d := range daily {
		totalSeconds += d.OnlineSeconds
	}
	windowSeconds := uint64(windowEnd.Sub(windowStart).Seconds())
	onlineRate := 0.0
	if windowSeconds > 0 {
		onlineRate = float64(totalSeconds) / float64(windowSeconds) * 100
	}

	runtime := server.RuntimeSnapshot()
	currentlyOnline := !runtime.LastActive.IsZero() && now.Sub(runtime.LastActive) <= model.OnlineActiveWindow
	var lastActive *time.Time
	if !runtime.LastActive.IsZero() {
		lastActiveUTC := runtime.LastActive.UTC()
		lastActive = &lastActiveUTC
	}

	// 响应统一以 UTC 序列化，避免窗口时间与库中读回的会话时间混用
	// 本地偏移与 Z 两种写法（时段为值拷贝，不影响调用方）
	utcPeriods := make([]model.OnlinePeriod, len(periods))
	for i, p := range periods {
		utcPeriods[i] = model.OnlinePeriod{Start: p.Start.UTC(), End: p.End.UTC(), Ongoing: p.Ongoing}
	}

	return &model.OnlineHistoryResponse{
		ServerID:           serverID,
		ServerName:         server.Name,
		Days:               days,
		MaxDays:            maxDays,
		Timezone:           singleton.Loc.String(),
		TzOffset:           tzOffsetSeconds(now, singleton.Loc),
		WindowStart:        windowStart.UTC(),
		WindowEnd:          windowEnd.UTC(),
		TotalOnlineSeconds: totalSeconds,
		OnlineRate:         onlineRate,
		CurrentlyOnline:    currentlyOnline,
		LastActive:         lastActive,
		Periods:            utcPeriods,
		Daily:              daily,
	}, nil
}

// tzOffsetSeconds 返回指定时区在给定时刻相对 UTC 的偏移秒数。
func tzOffsetSeconds(at time.Time, loc *time.Location) int {
	_, offset := at.In(loc).Zone()
	return offset
}
