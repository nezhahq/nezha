package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// 回归测试：GHSA-8fgr-wx8p-v6q7。低权限成员不能再把落库的引用集合撑到
// 超出真实对象数量的大小。

func insertTriggerCronForReferenceLimitTest(t *testing.T, id uint64) {
	t.Helper()
	cr := &model.Cron{Name: "trigger", TaskType: model.CronTypeTriggerTask, Cover: model.CronCoverAlertTrigger}
	cr.ID = id
	cr.UserID = 100
	singleton.CronShared.Update(cr)
}

func TestReferenceLimits_ServiceSkipServersDropsFalseAndMissingEntries(t *testing.T) {
	setupCoverPATFixture(t)
	skip := map[uint64]bool{1: true, 424242: true}
	for i := uint64(1000); i < 51000; i++ {
		skip[i] = false
	}
	c := ctxAsMemberWithBody(100, model.ServiceForm{Name: "svc", Target: "example.invalid:443", Type: model.TaskTypeTCPPing, Cover: model.ServiceCoverIgnoreAll, SkipServers: skip, Duration: 30})
	id, err := createService(c)
	require.NoError(t, err)

	var row model.Service
	require.NoError(t, singleton.DB.First(&row, id).Error)
	require.Equal(t, map[uint64]bool{1: true}, row.SkipServers)
	require.JSONEq(t, `{"1":true}`, row.SkipServersRaw)

	live, ok := singleton.ServiceSentinelShared.Get(id)
	require.True(t, ok)
	require.Equal(t, map[uint64]bool{1: true}, live.SkipServers, "in-memory copy must be normalized too")
}

func TestReferenceLimits_ServiceCoverAllDenySetKeepsStrictExistenceCheck(t *testing.T) {
	setupCoverPATFixture(t)
	c := ctxAsMemberWithBody(100, model.ServiceForm{Name: "svc", Target: "example.invalid:443", Type: model.TaskTypeTCPPing, Cover: model.ServiceCoverAll, SkipServers: map[uint64]bool{1: true, 424242: true}, Duration: 30})
	_, err := createService(c)
	require.Error(t, err, "a deny-set must not silently drop unknown servers")
}

func TestReferenceLimits_ServiceTriggerTasksDeduplicatedAndMissingDropped(t *testing.T) {
	setupCoverPATFixture(t)
	insertTriggerCronForReferenceLimitTest(t, 5)
	tasks := []uint64{5}
	for i := 0; i < 50000; i++ {
		tasks = append(tasks, 5, uint64(900000+i))
	}
	c := ctxAsMemberWithBody(100, model.ServiceForm{Name: "svc", Target: "example.invalid:443", Type: model.TaskTypeTCPPing, Cover: model.ServiceCoverIgnoreAll, SkipServers: map[uint64]bool{1: true}, Duration: 30, FailTriggerTasks: tasks, RecoverTriggerTasks: tasks})
	id, err := createService(c)
	require.NoError(t, err)

	var row model.Service
	require.NoError(t, singleton.DB.First(&row, id).Error)
	require.Equal(t, []uint64{5}, row.FailTriggerTasks)
	require.Equal(t, []uint64{5}, row.RecoverTriggerTasks)
	require.Equal(t, "[5]", row.FailTriggerTasksRaw)
}

func setupServerDDNSReferenceTest(t *testing.T) {
	t.Helper()
	setupCoverPATFixture(t)
	require.NoError(t, singleton.DB.AutoMigrate(&model.DDNSProfile{}))
	originalDDNS := singleton.DDNSShared
	singleton.DDNSShared = singleton.NewEmptyDDNSClassForTest()
	t.Cleanup(func() { singleton.DDNSShared = originalDDNS })
	for _, id := range []uint64{7, 8} {
		p := &model.DDNSProfile{}
		p.ID = id
		p.SetUserID(100)
		singleton.DDNSShared.InsertForTest(p)
	}
	srv := model.Server{Name: "s"}
	srv.ID = 1
	srv.UserID = 100
	require.NoError(t, singleton.DB.Create(&srv).Error)
}

func updateServerAsMember(t *testing.T, form model.ServerForm) error {
	t.Helper()
	c := ctxAsMemberWithBody(100, form)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	_, err := updateServer(c)
	return err
}

func TestReferenceLimits_ServerDDNSProfilesDeduplicatedPreservingOrder(t *testing.T) {
	setupServerDDNSReferenceTest(t)
	profiles := []uint64{8, 7}
	for i := 0; i < 100000; i++ {
		profiles = append(profiles, 7, 8)
	}
	unbound := map[uint64][]string{7: {"a.example", "a.example", "b.example"}}
	for i := uint64(1000); i < 6000; i++ {
		unbound[i] = []string{"x.example"}
	}
	require.NoError(t, updateServerAsMember(t, model.ServerForm{Name: "s", DDNSProfiles: profiles, OverrideDDNSDomains: unbound}))

	var row model.Server
	require.NoError(t, singleton.DB.First(&row, 1).Error)
	require.Equal(t, []uint64{8, 7}, row.DDNSProfiles)
	require.Equal(t, "[8,7]", row.DDNSProfilesRaw)
	require.Equal(t, map[uint64][]string{7: {"a.example", "b.example"}}, row.OverrideDDNSDomains, "only bound profiles keep overrides")

	live, ok := singleton.ServerShared.Get(1)
	require.True(t, ok)
	require.Equal(t, []uint64{8, 7}, live.DDNSProfiles)
}

func TestReferenceLimits_ServerOverrideDomainsValidated(t *testing.T) {
	setupServerDDNSReferenceTest(t)
	err := updateServerAsMember(t, model.ServerForm{Name: "s", DDNSProfiles: []uint64{7}, OverrideDDNSDomains: map[uint64][]string{7: {"bad domain\n"}}})
	require.Error(t, err, "override domains must pass the same IDN validation as profile domains")

	tooMany := make([]string, maxDomainsPerDDNSProfile+1)
	for i := range tooMany {
		tooMany[i] = "d" + itoa(uint64(i)) + ".example"
	}
	err = updateServerAsMember(t, model.ServerForm{Name: "s", DDNSProfiles: []uint64{7}, OverrideDDNSDomains: map[uint64][]string{7: tooMany}})
	require.ErrorContains(t, err, "too many domains")

	var row model.Server
	require.NoError(t, singleton.DB.First(&row, 1).Error)
	require.Empty(t, row.DDNSProfiles, "rejected updates must not persist")
}

func TestReferenceLimits_DDNSDomainsDeduplicatedAndBounded(t *testing.T) {
	setupCoverPATFixture(t)
	normalized, err := normalizeDDNSDomains([]string{"a.example", "A.example", "b.example", "a.example"})
	require.NoError(t, err)
	require.Equal(t, []string{"a.example", "b.example"}, normalized)

	_, err = normalizeDDNSDomains(make([]string, maxDomainsPerDDNSProfile+1))
	require.ErrorContains(t, err, "too many domains")
}

func TestReferenceLimits_CronAllowListDeduplicatedAndMissingDropped(t *testing.T) {
	setupCoverPATFixture(t)
	servers := []uint64{2, 1}
	for i := 0; i < 50000; i++ {
		servers = append(servers, 1, 2, uint64(900000+i))
	}
	c := ctxAsMemberWithBody(100, model.CronForm{Name: "c", TaskType: model.CronTypeCronTask, Scheduler: "0 0 * * * *", Command: "echo", Cover: model.CronCoverIgnoreAll, Servers: servers})
	id, err := createCron(c)
	require.NoError(t, err)

	var row model.Cron
	require.NoError(t, singleton.DB.First(&row, id).Error)
	require.Equal(t, []uint64{2, 1}, row.Servers)
	require.Equal(t, "[2,1]", row.ServersRaw)
}

func TestReferenceLimits_CronDenyListDeduplicated(t *testing.T) {
	setupCoverPATFixture(t)
	servers := []uint64{1}
	for i := 0; i < 50000; i++ {
		servers = append(servers, 1)
	}
	c := ctxAsMemberWithBody(100, model.CronForm{Name: "c", TaskType: model.CronTypeCronTask, Scheduler: "0 0 * * * *", Command: "echo", Cover: model.CronCoverAll, Servers: servers})
	id, err := createCron(c)
	require.NoError(t, err)

	var row model.Cron
	require.NoError(t, singleton.DB.First(&row, id).Error)
	require.Equal(t, []uint64{1}, row.Servers)
}

func validAlertSubRule() *model.Rule {
	return &model.Rule{Type: "cpu", Max: 90, Duration: 10, Cover: model.RuleCoverAll}
}

func TestReferenceLimits_AlertRuleNormalizedAndBounded(t *testing.T) {
	setupCoverPATFixture(t)
	insertTriggerCronForReferenceLimitTest(t, 5)
	ignore := map[uint64]bool{1: true, 424242: true}
	for i := uint64(1000); i < 51000; i++ {
		ignore[i] = false
	}
	rule := validAlertSubRule()
	rule.Ignore = ignore
	r := &model.AlertRule{Name: "a", Rules: []*model.Rule{rule}, FailTriggerTasks: []uint64{5, 5, 900001}, RecoverTriggerTasks: []uint64{900002, 5, 5}}
	r.UserID = 100
	require.NoError(t, validateRule(ctxAs(100, model.RoleMember), r))
	require.Equal(t, map[uint64]bool{1: true}, r.Rules[0].Ignore)
	require.Equal(t, []uint64{5}, r.FailTriggerTasks)
	require.Equal(t, []uint64{5}, r.RecoverTriggerTasks)

	tooMany := &model.AlertRule{Name: "a"}
	tooMany.UserID = 100
	for i := 0; i <= maxRulesPerAlertRule; i++ {
		tooMany.Rules = append(tooMany.Rules, validAlertSubRule())
	}
	require.ErrorContains(t, validateRule(ctxAs(100, model.RoleMember), tooMany), "too many rules")
}

func TestReferenceLimits_SetServerConfigDeduplicatesTargets(t *testing.T) {
	setupCoverPATFixture(t)
	servers := []uint64{1}
	for i := 0; i < 1000; i++ {
		servers = append(servers, 1)
	}
	c := ctxAsMemberWithBody(100, model.ServerConfigForm{Servers: servers, Config: "{}"})
	resp, err := setServerConfig(c)
	require.NoError(t, err)
	require.Equal(t, []uint64{1}, resp.Offline, "each server must be targeted once")
}

func TestReferenceLimits_RESTRequestBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("api/v1", restRequestBodyLimitMiddleware(restAPIRequestMaxBodyBytes))
	api.POST("/echo", func(c *gin.Context) {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		c.String(http.StatusOK, "ok")
	})

	small := httptest.NewRecorder()
	r.ServeHTTP(small, httptest.NewRequest(http.MethodPost, "/api/v1/echo", strings.NewReader(`{"a":1}`)))
	require.Equal(t, http.StatusOK, small.Code)

	oversized := `{"a":"` + strings.Repeat("x", restAPIRequestMaxBodyBytes) + `"}`
	big := httptest.NewRecorder()
	r.ServeHTTP(big, httptest.NewRequest(http.MethodPost, "/api/v1/echo", bytes.NewReader([]byte(oversized))))
	require.Equal(t, http.StatusBadRequest, big.Code)
	require.Contains(t, big.Body.String(), "request body too large")
}

func TestReferenceLimits_ProductionRouterLimitsAPIRequestBody(t *testing.T) {
	session := setupMCPOAuthSession(t)
	oversized := map[string]any{"name": strings.Repeat("x", restAPIRequestMaxBodyBytes)}
	response := session.request(t, http.MethodPost, "/api/v1/service", oversized, true)
	require.Contains(t, response.Body.String(), "request body too large", "routers() must mount the /api/v1 body limit")

	accepted := session.request(t, http.MethodPost, "/api/v1/service", map[string]any{"name": "small"}, true)
	require.NotContains(t, accepted.Body.String(), "request body too large")
}
