package singleton

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
)

func setupOnUserDeleteFixture(t *testing.T) (*ServerTransferClass, func()) {
	t.Helper()
	c, transferCleanup := setupTransferFixture(t)

	require.NoError(t, DB.AutoMigrate(&model.Cron{}, &model.Transfer{}, &model.ServerGroupServer{}))
	originalCronShared := CronShared
	CronShared = &CronClass{
		class: class[uint64, *model.Cron]{list: map[uint64]*model.Cron{}},
	}

	originalLocalizer := Localizer
	Localizer = i18n.NewLocalizer("zh_CN", domain, "translations", i18n.Translations)

	cleanup := func() {
		Localizer = originalLocalizer
		CronShared = originalCronShared
		transferCleanup()
	}
	return c, cleanup
}

func TestOnUserDeleteCancelsPendingTransfersAwayFromDeletedUser(t *testing.T) {
	c, cleanup := setupOnUserDeleteFixture(t)
	defer cleanup()

	const fromUser = uint64(100)
	const toUser = uint64(200)
	const serverID = uint64(1)

	seedServerForTransfer(t, serverID, fromUser)

	require.NoError(t, DB.AutoMigrate(&model.User{}))
	require.NoError(t, DB.Create(&model.User{
		Common:      model.Common{ID: fromUser},
		Username:    "alice",
		AgentSecret: "alice-secret",
	}).Error)
	require.NoError(t, DB.Create(&model.User{
		Common:      model.Common{ID: toUser},
		Username:    "bob",
		AgentSecret: "bob-secret",
	}).Error)
	UserLock.Lock()
	UserInfoMap[fromUser] = model.UserInfo{Role: model.RoleMember, AgentSecret: "alice-secret"}
	UserInfoMap[toUser] = model.UserInfo{Role: model.RoleMember, AgentSecret: "bob-secret"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, serverID, fromUser, toUser, fromUser)
	require.True(t, c.HasPending(serverID), "precondition: pending transfer published")

	srv, ok := ServerShared.Get(serverID)
	require.True(t, ok)
	require.Equal(t, toUser, srv.GetUserID(), "precondition: pending transfer flipped owner to ToUserID")

	require.NoError(t, OnUserDelete([]uint64{fromUser}, func(format string, args ...any) error {
		return nil
	}))

	if c.HasPending(serverID) {
		t.Fatal("OnUserDelete on the transfer FromUserID must terminate the pending transfer so a later Cancel/Fail/Timeout cannot revert ownership to the deleted user")
	}

	if srv, ok := ServerShared.Get(serverID); ok {
		require.NotEqual(t, fromUser, srv.GetUserID(),
			"server owner must not be reverted to the deleted FromUserID; got owner=%d", srv.GetUserID())
	}

	if _, err := c.Cancel(tr.ID); err == nil {
		var refreshed model.ServerTransfer
		if err := DB.First(&refreshed, tr.ID).Error; err == nil {
			require.NotEqual(t, model.ServerTransferStatusPending, refreshed.Status,
				"after OnUserDelete a subsequent Cancel must not leave the transfer Pending")
			if srv, ok := ServerShared.Get(serverID); ok {
				require.NotEqual(t, fromUser, srv.GetUserID(),
					"a late Cancel against the terminated transfer must not revert server.UserID to the deleted FromUserID; got owner=%d", srv.GetUserID())
			}
		}
	}
}

// Deleting the destination user of a Pending transfer must hand the server
// back to its source user, as a cancel would: Register attributed it to the
// destination already, but it is not theirs until the agent has rotated.
// Deleting it along with them would also free its UUID for re-enrolment.
func TestOnUserDeleteReturnsPendingServerToSource(t *testing.T) {
	c, cleanup := setupOnUserDeleteFixture(t)
	defer cleanup()

	const fromUser = uint64(100)
	const toUser = uint64(200)
	const pendingServer = uint64(1)
	const ownServer = uint64(2)

	seedServerForTransfer(t, pendingServer, fromUser)
	seedServerForTransfer(t, ownServer, toUser)

	require.NoError(t, DB.AutoMigrate(&model.User{}))
	require.NoError(t, DB.Create(&model.User{
		Common:      model.Common{ID: fromUser},
		Username:    "alice",
		AgentSecret: "alice-secret",
	}).Error)
	require.NoError(t, DB.Create(&model.User{
		Common:      model.Common{ID: toUser},
		Username:    "bob",
		AgentSecret: "bob-secret",
	}).Error)
	UserLock.Lock()
	UserInfoMap[fromUser] = model.UserInfo{Role: model.RoleMember, AgentSecret: "alice-secret"}
	UserInfoMap[toUser] = model.UserInfo{Role: model.RoleMember, AgentSecret: "bob-secret"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, pendingServer, fromUser, toUser, fromUser)
	srv, ok := ServerShared.Get(pendingServer)
	require.True(t, ok)
	require.Equal(t, toUser, srv.GetUserID(), "precondition: pending transfer flipped owner to ToUserID")

	require.NoError(t, OnUserDelete([]uint64{toUser}, func(format string, args ...any) error {
		return fmt.Errorf(format, args...)
	}))

	kept, ok := ServerShared.Get(pendingServer)
	require.True(t, ok, "deleting the destination of a pending transfer must not delete the source user's server")
	require.Equal(t, fromUser, kept.GetUserID())
	id, ok := ServerShared.UUIDToID(srv.UUID)
	require.True(t, ok, "the UUID must stay registered")
	require.Equal(t, pendingServer, id)
	var row model.Server
	require.NoError(t, DB.First(&row, pendingServer).Error)
	require.Equal(t, fromUser, row.UserID)

	require.False(t, c.HasPending(pendingServer))
	var settled model.ServerTransfer
	require.NoError(t, DB.First(&settled, tr.ID).Error)
	require.Equal(t, model.ServerTransferStatusCancelled, settled.Status)

	_, ok = ServerShared.Get(ownServer)
	require.False(t, ok, "a server the deleted user really owns still goes with them")
	UserLock.RLock()
	_, ok = UserInfoMap[toUser]
	UserLock.RUnlock()
	require.False(t, ok)
}

// When both ends of a Pending transfer are deleted together there is no
// surviving source to hand the server back to, so it goes with them.
func TestOnUserDeleteDeletesPendingServerWhenSourceIsDeletedToo(t *testing.T) {
	c, cleanup := setupOnUserDeleteFixture(t)
	defer cleanup()

	const fromUser = uint64(100)
	const toUser = uint64(200)
	const serverID = uint64(1)

	seedServerForTransfer(t, serverID, fromUser)
	require.NoError(t, DB.AutoMigrate(&model.User{}))

	tr := initiateAndRegister(t, c, serverID, fromUser, toUser, fromUser)

	require.NoError(t, OnUserDelete([]uint64{fromUser, toUser}, func(format string, args ...any) error {
		return fmt.Errorf(format, args...)
	}))

	_, ok := ServerShared.Get(serverID)
	require.False(t, ok, "neither owner survives, so the server must not be kept for a deleted user")
	require.False(t, c.HasPending(serverID))
	var settled model.ServerTransfer
	require.NoError(t, DB.First(&settled, tr.ID).Error)
	require.Equal(t, model.ServerTransferStatusCancelled, settled.Status)
}
