package singleton

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

func sourceUserMayControl(serverID, userID uint64) bool {
	release, _, err := BeginServerControl(serverID, userID, false)
	if err != nil {
		return false
	}
	release()
	return true
}

// An agent on plaintext gRPC, with InsecureTLS or with command execution
// disabled refuses every credential rotation, the rollback included. It never
// changed its credential, yet the source user was kept out of the server's
// terminal, files and NAT until the dashboard restarted.
func TestServerControl_SourceUserKeepsControlAfterTheAgentRefusedTheTransfer(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	require.True(t, sourceUserMayControl(1, 100), "test setup: the owner controls the server before any transfer")

	transfer := initiateAndRegister(t, c, 1, 100, 200, 1)
	require.False(t, sourceUserMayControl(1, 100), "a pending transfer keeps the source user out")
	_, err := c.MarkFailed(transfer.ID, "ServerTransferApply rejected: rotated secret cannot be delivered over plaintext or InsecureTLS")
	require.NoError(t, err)

	require.True(t, sourceUserMayControl(1, 100), "the agent refused and kept its credential; the source user must have the server back")
	require.False(t, sourceUserMayControl(1, 200), "the destination user never gained the server")
}

// A cancelled transfer may have reached the agent, so control waits for the
// agent to rotate back. If that confirmation never comes the wait has to end
// with the recovery window instead of lasting until the next restart.
func TestServerControl_RollbackWaitEndsWithTheRecoveryWindow(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	transfer := initiateAndRegister(t, c, 1, 100, 200, 1)
	_, err := c.Cancel(transfer.ID)
	require.NoError(t, err)
	require.False(t, sourceUserMayControl(1, 100), "an unconfirmed rollback inside the window keeps control closed")

	c.mu.Lock()
	expired := time.Now().Add(-defaultRevertDeliveryRecoveryWindow - time.Minute)
	c.revertDeliveries[1].UpdatedAt = expired
	c.terminalSecretRecovery[1].UpdatedAt = expired
	c.mu.Unlock()

	require.True(t, sourceUserMayControl(1, 100), "the recovery window is over; nothing is waited for any more")
}

// After a restart only the newest transfer of a server can still await its
// rollback. An older unconfirmed rollback was superseded when the later
// transfer was registered; bringing it back pushed its retired secret to the
// agent on reconnect and re-admitted that secret in authentication.
func TestNewServerTransferClass_DoesNotReviveARollbackSupersededByALaterTransfer(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	superseded := initiateAndRegister(t, c, 1, 100, 200, 1)
	_, err := c.Cancel(superseded.ID)
	require.NoError(t, err)
	later := initiateAndRegister(t, c, 1, 100, 300, 1)
	_, err = c.Cancel(later.ID)
	require.NoError(t, err)
	require.NoError(t, c.MarkRevertDelivered(1, later.ID), "the agent confirmed the later rollback")
	_, stillQueued := c.LookupRevertDelivery(1)
	require.False(t, stillQueued, "test setup: nothing awaits delivery before the restart")
	var supersededRow, laterRow model.ServerTransfer
	require.NoError(t, DB.First(&supersededRow, superseded.ID).Error)
	require.NoError(t, DB.First(&laterRow, later.ID).Error)
	require.Nil(t, supersededRow.AckedAt, "test setup: the first rollback was never confirmed")
	require.NotEmpty(t, supersededRow.RevertHandshakeSecret)
	c.Stop()

	reborn := NewServerTransferClass()
	defer reborn.Stop()
	ServerTransferShared = reborn

	revived, queued := reborn.LookupRevertDelivery(1)
	require.False(t, queued, "the superseded rollback %v is queued for delivery again", revived)
	_, admitted := reborn.LookupByRevertHandshakeSecret(supersededRow.RevertHandshakeSecret)
	require.False(t, admitted, "the superseded rollback secret authenticates again")
	require.True(t, sourceUserMayControl(1, 100), "the owner is kept out by a rollback that was already superseded")
	require.Equal(t, laterRow.RevertHandshakeSecret, reborn.verifiedHandshakes[1], "the agent's confirmed credential must survive the restart")
}
