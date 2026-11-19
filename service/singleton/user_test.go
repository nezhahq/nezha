package singleton

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	pb "github.com/nezhahq/nezha/proto"
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

type fakeStateStream struct {
	pb.NezhaService_ReportSystemStateServer
}

// Deleting the source user of a Pending transfer leaves the server with
// ToUserID without MarkVerified and without a rollback. Everything the source
// side holds as that server's agent must end with it: the open RequestTask /
// ReportSystemState streams, and the credentials of an EARLIER transfer that
// Register keeps alive for the Pending window.
func TestOnUserDeleteEndsSourceSideAgentAccess(t *testing.T) {
	c, cleanup := setupOnUserDeleteFixture(t)
	defer cleanup()

	const fromUser = uint64(100)
	const toUser = uint64(200)
	// recoveryServer: the earlier transfer was cancelled and its rollback is
	// still unacknowledged (terminalSecretRecovery). ackedServer: the agent
	// already rotated to the rollback secret (verifiedHandshakes).
	const recoveryServer = uint64(1)
	const ackedServer = uint64(2)

	require.NoError(t, DB.AutoMigrate(&model.User{}))
	require.NoError(t, DB.Create(&model.User{
		Common:      model.Common{ID: fromUser},
		Username:    "alice",
		AgentSecret: "alice-secret",
	}).Error)

	originalHook := ServerTransferStreamRevocationHook
	defer func() { ServerTransferStreamRevocationHook = originalHook }()
	ServerTransferStreamRevocationHook = nil

	cancelled := make(map[uint64]*model.ServerTransfer)
	for _, serverID := range []uint64{recoveryServer, ackedServer} {
		seedServerForTransfer(t, serverID, fromUser)
		tr := initiateAndRegister(t, c, serverID, fromUser, toUser, fromUser)
		_, err := c.Cancel(tr.ID)
		require.NoError(t, err)
		cancelled[serverID] = tr
	}
	require.NoError(t, c.MarkRevertDelivered(ackedServer, cancelled[ackedServer].ID))
	_, _, admitted := c.LookupByTerminalSecretRecovery(cancelled[recoveryServer].RevertHandshakeSecret)
	require.True(t, admitted, "precondition: the cancelled transfer's rollback secret is in its recovery window")
	_, admitted = c.LookupServerByVerifiedHandshakeSecret(cancelled[ackedServer].RevertHandshakeSecret)
	require.True(t, admitted, "precondition: the acknowledged rollback secret is the agent's long-term credential")

	leases := make(map[uint64]model.StateStreamLease)
	for _, serverID := range []uint64{recoveryServer, ackedServer} {
		initiateAndRegister(t, c, serverID, fromUser, toUser, fromUser)
		srv, ok := ServerShared.Get(serverID)
		require.True(t, ok)
		srv.SetTaskStream(newFakeTaskStream())
		leases[serverID] = srv.AttachStateStream(&fakeStateStream{})
		require.True(t, leases[serverID].UpdateState(&model.HostState{}, time.Now()), "precondition: state stream is live")
		require.True(t, ServerControlTransitioning(serverID), "precondition: the destination user is held back while Pending")
	}

	var revoked []uint64
	ServerTransferStreamRevocationHook = func(serverID uint64) { revoked = append(revoked, serverID) }

	require.NoError(t, OnUserDelete([]uint64{fromUser}, func(format string, args ...any) error {
		return nil
	}))

	for _, serverID := range []uint64{recoveryServer, ackedServer} {
		srv, ok := ServerShared.Get(serverID)
		require.True(t, ok)
		require.Equal(t, toUser, srv.GetUserID(), "server %d stays with the surviving ToUserID", serverID)
		require.Nil(t, srv.GetTaskStream(),
			"server %d: the task stream opened on the deleted user's side must be detached", serverID)
		require.False(t, leases[serverID].UpdateState(&model.HostState{}, time.Now()),
			"server %d: the state stream opened on the deleted user's side must be refused", serverID)
		require.False(t, ServerControlTransitioning(serverID),
			"server %d: the new owner must not stay locked out by a stale credential slot", serverID)
	}
	require.ElementsMatch(t, []uint64{recoveryServer, ackedServer}, revoked,
		"IOStreams of the affected servers must be revoked")

	for _, secret := range []string{cancelled[recoveryServer].HandshakeSecret, cancelled[recoveryServer].RevertHandshakeSecret} {
		_, _, admitted := c.LookupByTerminalSecretRecovery(secret)
		require.False(t, admitted, "a secret of the earlier, cancelled transfer must not authenticate as the new owner's server")
	}
	_, admitted = c.LookupServerByVerifiedHandshakeSecret(cancelled[ackedServer].RevertHandshakeSecret)
	require.False(t, admitted, "the earlier transfer's acknowledged rollback secret must not authenticate as the new owner's server")
}
