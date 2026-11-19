package controller

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/nezhahq/nezha/model"
)

func newServiceVisibilityCtx(viewer *model.User) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if viewer != nil {
		c.Set(model.CtxKeyAuthorizedUser, viewer)
	}
	return c
}

func TestUserCanViewServiceVisibleServiceIsPublic(t *testing.T) {
	visible := &model.Service{Common: model.Common{ID: 1, UserID: 100}, HideForGuest: false}
	assert.True(t, userCanViewService(newServiceVisibilityCtx(nil), visible), "guest must see HideForGuest=false regardless of owner")
}

func TestUserCanViewServiceHiddenServiceRejectsGuest(t *testing.T) {
	hidden := &model.Service{Common: model.Common{ID: 1, UserID: 100}, HideForGuest: true}
	assert.False(t, userCanViewService(newServiceVisibilityCtx(nil), hidden), "guest must NOT see hidden service via per-server / per-id sideband endpoints")
}

func TestUserCanViewServiceHiddenServiceRejectsForeignMember(t *testing.T) {
	hidden := &model.Service{Common: model.Common{ID: 1, UserID: 100}, HideForGuest: true}
	foreign := &model.User{Common: model.Common{ID: 200}, Role: model.RoleMember}
	assert.False(t, userCanViewService(newServiceVisibilityCtx(foreign), hidden), "foreign member must NOT see another user's hidden service")
}

func TestUserCanViewServiceHiddenServiceAllowsOwner(t *testing.T) {
	hidden := &model.Service{Common: model.Common{ID: 1, UserID: 100}, HideForGuest: true}
	owner := &model.User{Common: model.Common{ID: 100}, Role: model.RoleMember}
	assert.True(t, userCanViewService(newServiceVisibilityCtx(owner), hidden), "owner must still see their own hidden service")
}

func TestUserCanViewServiceHiddenServiceAllowsAdmin(t *testing.T) {
	hidden := &model.Service{Common: model.Common{ID: 1, UserID: 100}, HideForGuest: true}
	admin := &model.User{Common: model.Common{ID: 1}, Role: model.RoleAdmin}
	assert.True(t, userCanViewService(newServiceVisibilityCtx(admin), hidden), "admin must be able to see any hidden service")
}

// 钉死 admin 自己签发的 server_ids 受限 PAT 不能借助 admin 身份在
// service 可见性入口绕过白名单：与 userCanViewServer 的 PAT-first 收口
// 保持对称，避免 hidden service 通过 admin 早返回泄漏给受限 PAT。
func TestUserCanViewServiceLimitedPATShouldDenyAdminWhenOutsideWhitelist(t *testing.T) {
	ensureLocalizerForStreamTests(t)
	hidden := &model.Service{
		Common:       model.Common{ID: 1, UserID: 100},
		Cover:        model.ServiceCoverIgnoreAll,
		SkipServers:  map[uint64]bool{2: true},
		HideForGuest: true,
	}
	admin := &model.User{Common: model.Common{ID: 1}, Role: model.RoleAdmin}
	tok := &model.APIToken{ID: 7, UserID: 1}
	tok.SetServerIDs([]uint64{1})

	ctx := newServiceVisibilityCtx(admin)
	ctx.Set(model.CtxKeyAPIToken, tok)

	assert.False(t, userCanViewService(ctx, hidden),
		"admin caller using a server_ids=[1] PAT must NOT see a CoverIgnoreAll service whose only target is the non-whitelisted server 2")
}

func TestUserCanViewServiceLimitedPATAllowsAdminInsideWhitelist(t *testing.T) {
	visible := &model.Service{
		Common:       model.Common{ID: 2, UserID: 100},
		Cover:        model.ServiceCoverIgnoreAll,
		SkipServers:  map[uint64]bool{1: true},
		HideForGuest: true,
	}
	admin := &model.User{Common: model.Common{ID: 1}, Role: model.RoleAdmin}
	tok := &model.APIToken{ID: 7, UserID: 1}
	tok.SetServerIDs([]uint64{1})

	ctx := newServiceVisibilityCtx(admin)
	ctx.Set(model.CtxKeyAPIToken, tok)

	assert.True(t, userCanViewService(ctx, visible),
		"admin caller using a server_ids=[1] PAT must still see a CoverIgnoreAll service bound to whitelisted server 1")
}

// PAT 白名单对公开（HideForGuest=false）的 service 同样生效：GET /service 与
// /service/:id/history 必须和 /service/list、userCanViewServer 给出同一条边界。
func TestUserCanViewServiceLimitedPATDeniesVisibleServiceOutsideWhitelist(t *testing.T) {
	setupCoverFanoutFixture(t)
	owner := &model.User{Common: model.Common{ID: 100}, Role: model.RoleMember}
	tok := &model.APIToken{ID: 7, UserID: 100}
	tok.SetServerIDs([]uint64{1})
	ctx := newServiceVisibilityCtx(owner)
	ctx.Set(model.CtxKeyAPIToken, tok)

	outside := &model.Service{Common: model.Common{ID: 1, UserID: 100}, Cover: model.ServiceCoverIgnoreAll, SkipServers: map[uint64]bool{2: true}}
	assert.False(t, userCanViewService(ctx, outside),
		"server_ids=[1] PAT must NOT see a visible CoverIgnoreAll service whose only target is the non-whitelisted server 2")

	inside := &model.Service{Common: model.Common{ID: 2, UserID: 100}, Cover: model.ServiceCoverIgnoreAll, SkipServers: map[uint64]bool{1: true}}
	assert.True(t, userCanViewService(ctx, inside),
		"the same PAT must still see a visible service bound to whitelisted server 1")

	coverAll := &model.Service{Common: model.Common{ID: 3, UserID: 100}, Cover: model.ServiceCoverAll}
	assert.False(t, userCanViewService(ctx, coverAll),
		"server_ids=[1] PAT must NOT see a visible CoverAll service that also runs on non-whitelisted owner servers")

	coverAll.SkipServers = map[uint64]bool{2: true, 3: true}
	assert.True(t, userCanViewService(ctx, coverAll),
		"CoverAll service whose SkipServers excludes every non-whitelisted owner server stays visible")

	assert.True(t, userCanViewService(newServiceVisibilityCtx(owner), outside),
		"session (no PAT) callers are unaffected")
}
