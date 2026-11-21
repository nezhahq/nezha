package singleton

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

// The alert check holds the alerts lock and then reads the user table for every
// server; handling a monitor result takes the server list lock first. Deleting
// a user took those locks while it still held the user lock, so the two sides
// waited for each other forever and every Agent login hung with them.
func TestOnUserDeleteDoesNotHoldUserLockWhileWaitingForTheAlertsLock(t *testing.T) {
	_, cleanup := setupOnUserDeleteFixture(t)
	defer cleanup()
	const user = uint64(100)
	const serverID = uint64(1)
	seedServerForTransfer(t, serverID, user)
	require.NoError(t, DB.AutoMigrate(&model.User{}))
	require.NoError(t, DB.Create(&model.User{Common: model.Common{ID: user}, Username: "alice", AgentSecret: "alice-secret"}).Error)
	UserLock.Lock()
	UserInfoMap[user] = model.UserInfo{Role: model.RoleMember, Username: "alice", AgentSecret: "alice-secret"}
	UserLock.Unlock()

	// An alert check is in flight.
	AlertsLock.RLock()
	alertCheckFinished := false
	defer func() {
		if !alertCheckFinished {
			AlertsLock.RUnlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- OnUserDelete([]uint64{user}, func(string, ...any) error { return nil })
	}()

	// The check reaches its user lookup once the user is gone from the table.
	awaitWhileUnregistering(t, func() bool {
		_, known := lookupServerOwner(user)
		return !known
	}, "the alert check's user lookup is blocked behind the deletion: both sides wait for each other")
	select {
	case err := <-done:
		t.Fatalf("OnUserDelete returned %v while the alert check still held the alerts lock", err)
	case <-time.After(50 * time.Millisecond):
	}
	_, stillListed := ServerShared.Get(serverID)
	require.True(t, stillListed, "test setup: the server is removed only after the alert check lets go")

	AlertsLock.RUnlock()
	alertCheckFinished = true
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("OnUserDelete did not finish after the alert check released the alerts lock")
	}
	_, listed := ServerShared.Get(serverID)
	require.False(t, listed, "the deleted user's server must be removed from the server list")
}
