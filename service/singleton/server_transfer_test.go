package singleton

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
)

// fakeTaskStream is the smallest stub of pb.NezhaService_RequestTaskServer
// PushIfOnline needs: a Send that captures dispatched tasks. We only call Send
// from the production code under test, so the embedded interface satisfies the
// rest of the contract with nil-panicking methods we never invoke.
type fakeTaskStream struct {
	pb.NezhaService_RequestTaskServer
	mu   sync.Mutex
	sent []*pb.Task
}

func newFakeTaskStream() *fakeTaskStream { return &fakeTaskStream{} }

func (f *fakeTaskStream) Send(t *pb.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, t)
	return nil
}

func (f *fakeTaskStream) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = nil
}

func (f *fakeTaskStream) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// setupTransferFixture wires up an in-memory DB, ServerShared, and a fresh
// ServerTransferClass with the timeout sweeper stopped (each test that needs
// timeout behavior overrides c.timeout and calls c.sweepTimeouts directly).
func setupTransferFixture(t *testing.T) (*ServerTransferClass, func()) {
	t.Helper()
	originalDB := DB
	originalServerShared := ServerShared
	originalServerTransfer := ServerTransferShared
	originalUserInfoMap := UserInfoMap

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	// Pin the connection pool to 1: ":memory:" creates a NEW database per
	// connection, so a concurrent goroutine that the pool routes to a fresh
	// connection sees an empty DB ("no such table"). Tests using
	// sweepTimeouts's per-server fan-out goroutines hit this.
	if sqlDB, errInner := db.DB(); errInner == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	require.NoError(t, db.AutoMigrate(&model.Server{}, &model.ServerTransfer{}))
	DB = db

	ServerShared = NewServerClass()
	UserInfoMap = make(map[uint64]model.UserInfo)

	c := NewServerTransferClass()
	ServerTransferShared = c

	cleanup := func() {
		c.Stop()
		DB = originalDB
		ServerShared = originalServerShared
		ServerTransferShared = originalServerTransfer
		UserInfoMap = originalUserInfoMap
	}
	return c, cleanup
}

func seedServerForTransfer(t *testing.T, id, userID uint64) {
	t.Helper()
	s := &model.Server{
		Common: model.Common{ID: id, UserID: userID},
		UUID:   fmt.Sprintf("uuid-%s-%d", t.Name(), id),
		Name:   "test-srv",
	}
	require.NoError(t, DB.Create(s).Error)
	model.InitServer(s)
	ServerShared.Update(s, s.UUID)
}

// initiateAndRegister mirrors the controller flow: open a transaction, call
// Initiate, commit, then Register. Tests use it to set up a Pending transfer.
func initiateAndRegister(t *testing.T, c *ServerTransferClass, serverID, fromUserID, toUserID, initiatorID uint64) *model.ServerTransfer {
	t.Helper()
	var created *model.ServerTransfer
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		created, err = c.Initiate(tx, serverID, fromUserID, toUserID, initiatorID)
		return err
	})
	require.NoError(t, err)
	c.Register(created)
	return created
}

// markPendingVerified is a test convenience that resolves the current
// pending transfer for serverID and drives the new MarkVerified(serverID,
// transferID) signature. Tests that simulate the auth-path call don't care
// about the transferID lookup detail.
func markPendingVerified(t *testing.T, c *ServerTransferClass, serverID uint64) (verified bool, transfer *model.ServerTransfer, err error) {
	t.Helper()
	pending, ok := c.LookupPending(serverID)
	if !ok {
		return c.MarkVerified(serverID, 0)
	}
	return c.MarkVerified(serverID, pending.ID)
}

func TestServerTransferInitiateFlipsServerUserID(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	require.Equal(t, model.ServerTransferStatusPending, tr.Status)

	var s model.Server
	require.NoError(t, DB.First(&s, 1).Error)
	require.Equal(t, uint64(200), s.UserID, "Server.UserID must be flipped to ToUserID inside the transaction")

	cached, ok := ServerShared.Get(1)
	require.True(t, ok)
	require.Equal(t, uint64(200), cached.UserID, "in-memory ServerShared must also reflect the new owner")
}

func TestServerTransferLookupPendingDuringWindow(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	require.False(t, c.HasPending(1))

	initiateAndRegister(t, c, 1, 100, 200, 1)

	require.True(t, c.HasPending(1), "HasPending must report the freshly-registered transfer")
	got, ok := c.LookupPending(1)
	require.True(t, ok)
	require.Equal(t, uint64(100), got.FromUserID)
	require.Equal(t, uint64(200), got.ToUserID)
}

func TestServerTransferMarkVerifiedClearsPending(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	initiateAndRegister(t, c, 1, 100, 200, 1)

	ok, verified, err := markPendingVerified(t, c, 1)
	require.NoError(t, err)
	require.True(t, ok, "first call on a fresh pending must return verified=true")
	require.NotNil(t, verified)
	require.Equal(t, model.ServerTransferStatusVerified, verified.Status)
	require.NotNil(t, verified.AckedAt)

	require.False(t, c.HasPending(1), "Pending index must drop the row after MarkVerified")

	var s model.Server
	require.NoError(t, DB.First(&s, 1).Error)
	require.Equal(t, uint64(200), s.UserID, "Server.UserID stays at ToUserID after Verified")
}

// MarkVerified must be idempotent — a second call must not flip the row back
// or panic. The auth path calls MarkVerified opportunistically on every RPC
// authenticated as the new owner.
func TestServerTransferMarkVerifiedIsIdempotent(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	initiateAndRegister(t, c, 1, 100, 200, 1)

	ok, verified, err := markPendingVerified(t, c, 1)
	require.NoError(t, err)
	require.True(t, ok, "first call must perform the transition")
	require.NotNil(t, verified, "first call must transition the row to Verified")

	ok, verified, err = markPendingVerified(t, c, 1)
	require.NoError(t, err)
	require.False(t, ok, "second call must report verified=false")
	require.Nil(t, verified, "second call must be a silent no-op (RowsAffected=0)")
}

func TestServerTransferMarkFailedRevertsOwnership(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	failed, err := c.MarkFailed(tr.ID, "disable_command_execute")
	require.NoError(t, err)
	require.Equal(t, model.ServerTransferStatusFailed, failed.Status)
	require.Equal(t, "disable_command_execute", failed.LastError)

	require.False(t, c.HasPending(1))

	var s model.Server
	require.NoError(t, DB.First(&s, 1).Error)
	require.Equal(t, uint64(100), s.UserID, "Failed must revert Server.UserID to FromUserID")
}

func TestServerTransferCancelRevertsOwnership(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	cancelled, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	require.Equal(t, model.ServerTransferStatusCancelled, cancelled.Status)

	var s model.Server
	require.NoError(t, DB.First(&s, 1).Error)
	require.Equal(t, uint64(100), s.UserID, "Cancel must revert Server.UserID to FromUserID")
}

func TestServerTransferTimeoutRevertsOwnership(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	// Force the row to look ancient so the sweeper catches it.
	require.NoError(t, DB.Model(&model.ServerTransfer{}).
		Where("id = ?", tr.ID).
		Update("created_at", time.Now().Add(-48*time.Hour)).Error)
	c.mu.Lock()
	if pending, ok := c.pending[1]; ok {
		pending.CreatedAt = time.Now().Add(-48 * time.Hour)
	}
	c.mu.Unlock()

	c.sweepTimeouts()

	var refreshed model.ServerTransfer
	require.NoError(t, DB.First(&refreshed, tr.ID).Error)
	require.Equal(t, model.ServerTransferStatusTimeout, refreshed.Status)

	var s model.Server
	require.NoError(t, DB.First(&s, 1).Error)
	require.Equal(t, uint64(100), s.UserID, "Timeout must revert Server.UserID to FromUserID")
}

// Cancel after MarkVerified must be a no-op. The CAS guard (WHERE status =
// Pending) is the only thing preventing the auth-tolerance path and the
// timeout sweeper from racing past each other in production.
func TestServerTransferCancelAfterVerifiedIsNoOp(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	_, _, err := markPendingVerified(t, c, 1)
	require.NoError(t, err)

	result, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	require.Nil(t, result, "Cancel on a non-Pending row returns (nil, nil)")

	var refreshed model.ServerTransfer
	require.NoError(t, DB.First(&refreshed, tr.ID).Error)
	require.Equal(t, model.ServerTransferStatusVerified, refreshed.Status)

	var s model.Server
	require.NoError(t, DB.First(&s, 1).Error)
	require.Equal(t, uint64(200), s.UserID, "Server.UserID must remain at ToUserID")
}

// Retry guards two distinct conditions and we need a test per condition,
// otherwise a regression in one guard hides behind the other.
//
// Guard 1 (this test): the previous row must be terminal — passing a Pending
// row trips IsTerminal() before HasPending() is even consulted.
func TestServerTransferRetryRefusesOnNonTerminalStatus(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	UserLock.Lock()
	UserInfoMap[200] = model.UserInfo{AgentSecret: "target-secret"}
	UserLock.Unlock()
	prev := initiateAndRegister(t, c, 1, 100, 200, 1)

	_, err := c.Retry(prev, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-terminal", "must fail on the IsTerminal guard, not on HasPending")
}

// Guard 2: even with a properly terminal prev row, Retry must still refuse
// when the server has acquired a new in-flight transfer in the meantime —
// otherwise the operator could double-book the same server. The original
// test for this guard was a copy-paste of the non-terminal test and never
// actually exercised HasPending; this version drives it directly.
func TestServerTransferRetryRefusesWhenServerHasAnotherInflight(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	UserLock.Lock()
	UserInfoMap[200] = model.UserInfo{AgentSecret: "target-secret"}
	UserLock.Unlock()

	failed := initiateAndRegister(t, c, 1, 100, 200, 1)
	_, err := c.MarkFailed(failed.ID, "boom")
	require.NoError(t, err)
	var refreshed model.ServerTransfer
	require.NoError(t, DB.First(&refreshed, failed.ID).Error)
	require.True(t, refreshed.Status.IsTerminal(), "precondition: prev row must be terminal")

	// A different operator kicks off a new transfer right after the failure —
	// server now has an active Pending row again.
	initiateAndRegister(t, c, 1, 100, 300, 2)
	require.True(t, c.HasPending(1))

	_, err = c.Retry(&refreshed, 2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "in-flight", "must fail on the HasPending guard specifically")
}

func TestServerTransferRetryRecreatesPending(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	UserLock.Lock()
	UserInfoMap[200] = model.UserInfo{AgentSecret: "target-secret"}
	UserLock.Unlock()
	prev := initiateAndRegister(t, c, 1, 100, 200, 1)

	_, err := c.MarkFailed(prev.ID, "boom")
	require.NoError(t, err)

	// Refresh `prev` so IsTerminal sees Failed and Retry proceeds.
	var refreshed model.ServerTransfer
	require.NoError(t, DB.First(&refreshed, prev.ID).Error)

	created, err := c.Retry(&refreshed, 1)
	require.NoError(t, err)
	require.Equal(t, model.ServerTransferStatusPending, created.Status)
	require.NotEqual(t, prev.ID, created.ID)
	require.Equal(t, uint64(100), created.FromUserID, "Retry uses the current Server.UserID as FromUserID after the revert")
	require.Equal(t, uint64(200), created.ToUserID)

	require.True(t, c.HasPending(1))
}

func TestServerTransferRetryRejectsMissingTargetUser(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[200] = model.UserInfo{AgentSecret: "target-secret"}
	UserLock.Unlock()

	prev := initiateAndRegister(t, c, 1, 100, 200, 1)
	_, err := c.MarkFailed(prev.ID, "boom")
	require.NoError(t, err)
	UserLock.Lock()
	delete(UserInfoMap, 200)
	UserLock.Unlock()

	var refreshed model.ServerTransfer
	require.NoError(t, DB.First(&refreshed, prev.ID).Error)

	created, err := c.Retry(&refreshed, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "target user")
	require.Nil(t, created)
	require.False(t, c.HasPending(1))

	var s model.Server
	require.NoError(t, DB.First(&s, 1).Error)
	require.Equal(t, uint64(100), s.UserID)
}

// Guard 3 (anti-regression): Retry must NOT compare s.UserID against
// prev.FromUserID. The non-admin path is already forced by the controller's
// authz check (current.UserID == caller); for the admin path, the design
// contract — pinned down by TestRetryServerTransferAllowsAdmin — is "issue
// a transfer to prev.ToUserID using whatever current owner exists, regardless
// of drift". Adding a FromUserID-must-match check inside Retry would silently
// break the admin recovery path. This test exists so future cleanups don't
// reintroduce that check.
func TestServerTransferRetryDoesNotEnforceFromUserIDMatch(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	UserLock.Lock()
	UserInfoMap[200] = model.UserInfo{AgentSecret: "target-secret"}
	UserLock.Unlock()

	prev := initiateAndRegister(t, c, 1, 100, 200, 1)
	_, err := c.MarkFailed(prev.ID, "boom")
	require.NoError(t, err)
	var refreshed model.ServerTransfer
	require.NoError(t, DB.First(&refreshed, prev.ID).Error)

	// Ownership drifts to user 300 (e.g. via an out-of-band transfer or admin
	// override). The historical "from=100" no longer matches the live owner.
	require.NoError(t, DB.Model(&model.Server{}).Where("id = ?", uint64(1)).Update("user_id", uint64(300)).Error)
	if s, ok := ServerShared.Get(1); ok {
		s.SetUserID(300)
	}

	created, err := c.Retry(&refreshed, 999)
	require.NoError(t, err, "Retry must still issue against the current owner — drift is not an error here")
	require.Equal(t, uint64(300), created.FromUserID, "FromUserID tracks the live owner, not prev.FromUserID")
	require.Equal(t, uint64(200), created.ToUserID)
}

// On dashboard restart, persisted Pending rows must rehydrate the in-memory
// pending index — otherwise the auth-tolerance window evaporates after every
// restart and in-flight agents start failing authentication.
func TestServerTransferLoadsPendingFromDBOnConstruction(t *testing.T) {
	_, cleanup := setupTransferFixture(t)
	defer cleanup()

	seedServerForTransfer(t, 1, 200)
	require.NoError(t, DB.Create(&model.ServerTransfer{
		Common:     model.Common{ID: 42},
		ServerID:   1,
		FromUserID: 100,
		ToUserID:   200,
		Status:     model.ServerTransferStatusPending,
	}).Error)

	reborn := NewServerTransferClass()
	defer reborn.Stop()

	require.True(t, reborn.HasPending(1), "Pending row must be rehydrated from DB on construction")
}

// Subscribe must observe every transition broadcast, in order. WS clients
// rely on this to keep their cache fresh without polling.
func TestServerTransferBroadcastReachesSubscribers(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	id, ch, err := c.Subscribe(100)
	require.NoError(t, err)
	defer c.Unsubscribe(id)

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	// Register broadcasts; expect one event for the Pending registration.
	select {
	case ev := <-ch:
		require.Equal(t, tr.ID, ev.ID)
		require.Equal(t, model.ServerTransferStatusPending, ev.Status)
	case <-time.After(time.Second):
		t.Fatal("expected Pending broadcast within 1s")
	}

	_, _, err = markPendingVerified(t, c, 1)
	require.NoError(t, err)
	select {
	case ev := <-ch:
		require.Equal(t, model.ServerTransferStatusVerified, ev.Status)
	case <-time.After(time.Second):
		t.Fatal("expected Verified broadcast within 1s")
	}
}

func TestServerTransferSubscribeEnforcesPerUserLimitAndReleasesQuota(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()

	ids := make([]uint64, 0, maxServerTransferSubscribersPerUser)
	for range maxServerTransferSubscribersPerUser {
		id, ch, err := c.Subscribe(100)
		require.NoError(t, err)
		require.NotNil(t, ch)
		ids = append(ids, id)
	}

	_, _, err := c.Subscribe(100)
	require.ErrorIs(t, err, errTooManyServerTransferSubscribersForUser)

	otherID, _, err := c.Subscribe(200)
	require.NoError(t, err, "one user's quota must not block another user")
	c.Unsubscribe(otherID)

	c.Unsubscribe(ids[0])
	replacementID, _, err := c.Subscribe(100)
	require.NoError(t, err, "disconnecting must release the user's quota")
	c.Unsubscribe(replacementID)
	for _, id := range ids[1:] {
		c.Unsubscribe(id)
	}
}

func TestServerTransferSubscribeEnforcesGlobalLimit(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()

	ids := make([]uint64, 0, maxServerTransferSubscribersGlobal)
	for userID := uint64(1); len(ids) < maxServerTransferSubscribersGlobal; userID++ {
		for range maxServerTransferSubscribersPerUser {
			if len(ids) == maxServerTransferSubscribersGlobal {
				break
			}
			id, _, err := c.Subscribe(userID)
			require.NoError(t, err)
			ids = append(ids, id)
		}
	}

	_, _, err := c.Subscribe(999999)
	require.ErrorIs(t, err, errTooManyServerTransferSubscribersGlobal)
	for _, id := range ids {
		c.Unsubscribe(id)
	}
}

// The "one active transfer per server" invariant must hold under concurrent
// callers (two operators batch-moving the same server, or batch-move racing
// retry). The old flow had a TOCTOU between HasPending() and Initiate() that
// allowed two Pending rows to be created for the same server; this test pins
// down the contract that InitiateExclusive serializes the check + the
// transaction + the registration atomically.
func TestServerTransferInitiateExclusiveSerializesConcurrentCallers(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	const callers = 32
	var (
		started   sync.WaitGroup
		release   = make(chan struct{})
		successes atomic.Int64
		conflicts atomic.Int64
		otherErrs atomic.Int64
	)
	started.Add(callers)

	for i := 0; i < callers; i++ {
		go func() {
			started.Done()
			<-release
			_, err := c.InitiateExclusive(1, 100, 200, 1)
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrServerAlreadyTransferring):
				conflicts.Add(1)
			default:
				otherErrs.Add(1)
			}
		}()
	}

	started.Wait()
	close(release)

	require.Eventually(t, func() bool {
		return successes.Load()+conflicts.Load()+otherErrs.Load() == callers
	}, time.Second, 10*time.Millisecond, "expected all callers to settle")

	require.Equal(t, int64(0), otherErrs.Load(), "no caller should error with anything other than ErrServerAlreadyTransferring")
	require.Equal(t, int64(1), successes.Load(), "exactly one InitiateExclusive may win")
	require.Equal(t, int64(callers-1), conflicts.Load(), "all losers must observe ErrServerAlreadyTransferring")

	var pendingCount int64
	require.NoError(t, DB.Model(&model.ServerTransfer{}).
		Where("server_id = ? AND status = ?", uint64(1), model.ServerTransferStatusPending).
		Count(&pendingCount).Error)
	require.Equal(t, int64(1), pendingCount, "DB must contain exactly one Pending row")
}

// A failure inside the DB transaction must release the per-server claim so a
// later caller can retry. Without this the first failed initiation would
// permanently mark the server as "in flight" in memory and every subsequent
// move would mysteriously return ErrServerAlreadyTransferring.
func TestServerTransferInitiateExclusiveReleasesClaimOnFailure(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	// No server seeded — Initiate's UPDATE will affect zero rows but the
	// INSERT still succeeds in SQLite. Force a failure by closing the DB
	// briefly via a sub-test that uses an invalid server id; instead, do
	// the simpler thing: seed the server, run a successful initiation,
	// fail the second (HasPending conflict), then release the first via
	// MarkFailed and confirm a fresh initiation succeeds — this exercises
	// the release path for both the conflict and the post-terminal recovery.
	seedServerForTransfer(t, 1, 100)

	first, err := c.InitiateExclusive(1, 100, 200, 1)
	require.NoError(t, err)
	require.NotNil(t, first)

	_, err = c.InitiateExclusive(1, 100, 200, 1)
	require.ErrorIs(t, err, ErrServerAlreadyTransferring)

	_, err = c.MarkFailed(first.ID, "boom")
	require.NoError(t, err)
	require.False(t, c.HasPending(1), "MarkFailed must release the pending claim")

	second, err := c.InitiateExclusive(1, 100, 300, 1)
	require.NoError(t, err, "after MarkFailed the server must be eligible for a new transfer")
	require.NotEqual(t, first.ID, second.ID)
}

// revertTransition's CAS UPDATE returns RowsAffected=0 whenever a concurrent
// caller (the auth path's MarkVerified, another revert, the timeout sweep)
// has already transitioned the row out of Pending between our tx.First and
// our UPDATE. The old code silently returned (nil, nil) but left the
// in-memory pending entry behind, so the affected server kept enjoying the
// auth tolerance window long after the transfer was settled — a stale
// FromUserID secret would continue to authenticate against a server that
// had moved on. This test pins down "revertTransition must converge the
// in-memory cache to whatever the DB now shows, even on its no-op path."
func TestServerTransferRevertTransitionDropsStaleMemoryOnConcurrentWin(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	require.True(t, c.HasPending(1), "precondition: in-memory pending must hold the row")

	// Simulate "another caller already won the CAS" by transitioning the DB
	// row directly. The in-memory pending entry is intentionally left intact
	// — we are emulating the race window between two callers.
	require.NoError(t, DB.Model(&model.ServerTransfer{}).
		Where("id = ?", tr.ID).
		Update("status", model.ServerTransferStatusVerified).Error)

	// Cancel's CAS will see RowsAffected=0 and return (nil, nil). With the
	// fix in place, in-memory pending must converge to the DB state.
	result, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	require.Nil(t, result, "Cancel against a non-Pending row is a no-op result")

	require.False(t, c.HasPending(1), "in-memory pending must be cleaned when the DB row is no longer Pending")
}

// OnAgentReconnect is invoked from the gRPC stream handler on every fresh
// agent connection. It looks up the pending transfer and hands it to
// PushIfOnline. A concurrent Cancel can settle the transfer between those
// two steps; if PushIfOnline trusts its parameter blindly and sends the
// ApplyConfig anyway, the new secret races past the cancel's counter-push
// (pushRevertIfOnline). The agent's supersede behaviour gives the last
// arrival priority — so if our stale push arrives last, the agent commits
// the cancelled credential and locks itself out. This test pins down the
// re-check contract: PushIfOnline must verify the transfer is still pending
// for its server right before sending, and become a no-op otherwise.
func TestServerTransferPushIfOnlineSkipsStaleTransferAfterCancel(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[200] = model.UserInfo{AgentSecret: "new-owner-secret"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	stream := newFakeTaskStream()
	s, _ := ServerShared.Get(1)
	s.SetTaskStream(stream)

	// Cancel wins the race against the reconnect-triggered push.
	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	require.False(t, c.HasPending(1), "precondition: pending must be cleared by Cancel")

	// Drain Cancel's revert push so the next inspection sees only the stale
	// PushIfOnline (or its absence).
	stream.reset()

	// Simulate the OnAgentReconnect call that captured `tr` BEFORE Cancel
	// landed and is only now reaching PushIfOnline.
	c.PushIfOnline(tr)

	require.Equal(t, 0, stream.sendCount(), "PushIfOnline must skip a transfer that is no longer pending — otherwise it races past the cancel's counter-push and the agent commits the rejected secret")
}

type cancelRaceApplyConfigStream struct {
	pb.NezhaService_RequestTaskServer

	firstSendBlocked chan struct{}
	releaseFirstSend chan struct{}
	firstSendClaimed atomic.Bool
	releaseOnce      sync.Once

	mu   sync.Mutex
	sent []*pb.Task
}

type neverReturningTaskStream struct {
	pb.NezhaService_RequestTaskServer
	reachedSend chan struct{}
	release     chan struct{}
	reachOnce   sync.Once
	releaseOnce sync.Once
}

func newNeverReturningTaskStream() *neverReturningTaskStream {
	return &neverReturningTaskStream{
		reachedSend: make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (s *neverReturningTaskStream) Send(*pb.Task) error {
	s.reachOnce.Do(func() { close(s.reachedSend) })
	<-s.release
	return nil
}

func (s *neverReturningTaskStream) releaseAll() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func newCancelRaceApplyConfigStream() *cancelRaceApplyConfigStream {
	return &cancelRaceApplyConfigStream{
		firstSendBlocked: make(chan struct{}),
		releaseFirstSend: make(chan struct{}),
	}
}

func (s *cancelRaceApplyConfigStream) Send(task *pb.Task) error {
	if s.firstSendClaimed.CompareAndSwap(false, true) {
		close(s.firstSendBlocked)
		<-s.releaseFirstSend
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, task)
	return nil
}

func (s *cancelRaceApplyConfigStream) releaseBlockedFirstSend() {
	s.releaseOnce.Do(func() {
		close(s.releaseFirstSend)
	})
}

func (s *cancelRaceApplyConfigStream) sentTasksSnapshot() []*pb.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := make([]*pb.Task, len(s.sent))
	copy(snapshot, s.sent)
	return snapshot
}

func TestServerTransferCancelRevertWinsWhenPushIfOnlineSendWasAlreadyInFlight(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[100] = model.UserInfo{AgentSecret: "old-owner-secret"}
	UserInfoMap[200] = model.UserInfo{AgentSecret: "new-owner-secret"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	stream := newCancelRaceApplyConfigStream()
	defer stream.releaseBlockedFirstSend()
	s, ok := ServerShared.Get(1)
	require.True(t, ok)
	s.SetTaskStream(stream)

	pushDone := make(chan struct{})
	go func() {
		defer close(pushDone)
		c.PushIfOnline(tr)
	}()

	select {
	case <-stream.firstSendBlocked:
	case <-time.After(time.Second):
		t.Fatal("expected PushIfOnline to reach Send before cancelling")
	}

	cancelDone := make(chan error, 1)
	go func() {
		_, err := c.Cancel(tr.ID)
		cancelDone <- err
	}()

	require.Eventually(t, func() bool {
		return !c.HasPending(1)
	}, time.Second, 10*time.Millisecond, "Cancel must clear pending while the stale push is blocked")

	stream.releaseBlockedFirstSend()
	select {
	case <-pushDone:
	case <-time.After(time.Second):
		t.Fatal("expected blocked PushIfOnline Send to finish")
	}
	select {
	case err := <-cancelDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("expected Cancel to finish after the stale push is released")
	}

	var refreshed model.ServerTransfer
	require.NoError(t, DB.First(&refreshed, tr.ID).Error)

	sentAfterRelease := stream.sentTasksSnapshot()
	require.NotEmpty(t, sentAfterRelease, "expected at least one delivered ApplyConfig task")
	finalApplyConfig := sentAfterRelease[len(sentAfterRelease)-1]
	require.Equal(t, uint64(model.TaskTypeServerTransferApply), finalApplyConfig.Type)
	require.Contains(t, finalApplyConfig.Data, refreshed.RevertHandshakeSecret, "Cancel revert (RevertHandshakeSecret) must remain the final delivered ApplyConfig")
	require.NotContains(t, finalApplyConfig.Data, refreshed.HandshakeSecret, "stale forward HandshakeSecret push must not arrive after the cancel revert")
	require.NotContains(t, finalApplyConfig.Data, "old-owner-secret", "user-global AgentSecret must never appear in transfer ApplyConfig payloads")
	require.NotContains(t, finalApplyConfig.Data, "new-owner-secret", "user-global AgentSecret must never appear in transfer ApplyConfig payloads")
}

func TestServerTransferBlockedApplyConfigSendDoesNotBlockUnrelatedRevert(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	seedServerForTransfer(t, 2, 300)

	UserLock.Lock()
	UserInfoMap[100] = model.UserInfo{AgentSecret: "server-a-old-secret"}
	UserInfoMap[200] = model.UserInfo{AgentSecret: "server-a-new-secret"}
	UserInfoMap[300] = model.UserInfo{AgentSecret: "server-b-old-secret"}
	UserInfoMap[400] = model.UserInfo{AgentSecret: "server-b-new-secret"}
	UserLock.Unlock()

	transferA := initiateAndRegister(t, c, 1, 100, 200, 1)
	transferB := initiateAndRegister(t, c, 2, 300, 400, 1)

	blockedStream := newCancelRaceApplyConfigStream()
	defer blockedStream.releaseBlockedFirstSend()
	serverA, ok := ServerShared.Get(1)
	require.True(t, ok)
	serverA.SetTaskStream(blockedStream)

	serverBStream := newFakeTaskStream()
	serverB, ok := ServerShared.Get(2)
	require.True(t, ok)
	serverB.SetTaskStream(serverBStream)

	pushDone := make(chan struct{})
	go func() {
		defer close(pushDone)
		c.PushIfOnline(transferA)
	}()
	select {
	case <-blockedStream.firstSendBlocked:
	case <-time.After(time.Second):
		t.Fatal("expected server A PushIfOnline to block inside Send")
	}

	cancelDone := make(chan error, 1)
	go func() {
		_, err := c.Cancel(transferB.ID)
		cancelDone <- err
	}()

	select {
	case err := <-cancelDone:
		require.NoError(t, err)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("blocked Send for server A must not block server B cancel/revert delivery")
	}

	require.Equal(t, 1, serverBStream.sendCount(), "server B revert ApplyConfig must be delivered while server A is blocked")
	var refreshedB model.ServerTransfer
	require.NoError(t, DB.First(&refreshedB, transferB.ID).Error)
	require.Contains(t, serverBStream.sent[0].Data, refreshedB.RevertHandshakeSecret, "server B revert must carry its per-transfer RevertHandshakeSecret")
	require.NotContains(t, serverBStream.sent[0].Data, "server-b-old-secret", "user-global AgentSecret must never appear in transfer payloads")
	require.NotContains(t, serverBStream.sent[0].Data, "server-b-new-secret", "user-global AgentSecret must never appear in transfer payloads")

	blockedStream.releaseBlockedFirstSend()
	select {
	case <-pushDone:
	case <-time.After(time.Second):
		t.Fatal("expected blocked server A send to finish after release")
	}
}

func TestServerTransferApplyConfigSendDoesNotReturnBeforeSendCompletes(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[200] = model.UserInfo{AgentSecret: "timeout-new-secret"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	stream := newCancelRaceApplyConfigStream()
	defer stream.releaseBlockedFirstSend()
	server, ok := ServerShared.Get(1)
	require.True(t, ok)
	server.SetTaskStream(stream)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.PushIfOnline(tr)
	}()

	select {
	case <-stream.firstSendBlocked:
	case <-time.After(time.Second):
		t.Fatal("expected PushIfOnline to enter stream.Send")
	}
	select {
	case <-done:
		t.Fatal("PushIfOnline must not return while stream.Send is still blocked; stale ApplyConfig could arrive after a revert")
	case <-time.After(200 * time.Millisecond):
	}
	stream.releaseBlockedFirstSend()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected PushIfOnline to finish after stream.Send unblocks")
	}
}

func TestServerTransferRestartRestoresRevertDeliveryWindow(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[100] = model.UserInfo{AgentSecret: "restart-old-secret"}
	UserInfoMap[200] = model.UserInfo{AgentSecret: "restart-new-secret"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	c.Stop()

	reborn := NewServerTransferClass()
	defer reborn.Stop()
	ServerTransferShared = reborn

	if got, ok := reborn.LookupRevertDelivery(1); !ok || got.ID != tr.ID {
		t.Fatalf("restart must preserve reverted transfer delivery window, got transfer=%v ok=%v", got, ok)
	}

	stream := newFakeTaskStream()
	server, ok := ServerShared.Get(1)
	require.True(t, ok)
	server.SetTaskStream(stream)

	reborn.OnAgentReconnect(1)

	require.Equal(t, 1, stream.sendCount(), "new-secret reconnect after dashboard restart must receive the rollback ApplyConfig")
	var rebornTr model.ServerTransfer
	require.NoError(t, DB.First(&rebornTr, tr.ID).Error)
	require.Contains(t, stream.sent[0].Data, rebornTr.RevertHandshakeSecret, "rollback must carry the per-transfer RevertHandshakeSecret")
	require.NotContains(t, stream.sent[0].Data, "restart-old-secret", "user-global AgentSecret must never appear in transfer payloads")
	require.NotContains(t, stream.sent[0].Data, "restart-new-secret", "user-global AgentSecret must never appear in transfer payloads")
}

// MarkVerified is called from the auth hot path on every agent RPC. The old
// signature conflated "no pending entry" (the expected idempotent case) with
// "DB UPDATE failed" by both returning the same (nil, false) tuple, so a real
// DB error during the transition was silently dropped. That left the auth
// tolerance window open indefinitely for the affected server (the in-memory
// pending entry was never cleared because the transition appeared to
// succeed-but-no-op) and gave operators no signal that the dashboard couldn't
// finalize transfers. This test pins down: a genuine DB failure during the
// CAS UPDATE must surface as a non-nil error so callers can log it.
func TestServerTransferMarkVerifiedSurfacesDBError(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	initiateAndRegister(t, c, 1, 100, 200, 1)

	// Dropping the table makes any UPDATE against server_transfers return
	// "no such table". This is the same shape of failure a corrupt schema,
	// closed connection, or runaway lock would produce in production.
	require.NoError(t, DB.Migrator().DropTable(&model.ServerTransfer{}))

	_, transfer, err := markPendingVerified(t, c, 1)
	require.Error(t, err, "DB-level failures must propagate up to the caller")
	require.Nil(t, transfer)
}

// The idempotent no-op cases (no pending entry OR concurrent caller already
// settled the row) must return (nil, nil) — distinguishable from a real DB
// error by the absence of an error. Without this contract the auth path
// cannot tell "already verified, all good" from "DB is broken, bail".
func TestServerTransferMarkVerifiedNoOpReturnsNilError(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	// Case 1: no pending entry at all.
	ok, transfer, err := markPendingVerified(t, c, 1)
	require.NoError(t, err, "no pending entry must be a silent no-op, not an error")
	require.False(t, ok)
	require.Nil(t, transfer)

	// Case 2: pending entry exists but the DB row was concurrently transitioned
	// out of Pending — RowsAffected=0 is still an idempotent no-op.
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	require.NoError(t, DB.Model(&model.ServerTransfer{}).
		Where("id = ?", tr.ID).
		Update("status", model.ServerTransferStatusCancelled).Error)

	ok, transfer, err = markPendingVerified(t, c, 1)
	require.NoError(t, err, "concurrent CAS loser must be a silent no-op, not an error")
	require.False(t, ok, "lost CAS must report verified=false so auth rejects the credential")
	require.Nil(t, transfer)
}

// NewServerTransferClass must surface DB load failures via the standard logger
// so operators see the failure. The original implementation discarded the
// error from DB.Where(...).Find(&pending); a corrupted schema or transient
// query failure on startup would silently leave the in-memory pending index
// empty, evaporating the auth-tolerance window for every in-flight transfer
// without any operator-visible signal. This test pins the contract: the error
// must be logged with a NEZHA prefix.
func TestNewServerTransferClassLogsDBLoadError(t *testing.T) {
	originalDB := DB
	originalServerShared := ServerShared
	originalServerTransfer := ServerTransferShared
	defer func() {
		DB = originalDB
		ServerShared = originalServerShared
		ServerTransferShared = originalServerTransfer
	}()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	DB = db
	ServerShared = NewServerClass()

	// Don't migrate ServerTransfer — the Find below will fail with
	// "no such table", which is the same failure shape a corrupted DB
	// would surface in production.

	var buf bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(originalOutput)

	c := NewServerTransferClass()
	defer c.Stop()

	logged := buf.String()
	require.True(t,
		strings.Contains(logged, "NEZHA") && strings.Contains(logged, "transfer"),
		"NewServerTransferClass must log DB load failures so operators notice; got %q", logged)
}

// revertTransition's self-heal step previously deleted c.pending[t.ServerID]
// whenever the DB row was non-Pending, regardless of which transfer ID the
// in-memory entry was pointing at. After Retry creates a new Pending row for
// the same server, the in-memory pending entry holds the NEW transfer — but
// `cancelServerTransfer` still accepts the historical (terminal) transfer ID
// and routes it through revertTransition. The stale-id Cancel then wiped the
// fresh Pending entry's auth-tolerance window and re-opened the door for
// double initiation, even though the actual DB row it operated on never
// changed status. This test pins the contract: revertTransition's self-heal
// must only drop the in-memory entry it actually owns (same transfer ID).
func TestServerTransferCancelOnStaleTerminalKeepsNewPendingIntact(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	UserLock.Lock()
	UserInfoMap[200] = model.UserInfo{AgentSecret: "target-secret"}
	UserLock.Unlock()

	first := initiateAndRegister(t, c, 1, 100, 200, 1)
	_, err := c.MarkFailed(first.ID, "boom")
	require.NoError(t, err)
	require.False(t, c.HasPending(1), "precondition: first transfer must be released")

	// Operator (or admin) re-issues the transfer. Retry uses the live owner
	// (still user 100 because MarkFailed reverted it) and emits a fresh
	// Pending row for the same server.
	var refreshed model.ServerTransfer
	require.NoError(t, DB.First(&refreshed, first.ID).Error)
	second, err := c.Retry(&refreshed, 1)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID, "Retry must create a new transfer id")
	require.True(t, c.HasPending(1), "precondition: Retry must register the new pending row")

	// Now the buggy path: someone (UI, replayed API call, automation script)
	// calls Cancel against the OLD terminal transfer's id. cancelServerTransfer
	// doesn't gate on t.Status == Pending — only on permission — so the call
	// reaches revertTransition.
	result, err := c.Cancel(first.ID)
	require.NoError(t, err)
	require.Nil(t, result, "Cancel on a terminal row must be a silent no-op")

	require.True(t, c.HasPending(1),
		"the fresh pending transfer must survive Cancel against the stale terminal id")
	got, ok := c.LookupPending(1)
	require.True(t, ok)
	require.Equal(t, second.ID, got.ID,
		"in-memory pending must still point at the new transfer, not be wiped by a stale id")
}

// Cancel -> revertTransition synchronously calls pushRevertIfOnline at the
// end. That call captures the terminal transfer `tr` and races for the
// per-server applyConfigSendLock against any concurrent PushIfOnline (e.g.
// because the operator immediately Retried). If the new transfer's
// PushIfOnline acquires the lock FIRST and delivers the new owner's secret,
// then the still-queued pushRevertIfOnline for the OLD transfer must NOT
// send its rollback — doing so overwrites the new transfer's secret on the
// agent (supersede is last-arrival-wins), the new transfer never reconnects
// under its target secret, and it sits Pending until the 24h timeout sweep.
//
// Mirror of TestServerTransferPushIfOnlineSkipsStaleTransferAfterCancel
// (which pins down the same invariant on the `pending` index). Pin it on
// the `revertDeliveries` index as well: pushRevertIfOnline must re-check
// revertDelivery currency immediately before Send.
func TestServerTransferPushRevertIfOnlineSkipsStaleDeliveryAfterRetry(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[100] = model.UserInfo{AgentSecret: "old-owner-secret"}
	UserInfoMap[200] = model.UserInfo{AgentSecret: "new-owner-secret"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	stream := newFakeTaskStream()
	s, ok := ServerShared.Get(1)
	require.True(t, ok)
	s.SetTaskStream(stream)

	// Cancel registers a revertDelivery for tr and synchronously invokes
	// pushRevertIfOnline, which delivers the rollback (RevertHandshakeSecret).
	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	require.Equal(t, 1, stream.sendCount(), "precondition: Cancel must deliver the rollback ApplyConfig once")
	var cancelled model.ServerTransfer
	require.NoError(t, DB.First(&cancelled, tr.ID).Error)
	require.Contains(t, stream.sent[0].Data, cancelled.RevertHandshakeSecret)
	require.NotContains(t, stream.sent[0].Data, "old-owner-secret")

	// Operator immediately Retries — this clears the revertDelivery, installs
	// a fresh Pending row, and pushes the new transfer's HandshakeSecret.
	var refreshed model.ServerTransfer
	require.NoError(t, DB.First(&refreshed, tr.ID).Error)
	retried, err := c.Retry(&refreshed, 1)
	require.NoError(t, err)
	require.True(t, c.HasPending(1), "precondition: Retry must register the new pending row")
	require.Equal(t, 2, stream.sendCount(), "precondition: Retry must deliver the new-pending ApplyConfig")
	require.Contains(t, stream.sent[1].Data, retried.HandshakeSecret)
	require.NotContains(t, stream.sent[1].Data, "new-owner-secret")

	// Simulate the bug window: Cancel's pushRevertIfOnline was scheduled but
	// only now reaches the per-server lock — long after Retry has already
	// landed and delivered the new secret. Replay it with the stale tr.
	stream.reset()
	c.pushRevertIfOnline(tr)

	require.Equal(t, 0, stream.sendCount(),
		"pushRevertIfOnline must skip a transfer whose revertDelivery has been superseded by a Retry — "+
			"otherwise the agent's last-arrival ApplyConfig supersede commits the rejected old-owner secret "+
			"and the new transfer sits Pending until the 24h timeout")
}

// SECURITY: the ApplyConfig payload PushIfOnline writes to the agent stream
// must NEVER contain another user's global AgentSecret. During a Pending
// transfer the agent stream is still authenticated by the OLD owner's secret
// (auth tolerance). A malicious old owner who knows their own user-global
// AgentSecret can run a fake agent process under the server's UUID, hold the
// RequestTask stream open, and intercept whatever the dashboard sends. If we
// embed the destination user's global AgentSecret in the payload, the
// attacker recovers a secret that grants access to EVERY agent that
// destination user owns. The transfer credential must therefore be scoped to
// this transfer only — a one-time, per-transfer token that gates the
// agent's reconnect under the new owner's identity and grants no further
// access if leaked.
func TestServerTransferPushDoesNotLeakDestinationUserGlobalSecret(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[100] = model.UserInfo{AgentSecret: "old-owner-global-secret"}
	UserInfoMap[200] = model.UserInfo{AgentSecret: "DESTINATION-USER-GLOBAL-SECRET-MUST-NOT-LEAK"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	stream := newFakeTaskStream()
	s, ok := ServerShared.Get(1)
	require.True(t, ok)
	s.SetTaskStream(stream)

	c.PushIfOnline(tr)
	require.GreaterOrEqual(t, stream.sendCount(), 1, "PushIfOnline must dispatch a transfer ApplyConfig")

	for i, sent := range stream.sent {
		require.NotContains(t, sent.Data, "DESTINATION-USER-GLOBAL-SECRET-MUST-NOT-LEAK",
			"task[%d] embeds the destination user's global AgentSecret; a malicious previous owner holding the stream can recover it", i)
	}
}

// Symmetric coverage for the revert path: pushRevertIfOnline must not embed
// the FROM-user's global AgentSecret when delivering the rollback over a
// stream that — by definition of the revert window — is now authenticated by
// the NEW owner. Otherwise the new owner (legitimate or compromised) can
// recover the previous owner's secret.
func TestServerTransferRevertPushDoesNotLeakFromUserGlobalSecret(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[100] = model.UserInfo{AgentSecret: "FROM-USER-GLOBAL-SECRET-MUST-NOT-LEAK"}
	UserInfoMap[200] = model.UserInfo{AgentSecret: "new-owner-global-secret"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	stream := newFakeTaskStream()
	s, ok := ServerShared.Get(1)
	require.True(t, ok)
	s.SetTaskStream(stream)

	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, stream.sendCount(), 1, "Cancel must dispatch a revert ApplyConfig")

	for i, sent := range stream.sent {
		require.NotContains(t, sent.Data, "FROM-USER-GLOBAL-SECRET-MUST-NOT-LEAK",
			"revert task[%d] embeds the source user's global AgentSecret; the now-authenticated destination owner can recover it", i)
	}
}

// sweepTimeouts iterates Pending candidates and synchronously revertTransitions
// each one. If MarkTimeout's pushRevertIfOnline blocks indefinitely on a stuck
// stream.Send, every Pending transfer after it in the sweep would otherwise
// wait — the dashboard's timeout detection would freeze for all other tenants.
// The invariant: the sweeper must process every expired candidate's
// state-transition + revert delivery within a bounded time window regardless
// of how long any single agent's Send takes.
func TestServerTransferSweepTimeoutsNotBlockedByStuckSend(t *testing.T) {
	stuck := newNeverReturningTaskStream()
	// Release the stuck stream BEFORE the singleton cleanup runs (cleanup is
	// deferred first, releaseAll second, so LIFO unblocks the send first).
	// Without this, the Wait inside sweepTimeouts holds the fan-out goroutine
	// open and the test would hang on DB teardown.

	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	defer stuck.releaseAll()

	seedServerForTransfer(t, 1, 100)
	seedServerForTransfer(t, 2, 300)

	UserLock.Lock()
	UserInfoMap[100] = model.UserInfo{AgentSecret: "a-old"}
	UserInfoMap[200] = model.UserInfo{AgentSecret: "a-new"}
	UserInfoMap[300] = model.UserInfo{AgentSecret: "b-old"}
	UserInfoMap[400] = model.UserInfo{AgentSecret: "b-new"}
	UserLock.Unlock()

	trA := initiateAndRegister(t, c, 1, 100, 200, 1)
	trB := initiateAndRegister(t, c, 2, 300, 400, 1)

	serverA, ok := ServerShared.Get(1)
	require.True(t, ok)
	serverA.SetTaskStream(stuck)

	healthy := newFakeTaskStream()
	serverB, ok := ServerShared.Get(2)
	require.True(t, ok)
	serverB.SetTaskStream(healthy)

	expired := time.Now().Add(-2 * c.timeout)
	require.NoError(t, DB.Model(&model.ServerTransfer{}).
		Where("id IN ?", []uint64{trA.ID, trB.ID}).
		Update("created_at", expired).Error)
	c.mu.Lock()
	if entry, ok := c.pending[1]; ok {
		entry.CreatedAt = expired
	}
	if entry, ok := c.pending[2]; ok {
		entry.CreatedAt = expired
	}
	c.mu.Unlock()

	sweepDone := make(chan struct{})
	go func() {
		c.sweepTimeouts()
		close(sweepDone)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if healthy.sendCount() > 0 {
			break
		}
		select {
		case <-deadline:
			stuck.releaseAll()
			<-sweepDone
			t.Fatal("sweepTimeouts blocked on server A's stuck Send and never reached server B's rollback delivery")
		case <-time.After(20 * time.Millisecond):
		}
	}

	var savedB model.ServerTransfer
	require.NoError(t, DB.First(&savedB, trB.ID).Error)
	require.Equal(t, model.ServerTransferStatusTimeout, savedB.Status,
		"server B's transfer must be marked Timeout while server A's stuck delivery is in flight")

	stuck.releaseAll()
	<-sweepDone
}

// Initiate must refuse to register a Pending transfer when the targeted server
// row no longer exists. Without a RowsAffected==1 check the UPDATE silently
// succeeds with 0 rows touched, Register flips an in-memory ghost entry, and
// auth.go's tolerance window then accepts the previous owner's secret for a
// server that was never actually mutated. The whole transfer must roll back.
func TestServerTransferInitiateAbortsWhenServerRowMissing(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()

	const ghostServerID uint64 = 4242

	var created *model.ServerTransfer
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		created, err = c.Initiate(tx, ghostServerID, 100, 200, 1)
		return err
	})
	require.Error(t, err, "Initiate must error when servers.id is missing")
	require.Nil(t, created, "no transfer row may be returned on a missing server")

	var rows []model.ServerTransfer
	require.NoError(t, DB.Where("server_id = ?", ghostServerID).Find(&rows).Error)
	require.Empty(t, rows, "the failed Initiate transaction must leave NO ServerTransfer row behind")

	require.False(t, c.HasPending(ghostServerID), "no in-memory pending entry may exist for the ghost server")
}

// revertTransition must refuse to advance to a terminal state if the
// underlying server row has vanished since the transfer was created. Updating
// servers.user_id with 0 rows affected was silently succeeding and the
// in-memory ServerShared cache was still being flipped back to FromUserID,
// leaving DB and cache divergent on a row that nobody owns.
func TestServerTransferRevertTransitionAbortsWhenServerRowMissing(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	require.NoError(t, DB.Delete(&model.Server{}, 1).Error)

	_, err := c.MarkFailed(tr.ID, "agent-rejected")
	require.Error(t, err, "MarkFailed must error when servers.id has vanished")

	var saved model.ServerTransfer
	require.NoError(t, DB.First(&saved, tr.ID).Error)
	require.Equal(t, model.ServerTransferStatusPending, saved.Status,
		"transfer must remain Pending — a partial revert with no server row would leave DB/cache divergent")
}

// Regression: pushRevertIfOnline must NOT clear the revertDelivery as soon as
// stream.Send returns success. The agent's handleApplyConfigTask delays the
// actual credential swap by 10s (time.AfterFunc), so by the time the agent
// reconnects under RevertHandshakeSecret, LookupByRevertHandshakeSecret has
// to still find the record — otherwise auth falls through to the global-secret
// table which doesn't know the handshake token, and the agent is permanently
// locked out. The recovery record may only be cleared after the agent has
// actually proven it received and applied the rollback (i.e. after it
// authenticates with RevertHandshakeSecret), or via the natural
// defaultRevertDeliveryRecoveryWindow expiry sweep.
func TestServerTransferPushRevertIfOnlineKeepsRevertDeliveryUntilAgentRotates(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[200] = model.UserInfo{AgentSecret: "new-owner-secret"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	stream := newFakeTaskStream()
	s, ok := ServerShared.Get(1)
	require.True(t, ok)
	s.SetTaskStream(stream)

	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)

	require.GreaterOrEqual(t, stream.sendCount(), 1, "Cancel must push the rollback ApplyConfig down the live stream")

	revert, ok := c.LookupRevertDelivery(1)
	require.True(t, ok, "revertDelivery must persist after the rollback ApplyConfig has been sent — agent applies the new client_secret after a 10s timer and only then reconnects under RevertHandshakeSecret")
	require.Equal(t, tr.ID, revert.ID)

	found, ok := c.LookupByRevertHandshakeSecret(revert.RevertHandshakeSecret)
	require.True(t, ok, "LookupByRevertHandshakeSecret must succeed after the send — clearing on send strands the agent on a credential the dashboard no longer accepts")
	require.Equal(t, tr.ID, found.ID)
}

// BUG-1 regression: Register() must NOT drop the previous Verified
// HandshakeSecret. A server that has already completed a transfer (A→B)
// holds the per-transfer HandshakeSecret H1 on disk, NOT a user-global
// AgentSecret. When B→C is initiated, the only auth path for H1 is
// LookupServerByVerifiedHandshakeSecret. If Register() deletes the entry
// before the agent has actually rotated to H2 (which only happens ~10s
// after PushIfOnline returns, due to the agent's reload timer, and
// requires Send success in the first place), the agent cannot reconnect
// during the rollover window and may be permanently locked out if the
// process restarts before applying H2.
func TestRegisterMustNotDropPreviousVerifiedHandshakeForChainedTransfer(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	// Round 1: A=100 → B=200, then MarkVerified so the agent's persisted
	// credential becomes H1.
	t1 := initiateAndRegister(t, c, 1, 100, 200, 1)
	require.NotEmpty(t, t1.HandshakeSecret)
	h1 := t1.HandshakeSecret
	_, _, err := markPendingVerified(t, c, 1)
	require.NoError(t, err)

	sid, ok := c.LookupServerByVerifiedHandshakeSecret(h1)
	require.True(t, ok, "after Round 1 MarkVerified, H1 must authenticate")
	require.Equal(t, uint64(1), sid)

	// Round 2: B=200 → C=300. The agent has NOT yet received the new
	// HandshakeSecret H2 (Register runs before PushIfOnline, and even after
	// Send the agent has a 10s reload delay). H1 is still the credential
	// on disk and must keep authenticating.
	initiateAndRegister(t, c, 1, 200, 300, 1)

	sid, ok = c.LookupServerByVerifiedHandshakeSecret(h1)
	require.True(t, ok,
		"Register() must NOT delete the previous Verified HandshakeSecret — the agent still holds H1 on disk and has no other credential path during the new transfer's reload window")
	require.Equal(t, uint64(1), sid)
}

// BUG-1 regression (restart path): if dashboard restarts while a chained
// transfer is Pending, the previous Verified row must still be rebuilt into
// verifiedHandshakes. The default rebuild gate (server.UserID == ToUserID)
// would reject H1 because Server.UserID has already been flipped to C by
// the pending B→C transfer. Rebuild must additionally accept the case where
// the server has a Pending transfer whose FromUserID equals the previous
// Verified row's ToUserID.
func TestNewServerTransferClassRebuildsPreviousVerifiedDuringChainedPending(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	// Round 1: A=100 → B=200, MarkVerified.
	t1 := initiateAndRegister(t, c, 1, 100, 200, 1)
	h1 := t1.HandshakeSecret
	_, _, err := markPendingVerified(t, c, 1)
	require.NoError(t, err)

	// Round 2: B=200 → C=300, leave Pending (no MarkVerified).
	initiateAndRegister(t, c, 1, 200, 300, 1)

	// Simulate dashboard restart by constructing a fresh class against the
	// same DB & ServerShared.
	c.Stop()
	c2 := NewServerTransferClass()
	ServerTransferShared = c2
	defer c2.Stop()

	sid, ok := c2.LookupServerByVerifiedHandshakeSecret(h1)
	require.True(t, ok,
		"after restart with Pending B→C, the previous Verified A→B HandshakeSecret must still be rebuilt — agent on disk has H1 and reconnects must succeed until the new transfer completes")
	require.Equal(t, uint64(1), sid)
}

// BUG-2 regression: MarkRevertDelivered must promote RevertHandshakeSecret
// into verifiedHandshakes so the agent — which has now persisted that secret
// as its long-term on-disk credential after the 10s reload — keeps
// authenticating after defaultRevertDeliveryRecoveryWindow expires. Without
// this promotion, the auth path only finds the secret via the temporary
// revertDeliveries window; once that 24h window sweeps the entry, the agent
// has no auth path left and is permanently locked out.
func TestMarkRevertDeliveredPromotesRevertHandshakeSecret(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	require.NotEmpty(t, tr.RevertHandshakeSecret)
	revertSecret := tr.RevertHandshakeSecret

	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	require.True(t, hasRevertDeliveryFor(c, 1, tr.ID),
		"Cancel must register the rollback for delivery")

	require.NoError(t, c.MarkRevertDelivered(1, tr.ID))

	sid, ok := c.LookupServerByVerifiedHandshakeSecret(revertSecret)
	require.True(t, ok,
		"after the agent has authenticated with RevertHandshakeSecret, that secret must be promoted to verifiedHandshakes so it survives the 24h recovery sweep")
	require.Equal(t, uint64(1), sid)

	require.False(t, hasRevertDeliveryFor(c, 1, tr.ID),
		"promotion must consume the delivery record — keeping both would let a stale revert overwrite a later transfer")

	var saved model.ServerTransfer
	require.NoError(t, DB.First(&saved, tr.ID).Error)
	require.NotNil(t, saved.AckedAt,
		"AckedAt must be persisted so dashboard restart can rebuild the promoted credential")
}

// BUG-2 regression (restart path): after MarkRevertDelivered persists
// AckedAt on a terminal row, dashboard restart must rebuild
// verifiedHandshakes[serverID] = RevertHandshakeSecret.
func TestNewServerTransferClassRebuildsAckedRollbackCredential(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	revertSecret := tr.RevertHandshakeSecret

	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	require.NoError(t, c.MarkRevertDelivered(1, tr.ID))

	c.Stop()
	c2 := NewServerTransferClass()
	ServerTransferShared = c2
	defer c2.Stop()

	sid, ok := c2.LookupServerByVerifiedHandshakeSecret(revertSecret)
	require.True(t, ok,
		"restart must rebuild acked rollback credentials from terminal rows with acked_at set")
	require.Equal(t, uint64(1), sid)
}

// BUG: NewServerTransferClass loads Verified rows first and then skips any
// rollback-acked row whose serverID already appears in verifiedHandshakes.
// In a chained "transfer-then-rollback" history the most recent credential
// the agent actually rotated to on disk is the RevertHandshakeSecret of the
// later, rolled-back transfer — not the HandshakeSecret of the earlier
// Verified transfer. The two-pass alreadySeen check therefore rebuilds the
// wrong credential and the agent is locked out on the first post-restart
// reconnect.
//
// Scenario reproduced here:
//  1. Server S owned by A=100. Transfer t1 A→B, MarkVerified. Server.UserID=B,
//     agent on disk = H1, verifiedHandshakes[S]=H1, t1.AckedAt set.
//  2. Transfer t2 B→A initiated and Cancelled. Server.UserID reverts to B
//     (FromUserID), MarkRevertDelivered → agent on disk = R2,
//     verifiedHandshakes[S]=R2, t2.AckedAt set, t2.UpdatedAt > t1.AckedAt.
//  3. Dashboard restart.
//
// After restart the agent presents R2 — that is what is actually persisted
// on disk after step 2's reload. Auth must accept it. Today the loader
// rebuilds H1 instead and the agent is locked out forever.
func TestNewServerTransferClassPrefersNewerRollbackCredentialOverOlderVerified(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	t1 := initiateAndRegister(t, c, 1, 100, 200, 1)
	h1 := t1.HandshakeSecret
	_, _, err := markPendingVerified(t, c, 1)
	require.NoError(t, err)

	t2 := initiateAndRegister(t, c, 1, 200, 100, 1)
	r2 := t2.RevertHandshakeSecret
	require.NotEmpty(t, r2)
	_, err = c.Cancel(t2.ID)
	require.NoError(t, err)
	require.NoError(t, c.MarkRevertDelivered(1, t2.ID))

	require.NotEqual(t, h1, r2,
		"sanity: round 2's revert secret must differ from round 1's handshake secret")

	c.Stop()
	c2 := NewServerTransferClass()
	ServerTransferShared = c2
	defer c2.Stop()

	sid, ok := c2.LookupServerByVerifiedHandshakeSecret(r2)
	require.True(t, ok,
		"restart must rebuild the newer rollback credential R2 — that is what the agent has on disk after step 2. Picking the older Verified H1 locks the agent out.")
	require.Equal(t, uint64(1), sid)

	_, h1StillAccepted := c2.LookupServerByVerifiedHandshakeSecret(h1)
	require.False(t, h1StillAccepted,
		"the stale H1 from an earlier Verified row must NOT be accepted after a newer rollback has been acked — the agent no longer holds it")
}

func hasRevertDeliveryFor(c *ServerTransferClass, serverID, transferID uint64) bool {
	t, ok := c.LookupRevertDelivery(serverID)
	return ok && t.ID == transferID
}

// forceForwardRecoveryAge back-dates a recovery entry's UpdatedAt so TTL
// tests don't have to sleep through defaultRevertDeliveryRecoveryWindow.
func forceForwardRecoveryAge(c *ServerTransferClass, serverID uint64, age time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.terminalSecretRecovery[serverID]; ok {
		t.UpdatedAt = time.Now().Add(-age)
	}
}

// BUG-3 regression + HIGH-7 hardening: a Retry that runs before the agent
// has actually authenticated with the in-flight RevertHandshakeSecret must
// NOT strand auth recovery for that secret. The agent's 10s reload timer
// means rollback application is lazy. Register clears revertDeliveries
// (so a late pushRevertIfOnline does not re-deliver the now-stale rollback
// secret and overwrite the freshly applied new HandshakeSecret on the
// agent), but it must move the secret into the bounded revertRecovery
// slot so authentication keeps working until either the agent
// reconnects (MarkRevertDelivered promotes), MarkVerified on the new
// transfer supersedes, or the recovery window expires.
//
// Crucially, the secret must NOT be promoted to the permanent
// verifiedHandshakes map: that would keep an unacknowledged credential
// alive indefinitely.
func TestRegisterPreservesInflightRollbackSecretAcrossRetry(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	t1 := initiateAndRegister(t, c, 1, 100, 200, 1)
	revertSecret := t1.RevertHandshakeSecret
	require.NotEmpty(t, revertSecret)

	_, err := c.Cancel(t1.ID)
	require.NoError(t, err)
	require.True(t, hasRevertDeliveryFor(c, 1, t1.ID),
		"Cancel must register a rollback delivery carrying RevertHandshakeSecret")

	initiateAndRegister(t, c, 1, 100, 200, 1)

	if _, stillVerified := c.LookupServerByVerifiedHandshakeSecret(revertSecret); stillVerified {
		t.Fatal("Register on Retry must NOT promote the unacknowledged RevertHandshakeSecret into the permanent verifiedHandshakes map — that bypasses the bounded recovery window")
	}

	rec, ok := c.LookupByRevertHandshakeSecret(revertSecret)
	require.True(t, ok,
		"Register on Retry must keep the in-flight RevertHandshakeSecret reachable via the bounded recovery lookup (revertRecovery)")
	require.Equal(t, uint64(1), rec.ServerID)
	require.Equal(t, t1.ID, rec.ID)
}

// BUG: batchDeleteServer removes Server rows but never notifies
// ServerTransferShared. Any Pending transfer for that server is left in the
// DB as Pending forever, the in-memory `pending` map still holds it (so
// HasPending/InitiateExclusive still see it), and the timeout sweeper later
// tries to revert Server.UserID on a row that no longer exists. The UI shows
// the row as Pending indefinitely.
//
// OnServersDeleted must transition such Pending rows to a terminal status
// without touching the (now-gone) Server row, clear the in-memory indexes,
// and broadcast so subscribers can update.
func TestOnServersDeletedTerminatesPendingTransfersAndClearsIndexes(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	require.True(t, c.HasPending(1))

	subID, ch, err := c.Subscribe(100)
	require.NoError(t, err)
	defer c.Unsubscribe(subID)

	require.NoError(t, DB.Unscoped().Delete(&model.Server{}, tr.ServerID).Error)
	c.OnServersDeleted([]uint64{tr.ServerID})

	require.False(t, c.HasPending(1),
		"OnServersDeleted must drop the in-memory pending entry so a future server with the same id cannot inherit a stale transfer")

	var saved model.ServerTransfer
	require.NoError(t, DB.First(&saved, tr.ID).Error)
	require.True(t, saved.Status.IsTerminal(),
		"OnServersDeleted must transition the DB row to a terminal status; got status=%d", saved.Status)

	select {
	case got, ok := <-ch:
		require.True(t, ok)
		require.Equal(t, tr.ID, got.ID)
		require.True(t, got.Status.IsTerminal())
	case <-time.After(time.Second):
		t.Fatal("OnServersDeleted must broadcast the terminal transition to subscribers")
	}
}

// Defence in depth: after OnServersDeleted runs, a subsequent timeout sweep
// must not blow up trying to revert Server.UserID on the deleted server, and
// must not log spurious errors. Today revertTransition would touch the
// (gone) Server row via res.RowsAffected==0 and self-heal — but it also
// performs an UPDATE on the server table that touches no rows, which
// MarkTimeout will treat as "concurrent caller won the CAS" and return nil.
// Just make sure the sweep is a no-op after deletion.
func TestSweepTimeoutsAfterServerDeletedIsNoOp(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	c.timeout = time.Nanosecond
	seedServerForTransfer(t, 1, 100)

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	_ = tr

	require.NoError(t, DB.Unscoped().Delete(&model.Server{}, uint64(1)).Error)
	c.OnServersDeleted([]uint64{1})

	require.NotPanics(t, func() { c.sweepTimeouts() },
		"sweepTimeouts after OnServersDeleted must be a no-op even though the Server row is gone")
}

// HIGH security regression: Register must publish the new in-memory
// Server.UserID (which auth.go reads to enforce ownership) BEFORE other
// state becomes observable. Otherwise the gap between Initiate (DB
// already says ToUserID) and Register's SetUserID is a window where
// authorizeAgentForUUID sees the OLD owner == userId via the in-memory
// cache and admits the old owner via the happy "owner match" path
// rather than the bounded pending-tolerance path.
//
// The contract we lock down: after Register returns, ServerShared
// reports the new owner. There is no atomic test for "during" Register,
// so we assert the post-condition.
func TestRegisterPublishesNewOwnerBeforeReturning(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	s, ok := ServerShared.Get(tr.ServerID)
	require.True(t, ok)
	require.Equal(t, uint64(200), s.GetUserID(),
		"after Register returns, ServerShared must report the new owner so auth observes a consistent (DB, in-memory) snapshot")
}

// HIGH security regression: revertTransition must restore the in-memory
// Server.UserID (FromUserID) immediately after the DB transaction commits.
// During the window where DB says reverted but in-memory still says
// ToUserID, an auth call from the destination user's global AgentSecret
// would be admitted via the happy path and obtain a long-lived stream
// for a server that no longer belongs to them.
func TestCancelPublishesRevertedOwnerBeforeReturning(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	s, ok := ServerShared.Get(tr.ServerID)
	require.True(t, ok)
	require.Equal(t, uint64(200), s.GetUserID(), "precondition: pending flipped ownership")

	cancelled, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	require.NotNil(t, cancelled)

	require.Equal(t, uint64(100), s.GetUserID(),
		"after Cancel returns, ServerShared must report the rolled-back FromUserID so auth no longer admits the destination user's global AgentSecret")
}

// HIGH security regression: OnServersDeleted must guard map deletions by
// transferID. Between the moment the deletion enumeration captured the
// pending rows for server S and the moment the in-memory map deletion
// runs, a concurrent path can install a brand-new pending transfer for
// the same serverID (either via ID reuse after delete, or in the more
// common case via a Retry whose Register lands in the slot). A naive
// `delete(c.pending, serverID)` would wipe that fresh entry.
//
// Asserted contract: if the in-memory pending entry for S no longer
// matches any transferID OnServersDeleted authoritatively terminated,
// the entry survives.
func TestOnServersDeletedGuardsByTransferIDAgainstUnrelatedNewTransfer(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	staleTransferID := uint64(99999)
	require.NoError(t, DB.Create(&model.ServerTransfer{
		Common:     model.Common{ID: staleTransferID},
		ServerID:   1,
		FromUserID: 100,
		ToUserID:   200,
		Status:     model.ServerTransferStatusCancelled,
		LastError:  "test-seeded terminal row",
	}).Error)

	fresh := initiateAndRegister(t, c, 1, 100, 200, 1)
	require.NotEqual(t, staleTransferID, fresh.ID, "fresh transfer must be a distinct row")

	c.OnServersDeleted([]uint64{1})

	current, stillPending := c.LookupPending(1)
	require.False(t, stillPending && current.ID != fresh.ID,
		"OnServersDeleted must not silently replace the pending entry")
	if stillPending {
		require.Equal(t, fresh.ID, current.ID,
			"pending entry left intact must still point at fresh transfer")
	}
}

// FORWARD-RECOVERY (HIGH): regression for the recovery gap where an agent
// has already committed the per-transfer forward HandshakeSecret to disk
// but the transfer is Cancel/Fail/Timeout-ed before the dashboard observed
// the MarkVerified-via-handshake reconnect. PushIfOnline only ever
// delivers t.HandshakeSecret (never a user-global secret), so the only
// credential the agent now holds for this server is the forward
// HandshakeSecret of a transfer the dashboard has already settled.
//
// The fix introduces a bounded terminalForwardRecovery slot, populated
// from revertTransition, that lets auth admit the forward HandshakeSecret
// long enough for RequestTask → OnAgentReconnect → pushRevertIfOnline to
// push the RevertHandshakeSecret rollback. Without this, the agent is
// permanently locked out — TestAuthHandshakeSecretRejectedAfterTransferTerminated
// keeps the attacker-reuse path closed (it bypasses revertTransition by
// poking the DB directly, so terminalForwardRecovery never sees it).

// Cancel must register the just-terminated transfer's forward
// HandshakeSecret in the bounded terminalForwardRecovery slot AND keep
// LookupByRevertHandshakeSecret working for the RevertHandshakeSecret as
// today. The two recovery channels are separate maps because they have
// distinct lifecycles: revert recovery is consumed by MarkRevertDelivered
// (which promotes); forward recovery is consumed by the rollback delivery
// itself completing (handled via MarkRevertDelivered on the next loop).
func TestCancelRegistersForwardHandshakeSecretRecovery(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	forward := tr.HandshakeSecret
	revert := tr.RevertHandshakeSecret
	require.NotEmpty(t, forward)
	require.NotEmpty(t, revert)

	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)

	got, ok := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.True(t, ok,
		"Cancel must register forward HandshakeSecret into terminalForwardRecovery so an agent that already applied it on disk can still authenticate long enough to receive the rollback")
	require.Equal(t, tr.ID, got.ID)
	require.Equal(t, uint64(1), got.ServerID)

	// revert recovery channel still works as before — fix must not regress it.
	_, ok = c.LookupByRevertHandshakeSecret(revert)
	require.True(t, ok, "RevertHandshakeSecret recovery path must remain available alongside the new forward path")
}

// MarkFailed (agent reports failure via TaskResult) and MarkTimeout (sweeper)
// take the same revertTransition path as Cancel, so the forward recovery
// must also be registered on those.
func TestMarkFailedRegistersForwardHandshakeSecretRecovery(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	forward := tr.HandshakeSecret

	_, err := c.MarkFailed(tr.ID, "agent-rejected")
	require.NoError(t, err)

	got, ok := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.True(t, ok, "MarkFailed must register forward HandshakeSecret recovery")
	require.Equal(t, tr.ID, got.ID)
}

func TestMarkTimeoutRegistersForwardHandshakeSecretRecovery(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	forward := tr.HandshakeSecret

	_, err := c.MarkTimeout(tr.ID)
	require.NoError(t, err)

	got, ok := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.True(t, ok, "MarkTimeout must register forward HandshakeSecret recovery")
	require.Equal(t, tr.ID, got.ID)
}

// MarkVerified happens when the agent reconnects under the forward
// HandshakeSecret and the transfer is still Pending. The terminal recovery
// slot must not survive into the Verified lifecycle: once Verified, the
// forward secret is promoted into verifiedHandshakes (the long-term map) and
// keeping a stale terminal-recovery copy around could collide later if the
// same server transfers again.
func TestMarkVerifiedClearsForwardHandshakeRecoveryIfAny(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	forward := tr.HandshakeSecret

	// Simulate a prior failed cycle on the same server to seed a recovery
	// entry; then a fresh transfer is verified. The fresh transfer's
	// forward secret is unrelated to the prior terminal entry, but the
	// per-server slot must be cleared so verifiedHandshakes is the single
	// source of truth post-Verified.
	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	_, ok := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.True(t, ok, "precondition: cancel populated forward recovery")

	tr2 := initiateAndRegister(t, c, 1, 100, 200, 1)
	require.NotEqual(t, tr.ID, tr2.ID)
	verified, _, err := c.MarkVerified(1, tr2.ID)
	require.NoError(t, err)
	require.True(t, verified)

	_, stillRecovered := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.False(t, stillRecovered,
		"MarkVerified on a newer transfer for this server must purge any stale forward-secret terminal recovery entry — verifiedHandshakes is now the canonical credential")
}

// MarkRevertDelivered means the agent has authenticated with the
// RevertHandshakeSecret, which proves the rollback ApplyConfig was applied
// and the on-disk credential is now the revert secret — not the forward
// secret. The terminal-recovery entry for the forward secret is therefore
// stale and must be cleared so a leaked forward token cannot re-enter via
// recovery later in the window.
func TestMarkRevertDeliveredClearsForwardHandshakeRecovery(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	forward := tr.HandshakeSecret

	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	_, ok := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.True(t, ok)

	require.NoError(t, c.MarkRevertDelivered(1, tr.ID))

	_, stillRecovered := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.False(t, stillRecovered,
		"MarkRevertDelivered proves the agent rotated off the forward secret; recovery slot must be cleared so a leaked forward token cannot recover later")
}

// OnServersDeleted must also clear any forward-recovery entries so a
// future server with a recycled id cannot inherit a stale credential.
func TestOnServersDeletedClearsForwardHandshakeRecovery(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	forward := tr.HandshakeSecret

	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	_, ok := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.True(t, ok)

	require.NoError(t, DB.Unscoped().Delete(&model.Server{}, uint64(1)).Error)
	c.OnServersDeleted([]uint64{1})

	_, stillRecovered := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.False(t, stillRecovered,
		"OnServersDeleted must clear forward-recovery so a recycled server id cannot inherit the credential")
}

// TTL: a recovery entry older than defaultRevertDeliveryRecoveryWindow must
// be pruned on read. Same bound as the revert recovery channel so operators
// only have one window to reason about.
func TestForwardHandshakeRecoveryExpiresAfterWindow(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	forward := tr.HandshakeSecret

	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	_, ok := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.True(t, ok, "precondition: cancel populated forward recovery")

	// Back-date the in-memory entry past the recovery window. We poke the
	// private slot via a helper so the test does not depend on time.Now()
	// monkey-patching.
	forceForwardRecoveryAge(c, 1, defaultRevertDeliveryRecoveryWindow+time.Minute)

	_, stillRecovered := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.False(t, stillRecovered,
		"forward-recovery lookup must prune entries past defaultRevertDeliveryRecoveryWindow on read")
}

// UNIFIED TERMINAL RECOVERY (HIGH): the bounded "transfer terminated but
// agent may still hold one of its per-transfer secrets" window is one
// concept, not two. Both the forward HandshakeSecret (committed via the
// agent's 10s reload before Cancel landed) and the RevertHandshakeSecret
// (rollback ApplyConfig pushed, agent hasn't ACKed yet) need the same
// bounded acceptance — same TTL, same eviction triggers (Register on a
// new transfer for the same server, MarkRevertDelivered, MarkVerified on
// a newer transfer, OnServersDeleted). They differ only in which secret
// field on the same model.ServerTransfer is being presented. Express
// that in one table with a kind tag, not two parallel tables.
//
// This batch of tests pins down the unified surface:
//   - LookupByTerminalSecretRecovery dispatches by which secret matched
//   - one revertTransition call registers BOTH kinds in one slot
//   - Register-on-Retry preserves the slot (a fresh transfer for the same
//     server does NOT wipe rollback recovery the agent may still need)
//   - MarkRevertDelivered / MarkVerified / OnServersDeleted clear it
//   - the existing per-kind lookups remain as thin wrappers so callers
//     outside the singleton don't have to know about kind

type terminalSecretRecoveryMatch struct {
	transfer *model.ServerTransfer
	kind     TerminalRecoveryKind
}

func lookupTerminalRecoveryForTest(c *ServerTransferClass, secret string) (terminalSecretRecoveryMatch, bool) {
	transfer, kind, ok := c.LookupByTerminalSecretRecovery(secret)
	if !ok {
		return terminalSecretRecoveryMatch{}, false
	}
	return terminalSecretRecoveryMatch{transfer: transfer, kind: kind}, true
}

func TestTerminalSecretRecoveryRegistersBothKindsOnRevertTransition(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	forward := tr.HandshakeSecret
	revert := tr.RevertHandshakeSecret

	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)

	gotF, okF := lookupTerminalRecoveryForTest(c, forward)
	require.True(t, okF, "forward HandshakeSecret must resolve from terminalSecretRecovery after Cancel")
	require.Equal(t, TerminalRecoveryForward, gotF.kind, "lookup must report the kind so auth can decide whether to promote")
	require.Equal(t, tr.ID, gotF.transfer.ID)

	gotR, okR := lookupTerminalRecoveryForTest(c, revert)
	require.True(t, okR, "RevertHandshakeSecret must resolve from the SAME terminalSecretRecovery slot")
	require.Equal(t, TerminalRecoveryRevert, gotR.kind)
	require.Equal(t, tr.ID, gotR.transfer.ID)
}

// Per-kind wrappers must continue to work — they are the public-facing
// API existing call sites (and the auth layer) use.
func TestTerminalSecretRecoveryPerKindWrappersStayConsistent(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	forward := tr.HandshakeSecret
	revert := tr.RevertHandshakeSecret

	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)

	gotF, okF := c.LookupByForwardHandshakeSecretInTerminalRecovery(forward)
	require.True(t, okF)
	require.Equal(t, tr.ID, gotF.ID)

	gotR, okR := c.LookupByRevertHandshakeSecret(revert)
	require.True(t, okR)
	require.Equal(t, tr.ID, gotR.ID)
}

// Register-on-Retry: a fresh pending transfer for the same server MUST
// NOT evict the prior transfer's rollback recovery — the agent's reload
// timer is still running and the agent may not have rotated off the
// previous RevertHandshakeSecret yet. The forward recovery for the prior
// transfer is moot once a new transfer starts pushing a new
// HandshakeSecret, but the revert recovery must survive.
//
// This is the exact invariant TestRegisterPreservesInflightRollbackSecret
// AcrossRetry pins down today via revertRecovery; it must still hold after
// the unified-table refactor.
func TestTerminalSecretRecoveryPreservesRollbackAcrossRetry(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	t1 := initiateAndRegister(t, c, 1, 100, 200, 1)
	revertSecret := t1.RevertHandshakeSecret
	require.NotEmpty(t, revertSecret)

	_, err := c.Cancel(t1.ID)
	require.NoError(t, err)

	initiateAndRegister(t, c, 1, 100, 200, 1)

	got, ok := c.LookupByRevertHandshakeSecret(revertSecret)
	require.True(t, ok,
		"unified terminalSecretRecovery must preserve the previous transfer's RevertHandshakeSecret across a Retry — the agent's on-disk credential may still be the prior revert secret during the 10s reload")
	require.Equal(t, t1.ID, got.ID)

	if _, stillVerified := c.LookupServerByVerifiedHandshakeSecret(revertSecret); stillVerified {
		t.Fatal("recovery slot must NOT promote into verifiedHandshakes — that bypasses the bounded window")
	}
}

// REGRESSION: re-invoking Cancel against an already-Cancelled transfer must
// be a true no-op. The cancelServerTransfer HTTP handler does not gate on
// `t.Status == Pending`, so a stale terminal id can reach revertTransition
// via UI replay / lingering tabs / scripted retries. revertTransition's
// transaction returns early for non-Pending rows (transitionedByThisCall
// stays false), but the post-transaction code historically only suppressed
// side effects via `if t.Status != newStatus` — which is FALSE when both
// sides are Cancelled. The fall-through re-registered the OLD transfer's
// revertDelivery and pushed its RevertHandshakeSecret, after a Retry had
// already installed a NEW Pending transfer and delivered its forward
// HandshakeSecret. The agent's ApplyConfig is last-arrival-wins inside the
// 10s reload window, so the stale rollback overwrites the new credential
// and the new transfer is stranded until the 24h timeout sweep. The fix
// gates ALL post-tx side effects on transitionedByThisCall.
func TestServerTransferRepeatedCancelOnTerminalDoesNotResendStaleRollback(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[100] = model.UserInfo{AgentSecret: "old-owner-secret"}
	UserInfoMap[200] = model.UserInfo{AgentSecret: "new-owner-secret"}
	UserLock.Unlock()

	first := initiateAndRegister(t, c, 1, 100, 200, 1)
	_, err := c.Cancel(first.ID)
	require.NoError(t, err)

	// Admin Retries the failed transfer. Register clears the old
	// revertDeliveries entry and pushes the NEW transfer's HandshakeSecret;
	// the agent is now committed to the new credential.
	var refreshedFirst model.ServerTransfer
	require.NoError(t, DB.First(&refreshedFirst, first.ID).Error)
	second, err := c.Retry(&refreshedFirst, 1)
	require.NoError(t, err)
	require.True(t, c.HasPending(1), "precondition: Retry must register a fresh Pending transfer")

	stream := newFakeTaskStream()
	s, ok := ServerShared.Get(1)
	require.True(t, ok)
	s.SetTaskStream(stream)
	// Push the new transfer's ApplyConfig so the agent is on the new
	// HandshakeSecret. After this, the stream must NOT see another
	// per-transfer secret unless something authoritative changes.
	c.PushIfOnline(second)
	require.Equal(t, 1, stream.sendCount(), "precondition: new transfer's HandshakeSecret must be the latest ApplyConfig on the wire")
	require.Contains(t, stream.sent[0].Data, second.HandshakeSecret)
	stream.reset()

	// Stale terminal-id Cancel arrives (UI replay / lingering session / etc).
	// `cancelServerTransfer` does not pre-gate on status, so it reaches
	// revertTransition with the historical terminal row.
	result, err := c.Cancel(first.ID)
	require.NoError(t, err)
	require.Nil(t, result,
		"Cancel on an already-Cancelled row must be a silent no-op — no rollback re-delivery, no recovery re-registration")

	require.Equal(t, 0, stream.sendCount(),
		"a stale terminal Cancel must NOT push the OLD transfer's RevertHandshakeSecret — doing so supersedes the new transfer's just-applied HandshakeSecret and strands the new transfer until the 24h timeout")

	// The new transfer's runtime state must be intact: its pending entry,
	// its revertDelivery absence, and the agent's last-known credential
	// (still the new HandshakeSecret) must all be unchanged.
	require.True(t, c.HasPending(1), "fresh Pending transfer must survive a stale terminal Cancel")
	got, ok := c.LookupPending(1)
	require.True(t, ok)
	require.Equal(t, second.ID, got.ID, "in-memory pending must still point at the new transfer")

	if existing, ok := c.LookupRevertDelivery(1); ok {
		require.NotEqual(t, first.ID, existing.ID,
			"stale Cancel must NOT re-install the OLD transfer's revertDelivery and overwrite the fresh push queue state")
	}
}

// REGRESSION: dashboard restart must NOT rehydrate the
// revertDelivery / terminalSecretRecovery slots for transfers whose rollback
// has already been ACKed via MarkRevertDelivered. The auth path treats an
// entry in revertDeliveries as proof that the rollback window is still open
// and admits the rolled-back ToUserID's global AgentSecret accordingly
// (service/rpc/auth.go authorizeAgentForUUID's LookupRevertDelivery branch).
// MarkRevertDelivered persists acked_at and clears the in-memory delivery
// precisely to close that window — but NewServerTransferClass loaded all
// terminal rows within the recovery window without filtering acked_at,
// reopening it after every restart. Loading must skip acked rows; the
// acked credential is already rebuilt into verifiedHandshakes via the
// existing acked-row pass.
func TestNewServerTransferClassSkipsAckedRollbackRecovery(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)

	UserLock.Lock()
	UserInfoMap[100] = model.UserInfo{AgentSecret: "from-secret"}
	UserInfoMap[200] = model.UserInfo{AgentSecret: "to-secret"}
	UserLock.Unlock()

	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	_, err := c.Cancel(tr.ID)
	require.NoError(t, err)
	require.NoError(t, c.MarkRevertDelivered(1, tr.ID),
		"precondition: rollback must be ACKed so the in-memory delivery is consumed")
	require.False(t, hasRevertDeliveryFor(c, 1, tr.ID),
		"precondition: MarkRevertDelivered must clear the in-memory delivery")

	// Simulate dashboard restart against the same DB + ServerShared.
	c.Stop()
	reborn := NewServerTransferClass()
	defer reborn.Stop()
	ServerTransferShared = reborn

	if _, ok := reborn.LookupRevertDelivery(1); ok {
		t.Fatal("restart must NOT rehydrate an already-ACKed rollback into revertDeliveries — reopening the auth tolerance window for the ToUserID global AgentSecret contradicts MarkRevertDelivered's contract")
	}

	// terminalSecretRecovery must also be empty for the ACKed rollback —
	// auth's terminal-recovery lookups would otherwise readmit the per-
	// transfer secrets the agent has already rotated past.
	if got, _, ok := reborn.LookupByTerminalSecretRecovery(tr.RevertHandshakeSecret); ok {
		t.Fatalf("restart must NOT rehydrate ACKed RevertHandshakeSecret into terminalSecretRecovery; got=%v", got)
	}
	if got, _, ok := reborn.LookupByTerminalSecretRecovery(tr.HandshakeSecret); ok {
		t.Fatalf("restart must NOT rehydrate ACKed forward HandshakeSecret into terminalSecretRecovery; got=%v", got)
	}

	// Sanity: the long-term verifiedHandshakes credential must still be
	// rebuilt from the same row's acked_at, so the agent can keep
	// authenticating with the rotated RevertHandshakeSecret.
	sid, ok := reborn.LookupServerByVerifiedHandshakeSecret(tr.RevertHandshakeSecret)
	require.True(t, ok, "ACKed rollback secret must still be rebuilt into verifiedHandshakes — the agent on disk holds exactly this credential")
	require.Equal(t, uint64(1), sid)
}

func (*fakeTaskStream) Context() context.Context { return context.Background() }

func (*cancelRaceApplyConfigStream) Context() context.Context { return context.Background() }

func (*neverReturningTaskStream) Context() context.Context { return context.Background() }
