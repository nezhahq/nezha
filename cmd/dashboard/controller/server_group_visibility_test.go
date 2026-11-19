package controller

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

func setupServerGroupVisibilityFixture(t *testing.T) {
	t.Helper()

	originalDB := singleton.DB
	originalCache := singleton.Cache
	originalLoc := singleton.Loc
	originalLocalizer := singleton.Localizer
	originalServer := singleton.ServerShared
	originalUserInfo := singleton.UserInfoMap

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Server{}, &model.ServerGroup{}, &model.ServerGroupServer{}, &model.User{}))

	singleton.DB = db
	singleton.Loc = time.UTC
	singleton.Cache = cache.New(time.Minute, time.Minute)
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	singleton.UserLock.Lock()
	singleton.UserInfoMap = map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		200: {Role: model.RoleMember},
	}
	singleton.UserLock.Unlock()

	require.NoError(t, db.Create(&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "public", UUID: "public", HideForGuest: false}).Error)
	require.NoError(t, db.Create(&model.Server{Common: model.Common{ID: 2, UserID: 1}, Name: "hidden", UUID: "hidden", HideForGuest: true}).Error)

	require.NoError(t, db.Create(&model.ServerGroup{Common: model.Common{ID: 10, UserID: 1}, Name: "Public Group"}).Error)
	require.NoError(t, db.Create(&model.ServerGroup{Common: model.Common{ID: 11, UserID: 1}, Name: "Empty Group"}).Error)
	require.NoError(t, db.Create(&model.ServerGroupServer{Common: model.Common{UserID: 1}, ServerGroupId: 10, ServerId: 1}).Error)

	singleton.ServerShared = singleton.NewServerClass()

	t.Cleanup(func() {
		_ = sqlDB.Close()
		singleton.DB = originalDB
		singleton.Cache = originalCache
		singleton.Loc = originalLoc
		singleton.Localizer = originalLocalizer
		singleton.ServerShared = originalServer
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfo
		singleton.UserLock.Unlock()
	})
}

func newServerGroupCtx(viewer *model.User) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/v1/server-group", nil)
	if viewer != nil {
		c.Set(model.CtxKeyAuthorizedUser, viewer)
	}
	return c
}

func collectGroupNames(items []*model.ServerGroupResponseItem) []string {
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Group.Name)
	}
	return names
}

func TestListServerGroupGuestSkipsGroupsWithoutVisibleServers(t *testing.T) {
	setupServerGroupVisibilityFixture(t)

	items, err := listServerGroup(newServerGroupCtx(nil))
	require.NoError(t, err)
	names := collectGroupNames(items)

	assert.ElementsMatch(t, []string{"Public Group"}, names,
		"a group with no guest-visible servers is meaningless to a guest UI and exposing its name leaks the existence of empty/hidden-only groups")
}

func TestListServerGroupAuthenticatedMemberSeesOwnEmptyGroup(t *testing.T) {
	setupServerGroupVisibilityFixture(t)

	require.NoError(t, singleton.DB.Create(&model.ServerGroup{Common: model.Common{ID: 12, UserID: 200}, Name: "member empty group"}).Error)

	items, err := listServerGroup(newServerGroupCtx(&model.User{
		Common: model.Common{ID: 200},
		Role:   model.RoleMember,
	}))
	require.NoError(t, err)
	names := collectGroupNames(items)

	assert.Contains(t, names, "member empty group", "owner must still see their own empty group")
}

func TestListServerGroupAdminSeesAllGroupsIncludingEmpty(t *testing.T) {
	setupServerGroupVisibilityFixture(t)

	items, err := listServerGroup(newServerGroupCtx(&model.User{
		Common: model.Common{ID: 1},
		Role:   model.RoleAdmin,
	}))
	require.NoError(t, err)
	names := collectGroupNames(items)

	assert.ElementsMatch(t, []string{"Public Group", "Empty Group"}, names,
		"admin must keep full visibility, including empty groups")
}

func newServerGroupCtxWithPAT(viewer *model.User, tok *model.APIToken) *gin.Context {
	c := newServerGroupCtx(viewer)
	if tok != nil {
		c.Set(model.CtxKeyAPIToken, tok)
	}
	return c
}

// PAT scoped to server_ids must hide groups whose membership is entirely
// outside the whitelist and must strip out-of-whitelist server IDs from
// remaining groups. Otherwise admin-issued limited PATs still enumerate
// every group name + server id via /api/v1/server-group.
func TestListServerGroupPATWhitelistFiltersGroupsAndServerIDs(t *testing.T) {
	setupServerGroupVisibilityFixture(t)

	require.NoError(t, singleton.DB.Create(&model.ServerGroupServer{
		Common: model.Common{UserID: 1}, ServerGroupId: 10, ServerId: 2,
	}).Error)

	tok := &model.APIToken{ID: 77, UserID: 1}
	tok.SetServerIDs([]uint64{1})

	items, err := listServerGroup(newServerGroupCtxWithPAT(&model.User{
		Common: model.Common{ID: 1}, Role: model.RoleAdmin,
	}, tok))
	require.NoError(t, err)

	names := collectGroupNames(items)
	assert.ElementsMatch(t, []string{"Public Group"}, names,
		"PAT scoped to {1} must drop the empty group and not surface group names containing only server 2")

	if assert.Len(t, items, 1) {
		assert.ElementsMatch(t, []uint64{1}, items[0].Servers,
			"server IDs outside the PAT whitelist must be redacted from the response")
	}
}

func TestListServerGroupPATWithDisjointWhitelistReturnsEmpty(t *testing.T) {
	setupServerGroupVisibilityFixture(t)

	tok := &model.APIToken{ID: 78, UserID: 1}
	tok.SetServerIDs([]uint64{9999})

	items, err := listServerGroup(newServerGroupCtxWithPAT(&model.User{
		Common: model.Common{ID: 1}, Role: model.RoleAdmin,
	}, tok))
	require.NoError(t, err)
	assert.Empty(t, items, "PAT scoped to a server it cannot reach must see no groups, not all of them")
}

// batchDeleteServerGroup must refuse to delete a group whose members are not
// entirely covered by the PAT whitelist; otherwise an admin's limited PAT can
// drop groups that touch servers outside its scope.
func TestBatchDeleteServerGroupRejectsPATOutsideWhitelist(t *testing.T) {
	setupServerGroupVisibilityFixture(t)
	require.NoError(t, singleton.DB.Create(&model.ServerGroupServer{
		Common: model.Common{UserID: 1}, ServerGroupId: 10, ServerId: 2,
	}).Error)

	tok := &model.APIToken{ID: 79, UserID: 1}
	tok.SetServerIDs([]uint64{1})

	c := newServerGroupCtxWithPAT(&model.User{
		Common: model.Common{ID: 1}, Role: model.RoleAdmin,
	}, tok)
	body, _ := json.Marshal([]uint64{10})
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/batch-delete/server-group", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	_, err := batchDeleteServerGroup(c)
	require.Error(t, err, "PAT scoped to {1} must not delete group 10 which still contains server 2")

	var remaining int64
	require.NoError(t, singleton.DB.Model(&model.ServerGroup{}).Where("id = ?", 10).Count(&remaining).Error)
	assert.Equal(t, int64(1), remaining, "group 10 must remain after refused PAT delete")
}

// A group without members has nothing that places it inside a PAT whitelist
// (listServerGroup hides it from such a PAT), so the membership check must
// not pass vacuously: otherwise a PAT scoped to {1} can delete any empty
// group its issuer may manage — every user's, for an admin-issued PAT.
func TestBatchDeleteServerGroupRejectsPATWhitelistOnEmptyGroup(t *testing.T) {
	setupServerGroupVisibilityFixture(t)

	tok := &model.APIToken{ID: 80, UserID: 1}
	tok.SetServerIDs([]uint64{1})
	admin := &model.User{Common: model.Common{ID: 1}, Role: model.RoleAdmin}

	// 11 alone, and 11 batched with a group the whitelist does cover.
	for _, ids := range [][]uint64{{11}, {10, 11}} {
		c := newServerGroupCtxWithPAT(admin, tok)
		body, _ := json.Marshal(ids)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/batch-delete/server-group", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")

		_, err := batchDeleteServerGroup(c)
		require.Error(t, err, "PAT scoped to {1} must not delete %v: group 11 has no member inside the whitelist", ids)
	}

	var remaining int64
	require.NoError(t, singleton.DB.Model(&model.ServerGroup{}).Where("id in (?)", []uint64{10, 11}).Count(&remaining).Error)
	assert.Equal(t, int64(2), remaining, "groups 10 and 11 must remain after refused PAT deletes")

	// The whitelist still covers group 10 on its own.
	c := newServerGroupCtxWithPAT(admin, tok)
	body, _ := json.Marshal([]uint64{10})
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/batch-delete/server-group", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	_, err := batchDeleteServerGroup(c)
	require.NoError(t, err, "PAT scoped to {1} may delete group 10, whose only member is server 1")

	// An unscoped PAT has no whitelist to escape.
	c = newServerGroupCtxWithPAT(admin, &model.APIToken{ID: 81, UserID: 1})
	body, _ = json.Marshal([]uint64{11})
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/batch-delete/server-group", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	_, err = batchDeleteServerGroup(c)
	require.NoError(t, err, "PAT without server_ids must still delete an empty group")
}

// updateServerGroup shares the check: a PAT scoped to {1} must not rename an
// empty group and move its own server into it.
func TestUpdateServerGroupRejectsPATWhitelistOnEmptyGroup(t *testing.T) {
	setupServerGroupVisibilityFixture(t)

	tok := &model.APIToken{ID: 82, UserID: 1}
	tok.SetServerIDs([]uint64{1})

	c := newServerGroupCtxWithPAT(&model.User{
		Common: model.Common{ID: 1}, Role: model.RoleAdmin,
	}, tok)
	body, _ := json.Marshal(model.ServerGroupForm{Name: "taken", Servers: []uint64{1}})
	c.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/server-group/11", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "11"}}

	_, err := updateServerGroup(c)
	require.Error(t, err, "PAT scoped to {1} must not rewrite group 11, which has no member inside the whitelist")

	var group model.ServerGroup
	require.NoError(t, singleton.DB.First(&group, 11).Error)
	assert.Equal(t, "Empty Group", group.Name, "group 11 must keep its name after the refused PAT update")
	var members int64
	require.NoError(t, singleton.DB.Model(&model.ServerGroupServer{}).Where("server_group_id = ?", 11).Count(&members).Error)
	assert.Zero(t, members, "group 11 must stay empty after the refused PAT update")
}

func serversOfGroup(items []*model.ServerGroupResponseItem, groupID uint64) ([]uint64, bool) {
	for _, it := range items {
		if it.Group.ID == groupID {
			return it.Servers, true
		}
	}
	return nil, false
}

// A membership row is written under the writer's authority over the server
// (CheckPermission). Once the server is transferred away the row must stop
// counting: otherwise the former owner keeps the server under their own group
// label, which the new owner can neither see in the dashboard nor edit.
func TestListServerGroupDropsMembershipAfterServerChangedOwner(t *testing.T) {
	setupServerGroupVisibilityFixture(t)

	singleton.UserLock.Lock()
	singleton.UserInfoMap[201] = model.UserInfo{Role: model.RoleMember}
	singleton.UserLock.Unlock()

	// server 3 was grouped by member 200 and by the admin, then moved to member 201.
	require.NoError(t, singleton.DB.Create(&model.Server{Common: model.Common{ID: 3, UserID: 201}, Name: "moved", UUID: "moved"}).Error)
	require.NoError(t, singleton.DB.Create(&model.ServerGroup{Common: model.Common{ID: 12, UserID: 200}, Name: "former owner group"}).Error)
	require.NoError(t, singleton.DB.Create(&model.ServerGroupServer{Common: model.Common{UserID: 200}, ServerGroupId: 12, ServerId: 3}).Error)
	require.NoError(t, singleton.DB.Create(&model.ServerGroupServer{Common: model.Common{UserID: 1}, ServerGroupId: 10, ServerId: 3}).Error)
	singleton.ServerShared = singleton.NewServerClass()

	guestItems, err := listServerGroup(newServerGroupCtx(nil))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Public Group"}, collectGroupNames(guestItems),
		"guests must not see the server under its former owner's group")
	adminGroupServers, _ := serversOfGroup(guestItems, 10)
	assert.ElementsMatch(t, []uint64{1, 3}, adminGroupServers, "a membership written by an admin stays in force")

	for _, viewer := range []*model.User{
		{Common: model.Common{ID: 200}, Role: model.RoleMember},
		{Common: model.Common{ID: 1}, Role: model.RoleAdmin},
	} {
		items, err := listServerGroup(newServerGroupCtx(viewer))
		require.NoError(t, err)
		servers, found := serversOfGroup(items, 12)
		assert.True(t, found, "the group itself stays listed for its owner and for admins")
		assert.Empty(t, servers, "viewer %d must not get the transferred server in the former owner's group", viewer.ID)
	}

	// Handing the server back (cancelled transfer) puts the membership in force again.
	moved, _ := singleton.ServerShared.Get(3)
	moved.SetUserID(200)
	items, err := listServerGroup(newServerGroupCtx(&model.User{Common: model.Common{ID: 200}, Role: model.RoleMember}))
	require.NoError(t, err)
	servers, _ := serversOfGroup(items, 12)
	assert.ElementsMatch(t, []uint64{3}, servers)
}

// The HideForGuest check must not be guest-only: a member who does not own a
// hidden server may not view it anywhere else (userCanViewServer), so their
// own group listing must not name it either.
func TestListServerGroupMemberDoesNotSeeHiddenForeignServer(t *testing.T) {
	setupServerGroupVisibilityFixture(t)

	// The admin put their own servers 1 (public) and 2 (hidden) into member 200's group.
	require.NoError(t, singleton.DB.Create(&model.ServerGroup{Common: model.Common{ID: 12, UserID: 200}, Name: "member group"}).Error)
	require.NoError(t, singleton.DB.Create(&model.ServerGroupServer{Common: model.Common{UserID: 1}, ServerGroupId: 12, ServerId: 1}).Error)
	require.NoError(t, singleton.DB.Create(&model.ServerGroupServer{Common: model.Common{UserID: 1}, ServerGroupId: 12, ServerId: 2}).Error)

	items, err := listServerGroup(newServerGroupCtx(&model.User{Common: model.Common{ID: 200}, Role: model.RoleMember}))
	require.NoError(t, err)
	servers, found := serversOfGroup(items, 12)
	require.True(t, found)
	assert.ElementsMatch(t, []uint64{1}, servers, "a hidden server of another user must be withheld from a member")

	items, err = listServerGroup(newServerGroupCtx(&model.User{Common: model.Common{ID: 1}, Role: model.RoleAdmin}))
	require.NoError(t, err)
	servers, _ = serversOfGroup(items, 12)
	assert.ElementsMatch(t, []uint64{1, 2}, servers, "admin keeps full visibility")
}
