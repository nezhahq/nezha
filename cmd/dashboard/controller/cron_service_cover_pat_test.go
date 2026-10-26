package controller

// Regression tests for the implicit-cover PAT bypass classes.
//
// Background: ServerShared.CheckPermission iterates an idList and returns true
// for an empty list — it can only veto explicit IDs. createCron / createService
// both pipe cf.Servers (cron) and ss.SkipServers (service) through that helper.
// But under cover=CronCoverAll the cron's Servers slice is a *deny list* (and
// empty → fan out to every server owned by the user); under cover=ServiceCoverAll
// the service's SkipServers map is the equivalent deny set. A PAT scoped to
// server_ids=[1] can therefore craft a "cover all, deny none" config and force
// dashboard to dispatch cron commands / service probes to servers outside the
// PAT whitelist.
//
// These tests are deliberately end-to-end through commonHandler so a future
// refactor that moves the guard to a different layer still has to satisfy the
// "PAT can't escape its whitelist via cover semantics" invariant.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/service/singleton"
)

// setupCoverPATFixture builds a member-owned, two-server universe.
// alice (uid=100) owns server 1 and server 2. The caller PAT below will be
// scoped to server_ids=[1] only, so cover-all configs that fan out to
// server 2 must be rejected at the create/update boundary.
func setupCoverPATFixture(t *testing.T) {
	t.Helper()

	originalDB := singleton.DB
	originalCache := singleton.Cache
	originalLoc := singleton.Loc
	originalLocalizer := singleton.Localizer
	originalCron := singleton.CronShared
	originalServer := singleton.ServerShared
	originalUserInfo := singleton.UserInfoMap
	originalNotification := singleton.NotificationShared
	originalSentinel := singleton.ServiceSentinelShared

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Cron{}, &model.Server{}, &model.User{}, &model.Service{}, &model.NotificationGroup{}, &model.ServiceHistory{}))

	singleton.DB = db
	singleton.Loc = time.UTC
	singleton.Cache = cache.New(time.Minute, time.Minute)
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	singleton.NotificationShared = singleton.NewEmptyNotificationClassForTest()
	sc := singleton.NewEmptyServerClassForTest()
	singleton.ServerShared = sc
	singleton.CronShared = singleton.NewCronClass()

	sentinel, err := singleton.NewServiceSentinel(make(chan *model.Service, 4))
	require.NoError(t, err)
	singleton.ServiceSentinelShared = sentinel

	for _, id := range []uint64{1, 2} {
		s := &model.Server{}
		s.ID = id
		s.SetUserID(100)
		sc.InsertForTest(s)
	}
	singleton.ServerShared = sc

	singleton.UserLock.Lock()
	singleton.UserInfoMap = map[uint64]model.UserInfo{100: {Role: model.RoleMember}}
	singleton.UserLock.Unlock()

	t.Cleanup(func() {
		// Background components must be joined before restoring process globals.
		sentinel.Close()
		singleton.CronShared.Close()
		_ = sqlDB.Close()
		singleton.DB = originalDB
		singleton.Cache = originalCache
		singleton.Loc = originalLoc
		singleton.Localizer = originalLocalizer
		singleton.CronShared = originalCron
		singleton.ServerShared = originalServer
		singleton.NotificationShared = originalNotification
		singleton.ServiceSentinelShared = originalSentinel
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfo
		singleton.UserLock.Unlock()
	})
}

func coverPATRouter(t *testing.T, tok *model.APIToken, handler func(*gin.Context)) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		setAuthUser(c, 100, model.RoleMember)
		if tok != nil {
			c.Set(model.CtxKeyAPIToken, tok)
			c.Set(apiTokenCtxKey, tok)
		}
		c.Next()
	})
	r.POST("/api/v1/cron", handler)
	r.POST("/api/v1/service", handler)
	return r
}

func TestCreateCron_RejectsCoverAllForServerLimitedPAT(t *testing.T) {
	setupCoverPATFixture(t)

	tok := &model.APIToken{ID: 17, UserID: 100}
	tok.SetScopes([]string{model.ScopeCronWrite, model.ScopeCronExec})
	tok.SetServerIDs([]uint64{1})

	r := coverPATRouter(t, tok, commonHandler(createCron))

	body, _ := json.Marshal(model.CronForm{
		TaskType:  model.CronTypeCronTask,
		Name:      "evil cover-all",
		Scheduler: "@every 1m",
		Command:   "echo pwned",
		Servers:   nil,
		Cover:     model.CronCoverAll,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	assert.False(t, success,
		"PAT scoped to server_ids=[1] must NOT be able to create a CronCoverAll cron with no Servers — that fans out to server 2 outside the whitelist")
	assert.Contains(t, errMsg, "permission denied")

	var rows []model.Cron
	require.NoError(t, singleton.DB.Find(&rows).Error)
	assert.Empty(t, rows, "no cron row must be persisted when the create call is rejected")
}

func TestCreateCron_RejectsCoverIgnoreAllWithEmptyServersForLimitedPAT(t *testing.T) {
	// CoverIgnoreAll + empty Servers is "allow-list of zero" → effectively a
	// no-op cron. We still reject it because it normalises away the
	// whitelist hint a curious caller might attempt next ("just flip cover
	// to All and we'll get fan-out"). Defence-in-depth: any cover-mode that
	// implies dispatch beyond the literal Servers slice must require the
	// PAT to cover at least one whitelisted server explicitly.
	setupCoverPATFixture(t)

	tok := &model.APIToken{ID: 18, UserID: 100}
	tok.SetScopes([]string{model.ScopeCronWrite, model.ScopeCronExec})
	tok.SetServerIDs([]uint64{1})

	r := coverPATRouter(t, tok, commonHandler(createCron))

	body, _ := json.Marshal(model.CronForm{
		TaskType:  model.CronTypeCronTask,
		Name:      "ambiguous-cover",
		Scheduler: "@every 1m",
		Command:   "echo",
		Servers:   nil,
		Cover:     model.CronCoverIgnoreAll,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// Empty Servers + IgnoreAll is the degenerate "matches nothing" case;
	// it must succeed (it cannot escape) so legitimate API consumers
	// who serialise a 0-server allow-list aren't blocked.
	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	assert.True(t, success, "CoverIgnoreAll with no Servers is a no-op; not a bypass: error=%s", errMsg)
}

func TestCreateService_AllowsCoverIgnoreAllEmptySkipForLimitedPAT(t *testing.T) {
	// ServiceCoverIgnoreAll + empty SkipServers is the degenerate "matches
	// nothing" case: DispatchTask iterates only entries marked true in
	// SkipServers, so an empty map causes zero fan-out. Pin the no-op
	// classification so a future refactor that broadens IgnoreAll's
	// semantics has to update this test (and the dispatch-side guard) in
	// lock-step with the writer-side guard.
	setupCoverPATFixture(t)

	tok := &model.APIToken{ID: 21, UserID: 100}
	tok.SetScopes([]string{model.ScopeCronWrite, model.ScopeCronExec})
	tok.SetServerIDs([]uint64{1})

	r := coverPATRouter(t, tok, commonHandler(createService))

	body, _ := json.Marshal(model.ServiceForm{
		Name:        "no-op monitor",
		Target:      "example.invalid:80",
		Type:        model.TaskTypeTCPPing,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: nil,
		Duration:    30,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/service", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	assert.True(t, success, "CoverIgnoreAll with no SkipServers is a no-op; not a bypass: error=%s", errMsg)
}

func TestCreateService_RejectsCoverAllForServerLimitedPAT(t *testing.T) {
	setupCoverPATFixture(t)

	tok := &model.APIToken{ID: 19, UserID: 100}
	tok.SetScopes([]string{model.ScopeCronWrite, model.ScopeCronExec})
	tok.SetServerIDs([]uint64{1})

	r := coverPATRouter(t, tok, commonHandler(createService))

	body, _ := json.Marshal(model.ServiceForm{
		Name:        "evil cover-all monitor",
		Target:      "example.invalid:443",
		Type:        model.TaskTypeTCPPing,
		Cover:       model.ServiceCoverAll,
		SkipServers: nil,
		Duration:    30,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/service", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	assert.False(t, success,
		"PAT scoped to server_ids=[1] must NOT be able to create a ServiceCoverAll monitor with no SkipServers — DispatchTask fans out to server 2 outside the whitelist")
	assert.Contains(t, errMsg, "permission denied")

	var rows []model.Service
	require.NoError(t, singleton.DB.Find(&rows).Error)
	assert.Empty(t, rows, "no service row must be persisted when the create call is rejected")
}

// Threat: PAT server_ids=[1] + Cover=CronCoverAll + Servers=[1] (deny-list)
// passes the writer-side guard (len(Servers)>0), then CronTrigger iterates all
// owner servers, skips the whitelisted server 1, and dispatches to server 2 —
// outside the whitelist. CronTrigger has no PAT context, so the write-time
// guard is the only enforcement point.
func TestCreateCron_RejectsCoverAllWithDenyListCoveringOnlyWhitelistedServers(t *testing.T) {
	setupCoverPATFixture(t)

	tok := &model.APIToken{ID: 31, UserID: 100}
	tok.SetScopes([]string{model.ScopeCronWrite, model.ScopeCronExec})
	tok.SetServerIDs([]uint64{1})

	r := coverPATRouter(t, tok, commonHandler(createCron))

	body, _ := json.Marshal(model.CronForm{
		TaskType:  model.CronTypeCronTask,
		Name:      "cover-all deny-only-whitelisted",
		Scheduler: "@every 1m",
		Command:   "echo pwned-via-server-2",
		Servers:   []uint64{1},
		Cover:     model.CronCoverAll,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	assert.False(t, success,
		"PAT [1] must NOT create a CronCoverAll whose deny-list only contains whitelisted servers; CronTrigger would fan out to server 2")
	assert.Contains(t, errMsg, "permission denied")

	var rows []model.Cron
	require.NoError(t, singleton.DB.Find(&rows).Error)
	assert.Empty(t, rows, "no cron row must be persisted when the create call is rejected")
}

// Positive case: a server-limited PAT IS allowed to create CronCoverAll when
// the deny-list already covers every owner-visible server outside its
// whitelist. Pinning this prevents future "just block all CoverAll for PATs"
// over-corrections that would break a legitimate "schedule on whitelisted
// servers only, via deny-list" workflow.
func TestCreateCron_AllowsCoverAllWhenDenyListCoversAllNonWhitelistedServers(t *testing.T) {
	setupCoverPATFixture(t)

	tok := &model.APIToken{ID: 41, UserID: 100}
	tok.SetScopes([]string{model.ScopeCronWrite, model.ScopeCronExec})
	tok.SetServerIDs([]uint64{1})

	r := coverPATRouter(t, tok, commonHandler(createCron))

	body, _ := json.Marshal(model.CronForm{
		TaskType:  model.CronTypeCronTask,
		Name:      "legit cover-all",
		Scheduler: "@every 1m",
		Command:   "echo s1-only",
		Servers:   []uint64{2},
		Cover:     model.CronCoverAll,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	assert.True(t, success,
		"CronCoverAll with deny-list covering every non-whitelisted server must succeed for a server-limited PAT: error=%s", errMsg)
}

// Service-monitor analogue of the cron deny-list bypass: ServiceCoverAll +
// SkipServers={1:true} passes the writer-side guard (skipCount>0), then
// DispatchTask probes server 2. Same write-time enforcement requirement.
func TestCreateService_RejectsCoverAllWithSkipListCoveringOnlyWhitelistedServers(t *testing.T) {
	setupCoverPATFixture(t)

	tok := &model.APIToken{ID: 32, UserID: 100}
	tok.SetScopes([]string{model.ScopeCronWrite, model.ScopeCronExec})
	tok.SetServerIDs([]uint64{1})

	r := coverPATRouter(t, tok, commonHandler(createService))

	body, _ := json.Marshal(model.ServiceForm{
		Name:        "cover-all skip-only-whitelisted monitor",
		Target:      "example.invalid:8443",
		Type:        model.TaskTypeTCPPing,
		Cover:       model.ServiceCoverAll,
		SkipServers: map[uint64]bool{1: true},
		Duration:    30,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/service", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	assert.False(t, success,
		"PAT [1] must NOT create a ServiceCoverAll whose SkipServers only marks whitelisted servers; DispatchTask would probe server 2")
	assert.Contains(t, errMsg, "permission denied")

	var rows []model.Service
	require.NoError(t, singleton.DB.Find(&rows).Error)
	assert.Empty(t, rows, "no service row must be persisted when the create call is rejected")
}
