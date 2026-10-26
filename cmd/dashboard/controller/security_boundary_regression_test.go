package controller

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// Regression coverage uses in-memory fixtures and never contacts an instance.
func TestSecurityBoundaryBlankPassword(t *testing.T) {
	defer setupTenancyTest(t)()
	defer setupJWTSessionTest(t)()
	require.NoError(t, singleton.DB.AutoMigrate(&model.Oauth2Bind{}))
	oldUsers, oldSecrets := singleton.UserInfoMap, singleton.AgentSecretToUserId
	singleton.UserInfoMap = map[uint64]model.UserInfo{}
	singleton.AgentSecretToUserId = map[string]uint64{}
	defer func() { singleton.UserInfoMap = oldUsers; singleton.AgentSecretToUserId = oldSecrets }()
	var user model.User
	require.NoError(t, singleton.DB.First(&user, 100).Error)
	hash, err := bcrypt.GenerateFromPassword([]byte("original-password"), bcrypt.MinCost)
	require.NoError(t, err)
	user.Password = string(hash)
	require.NoError(t, singleton.DB.Save(&user).Error)
	_, err = updateProfile(profileUpdateContext(t, user, model.ProfileForm{NewUsername: "renamed"}))
	require.Error(t, err, "a logged-in session alone must not pass the original-password check")
	_, err = updateProfile(profileUpdateContext(t, user, model.ProfileForm{OriginalPassword: "original-password", NewUsername: "renamed"}))
	require.NoError(t, err)
	require.NoError(t, singleton.DB.First(&user, 100).Error)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(user.Password), []byte("original-password")))
	c := ctxAsMemberWithBody(100, model.LoginRequest{Username: "renamed", Password: ""})
	claims, err := authenticator()(c)
	require.Error(t, err)
	require.Nil(t, claims, "empty passwords must never create a session")
}

func TestSecurityBoundaryFutureAdminDDNS(t *testing.T) {
	defer setupTenancyTest(t)()
	originalUsers := singleton.UserInfoMap
	singleton.UserInfoMap = map[uint64]model.UserInfo{10: {Role: model.RoleMember}, 999: {Role: model.RoleAdmin}}
	defer func() { singleton.UserInfoMap = originalUsers }()
	require.False(t, singleton.DDNSShared.CheckPermission(ctxAs(10, model.RoleMember), slices.Values([]uint64{7})))
	profile := &model.DDNSProfile{Common: model.Common{ID: 7, UserID: 999}, Provider: model.ProviderDummy, AccessSecret: "synthetic-admin-ddns-secret"}
	singleton.DDNSShared.InsertForTest(profile)
	require.False(t, singleton.DDNSShared.CheckPermission(ctxAs(10, model.RoleMember), slices.Values([]uint64{7})), "the same ID is forbidden once it exists")
	providers, err := singleton.DDNSShared.GetDDNSProvidersFromProfiles([]uint64{7}, &model.IP{IPv4Addr: "192.0.2.10"}, 10)
	require.NoError(t, err)
	require.Empty(t, providers, "administrator ownership is not an implicit sharing grant")
}

func TestSecurityBoundaryNATCurrentHostAndLateReservation(t *testing.T) {
	defer setupTenancyTest(t)()
	oldConf, oldNAT := singleton.Conf, singleton.NATShared
	defer func() { singleton.Conf = oldConf; singleton.NATShared = oldNAT }()
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{InstallHost: "agent.example.invalid"}, ListenHost: "0.0.0.0"}}
	require.NoError(t, singleton.DB.AutoMigrate(&model.NAT{}))
	singleton.NATShared = singleton.NewNATClass()
	server := &model.Server{Common: model.Common{ID: 1, UserID: 10}, Name: "member-server"}
	singleton.ServerShared.InsertForTest(server)
	c := ctxAsMemberWithBody(10, map[string]any{"name": "panel-domain-binding", "enabled": true, "server_id": 1, "host": "127.0.0.1:8080", "domain": "panel.example.invalid"})
	c.Request.Host = "panel.example.invalid"
	id, err := createNAT(c)
	require.Error(t, err)
	require.Zero(t, id)
	require.Nil(t, singleton.NATShared.GetNATConfigByDomain("panel.example.invalid"))
}

func TestSecurityBoundaryOAuthBindAndReusableState(t *testing.T) {
	defer setupOAuth2Test(t)()
	c, _ := newOAuth2Ctx(t)
	c.Params = gin.Params{{Key: "provider", Value: "github"}}
	c.Request = httptest.NewRequest("GET", "/api/v1/oauth2/github?type=2", nil)
	response, err := oauth2redirect(c)
	require.Error(t, err, "bind requires a logged-in session")
	require.Nil(t, response)
	singleton.Cache.Set(model.CacheKeyOauth2State+"verification-key", &model.Oauth2State{State: "verification-state", Provider: "github", Action: model.RTypeLogin}, cache.DefaultExpiration)
	c.Request.AddCookie(&http.Cookie{Name: "nz-o2s", Value: "verification-key"})
	first, err := verifyState(c, "verification-state")
	require.NoError(t, err)
	second, err := verifyState(c, "verification-state")
	require.Error(t, err)
	require.NotNil(t, first)
	require.Nil(t, second, "state must be consumed once")
}

func TestSecurityBoundaryCronWriteSchedulesWithoutExec(t *testing.T) {
	setupCoverPATFixture(t)
	tok := &model.APIToken{ID: 41, UserID: 100}
	tok.SetServerIDs([]uint64{1})
	tok.SetScopes([]string{model.ScopeCronWrite})
	r := gin.New()
	r.Use(func(c *gin.Context) {
		setAuthUser(c, 100, model.RoleMember)
		c.Set(model.CtxKeyAPIToken, tok)
		c.Set(apiTokenCtxKey, tok)
		c.Next()
	})
	r.POST("/api/v1/cron", restScopeMiddleware(model.ScopeCronWrite), commonHandler(createCron))
	body, err := json.Marshal(model.CronForm{TaskType: model.CronTypeCronTask, Name: "scheduled-scope-evidence", Scheduler: "@every 1h", Command: "echo scope-evidence", Servers: []uint64{1}, Cover: model.CronCoverIgnoreAll})
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/api/v1/cron", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	success, message := decodeCommonResponseError(t, w.Body.Bytes())
	require.False(t, success, message)
	var rows []model.Cron
	require.NoError(t, singleton.DB.Find(&rows).Error)
	require.Empty(t, rows, "missing execution scope must not create or schedule a cron")
}

func TestSecurityBoundaryDisabledMCPAuditWrites(t *testing.T) {
	cleanup, _ := setupMCPTest(t)
	defer cleanup()
	singleton.Conf.SetMCPEnabled(false)
	r := gin.New()
	r.GET("/mcp/download/:token", mcpOriginGuard(), transferDownloadHandler)
	for i := 0; i < 8; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/mcp/download/not-a-valid-transfer", nil))
		require.Equal(t, http.StatusNotFound, w.Code)
	}
	var count int64
	require.NoError(t, singleton.DB.Model(&model.MCPAuditLog{}).Count(&count).Error)
	require.Zero(t, count, "disabled MCP must not generate anonymous audit rows")
	th := newTransferAnonAuditThrottle(time.Minute, 1)
	require.True(t, th.shouldRecord("192.0.2.10"))
	require.False(t, th.shouldRecord("192.0.2.10"))
	require.True(t, th.shouldRecord(""))
	require.False(t, th.shouldRecord(""))
}

func TestSecurityBoundaryFormLogin(t *testing.T) {
	defer setupJWTSessionTest(t)()
	hash, err := bcrypt.GenerateFromPassword([]byte("form-password"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", 100).Update("password", string(hash)).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	form := url.Values{"Username": {"victim"}, "Password": {"form-password"}}
	c.Request = httptest.NewRequest("POST", "/api/v1/login", strings.NewReader(form.Encode()))
	c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	claims, err := authenticator()(c)
	require.Error(t, err)
	require.Nil(t, claims)
}

func TestSecurityBoundaryInvalidServicePersists(t *testing.T) {
	setupCoverPATFixture(t)
	c := ctxAsMemberWithBody(100, model.ServiceForm{Name: "duration-overflow-evidence", Target: "example.invalid:443", Type: model.TaskTypeTCPPing, Cover: model.ServiceCoverIgnoreAll, SkipServers: map[uint64]bool{1: true}, Duration: math.MaxInt64})
	_, err := createService(c)
	require.Error(t, err, "scheduler rejects the duration")
	var rows []model.Service
	require.NoError(t, singleton.DB.Find(&rows).Error)
	require.Empty(t, rows, "invalid duration must not persist")
}
