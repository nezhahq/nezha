package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/service/singleton"
)

func setupOnlineHistoryControllerTest(t *testing.T) (visibleServer, hiddenServer *model.Server) {
	t.Helper()
	originalDB := singleton.DB
	originalLocalizer := singleton.Localizer
	originalServer := singleton.ServerShared
	originalConf := singleton.Conf
	originalLoc := singleton.Loc
	if singleton.Localizer == nil {
		singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Server{}, &model.OnlineSession{}))

	singleton.DB = db
	singleton.ServerShared = singleton.NewEmptyServerClassForTest()
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{}}
	singleton.Conf.OnlineStatsDays = 30
	singleton.Loc = time.FixedZone("TEST", 8*3600)

	visibleServer = &model.Server{
		Common:     model.Common{ID: 1},
		Name:       "visible",
		UUID:       "online-history-visible",
		LastActive: time.Now(),
	}
	hiddenServer = &model.Server{
		Common:       model.Common{ID: 2},
		Name:         "hidden",
		UUID:         "online-history-hidden",
		HideForGuest: true,
	}
	singleton.ServerShared.InsertForTest(visibleServer)
	singleton.ServerShared.InsertForTest(hiddenServer)

	t.Cleanup(func() {
		_ = sqlDB.Close()
		singleton.DB = originalDB
		singleton.Localizer = originalLocalizer
		singleton.ServerShared = originalServer
		singleton.Conf = originalConf
		singleton.Loc = originalLoc
	})
	return visibleServer, hiddenServer
}

func onlineHistoryCtx(t *testing.T, serverID string, query string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/server/"+serverID+"/online-history?"+query, nil)
	c.Params = gin.Params{{Key: "id", Value: serverID}}
	return c
}

func onlineHistoryCtxAsMember(t *testing.T, serverID string, query string) *gin.Context {
	t.Helper()
	c := onlineHistoryCtx(t, serverID, query)
	c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 100}, Role: model.RoleMember})
	return c
}

func seedOnlineSessions(t *testing.T, serverID uint64, now time.Time) []model.OnlineSession {
	t.Helper()
	now = now.Truncate(time.Second)
	sessions := []model.OnlineSession{
		{ServerID: serverID, StartedAt: now.Add(-48 * time.Hour), EndedAt: now.Add(-47 * time.Hour), LastActiveAt: now.Add(-47 * time.Hour)},
		{ServerID: serverID, StartedAt: now.Add(-25 * time.Hour), EndedAt: now.Add(-23*time.Hour - 30*time.Minute), LastActiveAt: now.Add(-23 * time.Hour)},
		{ServerID: serverID, StartedAt: now.Add(-10 * time.Minute), Ongoing: true, LastActiveAt: now},
		// 超出默认 7 天窗口、但在 30 天窗口内的会话
		{ServerID: serverID, StartedAt: now.Add(-20 * time.Hour * 24), EndedAt: now.Add(-20*time.Hour*24 + time.Hour), LastActiveAt: now.Add(-20 * time.Hour * 24)},
	}
	for i := range sessions {
		require.NoError(t, singleton.DB.Create(&sessions[i]).Error)
	}
	return sessions
}

func TestGetServerOnlineHistoryDefaultsToConfiguredWindow(t *testing.T) {
	visible, _ := setupOnlineHistoryControllerTest(t)
	now := time.Now()
	seedOnlineSessions(t, visible.ID, now)

	// 不带 days 参数：使用后台设置的统计范围（30 天）
	c := onlineHistoryCtxAsMember(t, "1", "")
	resp, err := getServerOnlineHistory(c)
	require.NoError(t, err)
	require.Equal(t, 30, resp.Days)
	require.Equal(t, 30, resp.MaxDays)
	// 4 个会话全部落在 30 天窗口内
	require.Len(t, resp.Periods, 4)
	require.True(t, resp.CurrentlyOnline)
}

func TestGetServerOnlineHistoryDefaultIsSevenDays(t *testing.T) {
	visible, _ := setupOnlineHistoryControllerTest(t)
	now := time.Now()
	seedOnlineSessions(t, visible.ID, now)

	// 出厂默认统计范围为 7 天
	singleton.Conf.OnlineStatsDays = model.DefaultOnlineStatsDays
	c := onlineHistoryCtxAsMember(t, "1", "")
	resp, err := getServerOnlineHistory(c)
	require.NoError(t, err)
	require.Equal(t, 7, resp.Days)
	// 20 天前的会话不进入 7 天窗口
	require.Len(t, resp.Periods, 3)
}

func TestGetServerOnlineHistoryAggregatesAndClampsWindow(t *testing.T) {
	visible, _ := setupOnlineHistoryControllerTest(t)
	now := time.Now()
	seedOnlineSessions(t, visible.ID, now)

	c := onlineHistoryCtxAsMember(t, "1", "days=9999")
	resp, err := getServerOnlineHistory(c)
	require.NoError(t, err)
	// 请求超出后台设置窗口时收敛到设置值
	require.Equal(t, 30, resp.Days)

	// 聚合正确性：三个已结束/进行中会话都计入，进行中会话按当前时间截断
	var total uint64
	for _, p := range resp.Periods {
		total += uint64(p.End.Sub(p.Start).Seconds())
	}
	require.Equal(t, resp.TotalOnlineSeconds, total)
	require.GreaterOrEqual(t, resp.TotalOnlineSeconds, uint64(3600+5400+599))

	// 每日拆分合计等于总时长
	var dailySum uint64
	for _, d := range resp.Daily {
		dailySum += d.OnlineSeconds
	}
	require.Equal(t, resp.TotalOnlineSeconds, dailySum)

	// 在线率按窗口秒数计算
	windowSeconds := resp.WindowEnd.Sub(resp.WindowStart).Seconds()
	require.InDelta(t, float64(resp.TotalOnlineSeconds)/windowSeconds*100, resp.OnlineRate, 0.001)
}

func TestGetServerOnlineHistoryInvalidDays(t *testing.T) {
	setupOnlineHistoryControllerTest(t)
	for _, q := range []string{"days=abc", "days=0", "days=-1"} {
		c := onlineHistoryCtxAsMember(t, "1", q)
		_, err := getServerOnlineHistory(c)
		require.EqualError(t, err, singleton.Localizer.ErrorT("invalid days parameter").Error(), "query %q should be rejected", q)
	}
}

func TestGetServerOnlineHistoryUnknownServer(t *testing.T) {
	setupOnlineHistoryControllerTest(t)
	c := onlineHistoryCtx(t, "424242", "")
	_, err := getServerOnlineHistory(c)
	require.Error(t, err)
}

func TestGetServerOnlineHistoryGuestLimitedToOneDay(t *testing.T) {
	visible, _ := setupOnlineHistoryControllerTest(t)
	now := time.Now()
	seedOnlineSessions(t, visible.ID, now)

	// 游客不带 days 参数时默认为配置窗口（>1 天），与 metrics 的游客语义一致被拒绝
	_, err := getServerOnlineHistory(onlineHistoryCtx(t, "1", ""))
	require.Error(t, err)

	// 游客显式请求多天被拒绝，1 天可用
	_, err = getServerOnlineHistory(onlineHistoryCtx(t, "1", "days=3"))
	require.Error(t, err)
	resp, err := getServerOnlineHistory(onlineHistoryCtx(t, "1", "days=1"))
	require.NoError(t, err)
	require.Equal(t, 1, resp.Days)
	// 与 1 天窗口相交的两个会话：一个被裁剪到窗口起点，一个进行中
	require.Len(t, resp.Periods, 2)

	// 登录用户不受游客窗口限制
	resp, err = getServerOnlineHistory(onlineHistoryCtxAsMember(t, "1", "days=30"))
	require.NoError(t, err)
	require.Equal(t, 30, resp.Days)
	require.Len(t, resp.Periods, 4)
}

func TestGetServerOnlineHistoryPermission(t *testing.T) {
	visible, _ := setupOnlineHistoryControllerTest(t)

	// 游客可以查看未隐藏服务器（窗口受限于 1 天）
	guest := onlineHistoryCtx(t, "1", "days=1")
	resp, err := getServerOnlineHistory(guest)
	require.NoError(t, err)
	require.Equal(t, visible.ID, resp.ServerID)

	// 游客不能查看 HideForGuest 服务器
	guestHidden := onlineHistoryCtx(t, "2", "days=1")
	_, err = getServerOnlineHistory(guestHidden)
	require.EqualError(t, err, singleton.Localizer.ErrorT("server not found").Error())

	// 成员不能查看他人隐藏服务器
	member := onlineHistoryCtx(t, "2", "days=7")
	member.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 100}, Role: model.RoleMember})
	_, err = getServerOnlineHistory(member)
	require.EqualError(t, err, singleton.Localizer.ErrorT("server not found").Error())
}

// A server the caller may not view must answer exactly like an ID that does
// not exist on every per-server read route, otherwise walking the sequential
// IDs tells guests (and other members) which ones are hidden servers.
func TestServerReadRoutesDoNotDistinguishHiddenFromUnknown(t *testing.T) {
	_, hidden := setupOnlineHistoryControllerTest(t)
	const hiddenID, unknownID = "2", "3"
	require.EqualValues(t, 2, hidden.ID)

	routes := map[string]func(*gin.Context) error{
		"online-history": func(c *gin.Context) error { _, err := getServerOnlineHistory(c); return err },
		"metrics":        func(c *gin.Context) error { _, err := getServerMetrics(c); return err },
		"service":        func(c *gin.Context) error { _, err := listServerServices(c); return err },
	}
	callers := map[string]func(*gin.Context){
		"guest": func(*gin.Context) {},
		"other member": func(c *gin.Context) {
			c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 100}, Role: model.RoleMember})
		},
	}
	for route, call := range routes {
		for caller, login := range callers {
			hiddenCtx := onlineHistoryCtx(t, hiddenID, "days=1&metric=cpu")
			login(hiddenCtx)
			unknownCtx := onlineHistoryCtx(t, unknownID, "days=1&metric=cpu")
			login(unknownCtx)

			hiddenErr, unknownErr := call(hiddenCtx), call(unknownCtx)
			require.EqualError(t, unknownErr, singleton.Localizer.ErrorT("server not found").Error(), "%s as %s", route, caller)
			require.EqualError(t, hiddenErr, unknownErr.Error(), "%s as %s", route, caller)
		}
	}
}

func patchOnlineStatsDays(t *testing.T, days any) error {
	t.Helper()
	gin.SetMode(gin.TestMode)
	body, err := json.Marshal(map[string]any{"user_template": "user-dist", "online_stats_days": days})
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/setting", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	_, err = updateConfig(c)
	return err
}

func TestUpdateConfigOnlineStatsDaysRange(t *testing.T) {
	originalLocalizer := singleton.Localizer
	if singleton.Localizer == nil {
		singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	}
	t.Cleanup(func() { singleton.Localizer = originalLocalizer })

	templates := []model.FrontendTemplate{{Path: "user-dist", IsAdmin: false}}
	originalFrontendTemplates := singleton.FrontendTemplates
	singleton.FrontendTemplates = templates
	t.Cleanup(func() { singleton.FrontendTemplates = originalFrontendTemplates })
	conf := &singleton.ConfigClass{Config: &model.Config{}}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, conf.Read(configPath, templates))
	originalConf := singleton.Conf
	singleton.Conf = conf
	t.Cleanup(func() { singleton.Conf = originalConf })

	require.Equal(t, model.DefaultOnlineStatsDays, conf.OnlineStatsDays)

	// 合法边界值
	for _, d := range []int{model.MinOnlineStatsDays, 30, model.MaxOnlineStatsDays} {
		require.NoError(t, patchOnlineStatsDays(t, d), "days=%d should be accepted", d)
		require.Equal(t, d, conf.OnlineStatsDays)
	}

	// 越界值整体拒绝且不修改现值
	for _, d := range []int{6, model.MaxOnlineStatsDays + 1, 0, -5} {
		require.Error(t, patchOnlineStatsDays(t, d), "days=%d should be rejected", d)
		require.Equal(t, model.MaxOnlineStatsDays, conf.OnlineStatsDays)
	}

	// 非法值必须整体拒绝：校验先于任何对 Conf 的写入完成，其他字段不得
	// 半提交（评审复现场景：错误返回后 SiteName 不能被请求内容覆盖）
	conf.SiteName = "existing-site"
	require.Error(t, patchOnlineStatsDays(t, 6))
	require.Equal(t, "existing-site", conf.SiteName)
	conf.SiteName = ""

	// 字段缺省时保留现值（部分 PATCH 语义）
	require.NoError(t, patchOnlineStatsDays(t, nil))
	require.Equal(t, model.MaxOnlineStatsDays, conf.OnlineStatsDays)

	// 重启持久化断言：updateConfig 显式 Save 后，重新从磁盘读取的配置
	// 必须带上刚写入的值（持久化不再依赖 MCP 过渡块的副作用）
	reloaded := &singleton.ConfigClass{Config: &model.Config{}}
	require.NoError(t, reloaded.Read(configPath, templates))
	require.Equal(t, model.MaxOnlineStatsDays, reloaded.OnlineStatsDays)
}

func TestSettingFormOnlineStatsDaysDecode(t *testing.T) {
	var absent model.SettingForm
	require.NoError(t, json.NewDecoder(bytes.NewReader([]byte(`{"site_name":"x"}`))).Decode(&absent))
	require.Nil(t, absent.OnlineStatsDays, "omitted key must decode to nil")

	var explicit model.SettingForm
	require.NoError(t, json.NewDecoder(bytes.NewReader([]byte(`{"online_stats_days":3650}`))).Decode(&explicit))
	require.NotNil(t, explicit.OnlineStatsDays)
	require.Equal(t, 3650, *explicit.OnlineStatsDays)
}

func TestOnlineHistoryReadsPendingCloseBoundary(t *testing.T) {
	setupOnlineHistoryControllerTest(t)
	previous := singleton.OnlineHistoryShared
	t.Cleanup(func() { singleton.OnlineHistoryShared = previous })
	r := singleton.NewOnlineHistoryRecorder()
	singleton.OnlineHistoryShared = r
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	r.RecordOnline(1, start)
	require.NoError(t, singleton.DB.Exec(`CREATE TRIGGER reject_close BEFORE UPDATE ON online_sessions WHEN NEW.ongoing = 0 BEGIN SELECT RAISE(ABORT, 'close failure'); END`).Error)
	r.RecordOffline(1, start.Add(time.Minute))
	r.RecordOnline(1, start.Add(30*time.Minute))
	resp, err := getServerOnlineHistory(onlineHistoryCtx(t, "1", "days=1"))
	require.NoError(t, err)
	require.Len(t, resp.Periods, 2)
	require.False(t, resp.Periods[0].Ongoing)
	require.True(t, resp.Periods[0].End.Equal(start.Add(time.Minute)))
	require.Less(t, resp.TotalOnlineSeconds, uint64(1900))
}

func TestOnlineHistoryQueryWithNonUTCSession(t *testing.T) {
	setupOnlineHistoryControllerTest(t)
	now := time.Now()
	offset := time.FixedZone("UTC+12", 12*3600)
	session := model.OnlineSession{ServerID: 1, StartedAt: now.Add(-time.Hour).In(offset), LastActiveAt: now.Add(-time.Minute).In(offset), Ongoing: true}
	require.NoError(t, singleton.DB.Create(&session).Error)
	resp, err := getServerOnlineHistory(onlineHistoryCtx(t, "1", "days=1"))
	require.NoError(t, err)
	require.Len(t, resp.Periods, 1)
	require.True(t, resp.Periods[0].Start.Equal(session.StartedAt))
	require.InDelta(t, 3600, resp.TotalOnlineSeconds, 2)
}

func TestUpdateConfigFailedSavePreservesRetention(t *testing.T) {
	server, _ := setupOnlineHistoryControllerTest(t)
	require.NoError(t, singleton.DB.Create(server).Error)
	templates := []model.FrontendTemplate{{Path: "user-dist"}}
	previous := singleton.FrontendTemplates
	singleton.FrontendTemplates = templates
	t.Cleanup(func() { singleton.FrontendTemplates = previous })
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, singleton.Conf.Read(path, templates))
	singleton.Conf.OnlineStatsDays = 30
	require.NoError(t, singleton.Conf.Save())
	// Replacing the destination with a directory deterministically fails Save even as root.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0700))
	now := time.Now().UTC()
	old := model.OnlineSession{ServerID: 1, StartedAt: now.AddDate(0, 0, -15), EndedAt: now.AddDate(0, 0, -14)}
	require.NoError(t, singleton.DB.Create(&old).Error)
	require.Error(t, patchOnlineStatsDays(t, 7))
	require.Equal(t, 30, singleton.Conf.OnlineStatsDays)
	singleton.PruneOnlineSessions()
	var retained model.OnlineSession
	require.NoError(t, singleton.DB.First(&retained, old.ID).Error)
	require.Equal(t, old.ID, retained.ID)
}
