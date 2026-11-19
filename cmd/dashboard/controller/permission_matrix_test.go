package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/assert"
)

func TestValidateRuleAcceptsMemberSelfTriggerTasks(t *testing.T) {
	ctx := newMemberValidationContext(t)
	rule := &model.AlertRule{
		Common:              model.Common{UserID: 200},
		Name:                "member alert",
		Rules:               []*model.Rule{{Type: "offline", Duration: 3}},
		FailTriggerTasks:    []uint64{43},
		RecoverTriggerTasks: []uint64{43},
	}
	assert.NoError(t, validateRule(ctx, rule))
}

func TestValidateRuleAcceptsAdminCrossUserTriggerTasks(t *testing.T) {
	ctx := newAdminValidationContext(t)
	rule := &model.AlertRule{
		Common:              model.Common{UserID: 1},
		Name:                "admin alert",
		Rules:               []*model.Rule{{Type: "offline", Duration: 3}},
		FailTriggerTasks:    []uint64{43},
		RecoverTriggerTasks: []uint64{43},
	}
	assert.NoError(t, validateRule(ctx, rule))
}

func TestValidateRuleAcceptsEmptyTriggerTasks(t *testing.T) {
	ctx := newMemberValidationContext(t)
	rule := &model.AlertRule{
		Common: model.Common{UserID: 200},
		Name:   "member alert",
		Rules:  []*model.Rule{{Type: "offline", Duration: 3}},
	}
	assert.NoError(t, validateRule(ctx, rule))
}

func TestValidateRuleAcceptsUnknownTriggerTaskID(t *testing.T) {
	ctx := newMemberValidationContext(t)
	rule := &model.AlertRule{
		Common:           model.Common{UserID: 200},
		Name:             "member alert",
		Rules:            []*model.Rule{{Type: "offline", Duration: 3}},
		FailTriggerTasks: []uint64{9999},
	}
	assert.NoError(t, validateRule(ctx, rule))
}

func TestValidateRuleRejectsForeignNotificationGroup(t *testing.T) {
	ctx := newMemberValidationContext(t)
	rule := &model.AlertRule{
		Common:              model.Common{UserID: 200},
		Name:                "member alert",
		Rules:               []*model.Rule{{Type: "offline", Duration: 3}},
		NotificationGroupID: 7,
	}
	assert.Error(t, validateRule(ctx, rule))
}

func TestValidateRuleAcceptsMemberOwnedNotificationGroup(t *testing.T) {
	ctx := newMemberValidationContext(t)
	rule := &model.AlertRule{
		Common:              model.Common{UserID: 200},
		Name:                "member alert",
		Rules:               []*model.Rule{{Type: "offline", Duration: 3}},
		NotificationGroupID: 8,
	}
	assert.NoError(t, validateRule(ctx, rule))
}

func TestValidateRuleAdminCanReferenceAnyNotificationGroup(t *testing.T) {
	ctx := newAdminValidationContext(t)
	rule := &model.AlertRule{
		Common:              model.Common{UserID: 1},
		Name:                "admin alert",
		Rules:               []*model.Rule{{Type: "offline", Duration: 3}},
		NotificationGroupID: 8,
	}
	assert.NoError(t, validateRule(ctx, rule))
}

func TestValidateServersRejectsForeignNotificationGroup(t *testing.T) {
	ctx := newMemberValidationContext(t)
	service := &model.Service{
		Common:              model.Common{UserID: 200},
		Name:                "member service",
		SkipServers:         map[uint64]bool{},
		NotificationGroupID: 7,
	}
	assert.Error(t, validateServers(ctx, service))
}

func TestValidateServersAcceptsMemberOwnedNotificationGroup(t *testing.T) {
	ctx := newMemberValidationContext(t)
	service := &model.Service{
		Common:              model.Common{UserID: 200},
		Name:                "member service",
		SkipServers:         map[uint64]bool{},
		NotificationGroupID: 8,
	}
	assert.NoError(t, validateServers(ctx, service))
}

func TestUserCanViewServer(t *testing.T) {
	memberServer := &model.Server{Common: model.Common{ID: 1, UserID: 200}}
	adminServer := &model.Server{Common: model.Common{ID: 2, UserID: 1}}
	hiddenAdminServer := &model.Server{Common: model.Common{ID: 3, UserID: 1}, HideForGuest: true}
	publicAdminServer := &model.Server{Common: model.Common{ID: 4, UserID: 1}, HideForGuest: false}

	cases := []struct {
		name      string
		setup     func(c *gin.Context)
		server    *model.Server
		wantAllow bool
	}{
		{
			name:      "guest sees public",
			setup:     func(c *gin.Context) {},
			server:    publicAdminServer,
			wantAllow: true,
		},
		{
			name:      "guest blocked by HideForGuest",
			setup:     func(c *gin.Context) {},
			server:    hiddenAdminServer,
			wantAllow: false,
		},
		{
			name: "member sees own",
			setup: func(c *gin.Context) {
				c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 200}, Role: model.RoleMember})
			},
			server:    memberServer,
			wantAllow: true,
		},
		{
			name: "member can still see public foreign",
			setup: func(c *gin.Context) {
				c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 200}, Role: model.RoleMember})
			},
			server:    publicAdminServer,
			wantAllow: true,
		},
		{
			name: "member blocked by HideForGuest foreign",
			setup: func(c *gin.Context) {
				c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 200}, Role: model.RoleMember})
			},
			server:    hiddenAdminServer,
			wantAllow: false,
		},
		{
			name: "admin sees hidden foreign",
			setup: func(c *gin.Context) {
				c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 1}, Role: model.RoleAdmin})
			},
			server:    hiddenAdminServer,
			wantAllow: true,
		},
		{
			name: "admin sees other admin server",
			setup: func(c *gin.Context) {
				c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 1}, Role: model.RoleAdmin})
			},
			server:    adminServer,
			wantAllow: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			tc.setup(ctx)
			if got := userCanViewServer(ctx, tc.server); got != tc.wantAllow {
				t.Fatalf("userCanViewServer = %v, want %v", got, tc.wantAllow)
			}
		})
	}
}

type permissionTestResource struct {
	ID     uint64 `json:"id"`
	UserID uint64 `json:"user_id"`
}

func (r *permissionTestResource) GetID() uint64     { return r.ID }
func (r *permissionTestResource) GetUserID() uint64 { return r.UserID }
func (r *permissionTestResource) HasPermission(c *gin.Context) bool {
	auth, ok := c.Get(model.CtxKeyAuthorizedUser)
	if !ok {
		return false
	}
	user := *auth.(*model.User)
	if user.Role == model.RoleAdmin {
		return true
	}
	return user.ID == r.UserID
}

func TestListHandlerFiltersByOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	data := []*permissionTestResource{
		{ID: 1, UserID: 100},
		{ID: 2, UserID: 200},
		{ID: 3, UserID: 200},
	}
	handler := listHandler(func(c *gin.Context) ([]*permissionTestResource, error) {
		return append([]*permissionTestResource{}, data...), nil
	})

	t.Run("member only sees own", func(t *testing.T) {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 200}, Role: model.RoleMember})
			c.Next()
		})
		r.GET("/test", handler)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		r.ServeHTTP(w, req)

		ids := decodeIDs[uint64](t, w.Body.Bytes())
		assert.ElementsMatch(t, []uint64{2, 3}, ids)
	})

	t.Run("admin sees all", func(t *testing.T) {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 1}, Role: model.RoleAdmin})
			c.Next()
		})
		r.GET("/test", handler)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		r.ServeHTTP(w, req)

		ids := decodeIDs[uint64](t, w.Body.Bytes())
		assert.ElementsMatch(t, []uint64{1, 2, 3}, ids)
	})
}

func TestShowServiceFiltersCycleTransferStatsLikeServerList(t *testing.T) {
	newMemberValidationContext(t)
	assert.NoError(t, singleton.DB.AutoMigrate(&model.Service{}, &model.ServiceHistory{}))
	assert.NoError(t, singleton.DB.Create(&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "public server", UUID: "public-server"}).Error)
	assert.NoError(t, singleton.DB.Create(&model.Server{Common: model.Common{ID: 2, UserID: 1}, Name: "hidden admin server", UUID: "hidden-admin-server", HideForGuest: true}).Error)
	assert.NoError(t, singleton.DB.Create(&model.Server{Common: model.Common{ID: 3, UserID: 200}, Name: "hidden member server", UUID: "hidden-member-server", HideForGuest: true}).Error)
	singleton.ServerShared = singleton.NewServerClass()

	assert.NoError(t, singleton.DB.Create(&model.Service{Common: model.Common{ID: 10, UserID: 1}, Name: "shown service", Type: model.TaskTypeTCPPing}).Error)
	assert.NoError(t, singleton.DB.Create(&model.Service{Common: model.Common{ID: 11, UserID: 1}, Name: "hidden service", Type: model.TaskTypeTCPPing, HideForGuest: true}).Error)

	originalServiceSentinel := singleton.ServiceSentinelShared
	serviceSentinel, err := singleton.NewServiceSentinel(make(chan *model.Service, 2))
	assert.NoError(t, err)
	singleton.ServiceSentinelShared = serviceSentinel
	t.Cleanup(func() {
		serviceSentinel.Close()
		singleton.ServiceSentinelShared = originalServiceSentinel
	})

	singleton.AlertsLock.Lock()
	originalCycleTransferStats := singleton.AlertsCycleTransferStatsStore
	singleton.AlertsCycleTransferStatsStore = map[uint64]*model.CycleTransferStats{
		7: {
			Name:       "transfer alert",
			ServerName: map[uint64]string{1: "public server", 2: "hidden admin server", 3: "hidden member server"},
			Transfer:   map[uint64]uint64{1: 100, 2: 200, 3: 300},
			NextUpdate: map[uint64]time.Time{1: time.Unix(1, 0), 2: time.Unix(2, 0), 3: time.Unix(3, 0)},
		},
	}
	singleton.AlertsLock.Unlock()
	t.Cleanup(func() {
		singleton.AlertsLock.Lock()
		singleton.AlertsCycleTransferStatsStore = originalCycleTransferStats
		singleton.AlertsLock.Unlock()
	})

	tests := []struct {
		name         string
		viewer       *model.User
		wantServices []uint64
		wantNames    map[uint64]string
	}{
		{
			name:         "guest sees public servers only",
			wantServices: []uint64{10},
			wantNames:    map[uint64]string{1: "public server"},
		},
		{
			name:         "member sees public and owned hidden servers",
			viewer:       &model.User{Common: model.Common{ID: 200}, Role: model.RoleMember},
			wantServices: []uint64{10},
			wantNames:    map[uint64]string{1: "public server", 3: "hidden member server"},
		},
		{
			name:         "admin sees every server",
			viewer:       &model.User{Common: model.Common{ID: 1}, Role: model.RoleAdmin},
			wantServices: []uint64{10, 11},
			wantNames:    map[uint64]string{1: "public server", 2: "hidden admin server", 3: "hidden member server"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			if tc.viewer != nil {
				ctx.Set(model.CtxKeyAuthorizedUser, tc.viewer)
			}

			got, err := showService(ctx)
			assert.NoError(t, err)
			assert.ElementsMatch(t, tc.wantServices, serviceResponseIDs(got.Services))
			if assert.Contains(t, got.CycleTransferStats, uint64(7)) {
				cycleStats := got.CycleTransferStats[7]
				assert.Equal(t, tc.wantNames, cycleStats.ServerName)
				assert.Len(t, cycleStats.Transfer, len(tc.wantNames))
				assert.Len(t, cycleStats.NextUpdate, len(tc.wantNames))
				for serverID := range cycleStats.Transfer {
					assert.Contains(t, tc.wantNames, serverID)
				}
				for serverID := range cycleStats.NextUpdate {
					assert.Contains(t, tc.wantNames, serverID)
				}
			}
		})
	}
}

func serviceResponseIDs(stats map[uint64]model.ServiceResponseItem) []uint64 {
	ids := make([]uint64, 0, len(stats))
	for id := range stats {
		ids = append(ids, id)
	}
	return ids
}

func decodeIDs[T ~uint64](t *testing.T, body []byte) []T {
	t.Helper()
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, string(body))
	}
	ids := make([]T, 0, len(resp.Data))
	for _, item := range resp.Data {
		switch v := item["id"].(type) {
		case float64:
			ids = append(ids, T(v))
		case json.Number:
			n, _ := v.Int64()
			ids = append(ids, T(n))
		}
	}
	return ids
}

func TestCallerIsAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name  string
		setup func(c *gin.Context)
		want  bool
	}{
		{name: "unauth", setup: func(c *gin.Context) {}, want: false},
		{
			name: "member",
			setup: func(c *gin.Context) {
				c.Set(model.CtxKeyAuthorizedUser, &model.User{Role: model.RoleMember})
			},
			want: false,
		},
		{
			name: "admin",
			setup: func(c *gin.Context) {
				c.Set(model.CtxKeyAuthorizedUser, &model.User{Role: model.RoleAdmin})
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			tc.setup(ctx)
			if got := callerIsAdmin(ctx); got != tc.want {
				t.Fatalf("callerIsAdmin = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAssertOwnsNotificationGroup(t *testing.T) {
	memberCtx := newMemberValidationContext(t)
	assert.NoError(t, assertOwnsNotificationGroup(memberCtx, 0))
	assert.NoError(t, assertOwnsNotificationGroup(memberCtx, 8))
	assert.Error(t, assertOwnsNotificationGroup(memberCtx, 7))
	assert.ErrorContains(t, assertOwnsNotificationGroup(memberCtx, 9999), "does not exist")

	adminCtx := newAdminValidationContext(t)
	assert.NoError(t, assertOwnsNotificationGroup(adminCtx, 7))
	assert.NoError(t, assertOwnsNotificationGroup(adminCtx, 8))
}

// 写侧绑定通知组后，sentinel / cron 会在没有 PAT 上下文的情况下通过该组发通知，
// 所以 PAT 必须自带 notification-group scope，仅凭 service / alertrule / cron
// 的 write scope 不能借用签发者的通知渠道。
func TestAssertOwnsNotificationGroupPATRequiresNotificationGroupScope(t *testing.T) {
	withPAT := func(ctx *gin.Context, userID uint64, scopes ...string) *gin.Context {
		tok := &model.APIToken{ID: 1, UserID: userID}
		tok.SetScopes(scopes)
		ctx.Set(model.CtxKeyAPIToken, tok)
		ctx.Set(apiTokenCtxKey, tok)
		return ctx
	}

	noScope := withPAT(newMemberValidationContext(t), 200, model.ScopeServiceWrite, model.ScopeAlertRuleWrite, model.ScopeCronWrite, model.ScopeCronExec)
	assert.NoError(t, assertOwnsNotificationGroup(noScope, 0), "leaving the group unset needs no scope")
	assert.Error(t, assertOwnsNotificationGroup(noScope, 8), "PAT without a notification-group scope must not bind its owner's group")
	// 无 scope 的 PAT 不应能通过报错差异枚举通知组 ID。
	assert.ErrorContains(t, assertOwnsNotificationGroup(noScope, 9999), "permission denied")
	assert.Error(t, validateServers(noScope, &model.Service{
		Common:              model.Common{UserID: 200},
		Cover:               model.ServiceCoverIgnoreAll,
		Notify:              true,
		NotificationGroupID: 8,
	}), "service:write PAT must not bind a notification group")
	assert.Error(t, validateRule(noScope, &model.AlertRule{
		Common:              model.Common{UserID: 200},
		Rules:               []*model.Rule{{Type: "offline", Cover: model.RuleCoverIgnoreAll, Duration: 10}},
		NotificationGroupID: 8,
	}), "alertrule:write PAT must not bind a notification group")

	withScope := withPAT(newMemberValidationContext(t), 200, model.ScopeServiceWrite, model.ScopeNotificationGroupRead)
	assert.NoError(t, assertOwnsNotificationGroup(withScope, 8))
	assert.Error(t, assertOwnsNotificationGroup(withScope, 7), "scope must not widen ownership")
	assert.NoError(t, validateServers(withScope, &model.Service{
		Common:              model.Common{UserID: 200},
		Cover:               model.ServiceCoverIgnoreAll,
		Notify:              true,
		NotificationGroupID: 8,
	}))

	adminNoScope := withPAT(newAdminValidationContext(t), 1, model.ScopeServiceWrite)
	assert.Error(t, assertOwnsNotificationGroup(adminNoScope, 8), "admin-issued PAT is bound by its scopes too")
}

func TestListServerGroupFiltersByOwnership(t *testing.T) {
	ctx := newMemberValidationContext(t)
	assert.NoError(t, singleton.DB.Create(&model.ServerGroup{Common: model.Common{ID: 1, UserID: 200}, Name: "member group"}).Error)
	assert.NoError(t, singleton.DB.Create(&model.ServerGroup{Common: model.Common{ID: 2, UserID: 1}, Name: "admin group"}).Error)

	got, err := listServerGroup(ctx)
	assert.NoError(t, err)
	var names []string
	for _, g := range got {
		names = append(names, g.Group.Name)
	}
	assert.ElementsMatch(t, []string{"member group"}, names)
}

func TestListServerGroupAdminSeesAll(t *testing.T) {
	ctx := newAdminValidationContext(t)
	assert.NoError(t, singleton.DB.Create(&model.ServerGroup{Common: model.Common{ID: 1, UserID: 200}, Name: "member group"}).Error)
	assert.NoError(t, singleton.DB.Create(&model.ServerGroup{Common: model.Common{ID: 2, UserID: 1}, Name: "admin group"}).Error)

	got, err := listServerGroup(ctx)
	assert.NoError(t, err)
	var names []string
	for _, g := range got {
		names = append(names, g.Group.Name)
	}
	assert.ElementsMatch(t, []string{"member group", "admin group"}, names)
}

func TestListNotificationGroupFiltersByOwnership(t *testing.T) {
	ctx := newMemberValidationContext(t)
	got, err := listNotificationGroup(ctx)
	assert.NoError(t, err)
	var names []string
	for _, g := range got {
		names = append(names, g.Group.Name)
	}
	assert.ElementsMatch(t, []string{"member group"}, names)
}

func TestListNotificationGroupAdminSeesAll(t *testing.T) {
	ctx := newAdminValidationContext(t)
	got, err := listNotificationGroup(ctx)
	assert.NoError(t, err)
	var names []string
	for _, g := range got {
		names = append(names, g.Group.Name)
	}
	assert.ElementsMatch(t, []string{"admin group", "member group"}, names)
}

func TestBatchMoveServerRejectsNonAdminCrossUser(t *testing.T) {
	ctx := newMemberValidationContext(t)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/batch-move/server", strings.NewReader(`{"ids":[],"to_user":1}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	_, err := batchMoveServer(ctx)
	assert.Error(t, err)
}

func TestBatchMoveServerAllowsMemberSelfMove(t *testing.T) {
	ctx := newMemberValidationContext(t)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/batch-move/server", strings.NewReader(`{"ids":[],"to_user":200}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	_, err := batchMoveServer(ctx)
	assert.NoError(t, err)
}

func TestBatchMoveServerAllowsAdminCrossUser(t *testing.T) {
	ctx := newAdminValidationContext(t)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/batch-move/server", strings.NewReader(`{"ids":[],"to_user":200}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	_, err := batchMoveServer(ctx)
	assert.NoError(t, err)
}

func TestBatchMoveServerMasksForeignServerIDsForMembers(t *testing.T) {
	ctx := newMemberValidationContext(t)
	assert.NoError(t, singleton.DB.Create(&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "foreign", UUID: "foreign-server"}).Error)
	singleton.ServerShared = singleton.NewServerClass()

	ctx.Request = httptest.NewRequest(http.MethodPost, "/batch-move/server", strings.NewReader(`{"ids":[1,9999],"to_user":200}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	got, err := batchMoveServer(ctx)

	assert.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Equal(t, model.BatchMoveServerResultServerNotFound, got[0].Status)
	assert.Equal(t, model.BatchMoveServerResultServerNotFound, got[1].Status)
}

func TestBatchMoveServerBoundsIDList(t *testing.T) {
	for _, n := range []int{batchMoveServerMaxIDs, batchMoveServerMaxIDs + 1} {
		ctx := newMemberValidationContext(t)
		body := `{"ids":[0` + strings.Repeat(",0", n-1) + `],"to_user":200}`
		ctx.Request = httptest.NewRequest(http.MethodPost, "/batch-move/server", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		got, err := batchMoveServer(ctx)
		if n > batchMoveServerMaxIDs {
			assert.Error(t, err)
			assert.Empty(t, got)
			continue
		}
		assert.NoError(t, err)
		assert.Len(t, got, n)
	}
}

func TestNATRejectsUnknownServerID(t *testing.T) {
	ctx := newMemberValidationContext(t)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/nat", strings.NewReader(`{"name":"x","domain":"x.example","host":"127.0.0.1:80","server_id":9999}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	_, err := createNAT(ctx)
	assert.Error(t, err)
}
