package singleton

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type markVerifiedResult struct {
	verified bool
	err      error
}

// startMarkVerifiedParkedInDB runs MarkVerified(serverID, transferID) in a
// goroutine and returns once its CAS UPDATE is parked inside the DB layer,
// which is where a slow or contended database holds it in production. release
// lets the UPDATE proceed; done delivers MarkVerified's outcome.
func startMarkVerifiedParkedInDB(t *testing.T, c *ServerTransferClass, serverID, transferID uint64) (release func(), done <-chan markVerifiedResult) {
	t.Helper()
	writing, parked := make(chan struct{}), make(chan struct{})
	var park, unpark sync.Once
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("park_verify_cas", func(tx *gorm.DB) {
		park.Do(func() {
			close(writing)
			<-parked
		})
	}))
	release = func() { unpark.Do(func() { close(parked) }) }
	t.Cleanup(release)

	result := make(chan markVerifiedResult, 1)
	go func() {
		verified, _, err := c.MarkVerified(serverID, transferID)
		result <- markVerifiedResult{verified, err}
	}()
	select {
	case <-writing:
	case got := <-result:
		t.Fatalf("MarkVerified returned without writing the DB (verified=%v err=%v)", got.verified, got.err)
	}
	return release, result
}

// Every agent sign-in and every control admission reads the transfer index,
// whichever server it is for, so MarkVerified must not keep the index locked
// while its UPDATE waits for the database: the agent presenting a handshake
// secret decides when that happens.
func TestServerTransferMarkVerifiedDoesNotLockIndexWhileWritingDB(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	seedServerForTransfer(t, 2, 300)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)
	require.True(t, c.PendingSourceOwner(1, 100))
	require.False(t, c.PendingSourceOwner(1, 200))

	release, first := startMarkVerifiedParkedInDB(t, c, 1, tr.ID)

	type view struct {
		otherTransitioning, transitioning, pending, sourceTolerated bool
		initiateErr                                                 error
	}
	seen := make(chan view, 1)
	go func() {
		var v view
		// What a sign-in with an unrelated secret and a task for an
		// unrelated server take on their way.
		c.LookupByHandshakeSecret("unrelated")
		c.LookupByTerminalSecretRecovery("unrelated")
		c.LookupServerByVerifiedHandshakeSecret("unrelated")
		v.otherTransitioning = ServerControlTransitioning(2)
		v.transitioning = ServerControlTransitioning(1)
		v.pending = c.HasPending(1)
		v.sourceTolerated = c.PendingSourceOwner(1, 100)
		_, v.initiateErr = c.InitiateExclusive(1, 200, 300, 1)
		seen <- v
	}()
	var v view
	select {
	case v = <-seen:
	case <-time.After(5 * time.Second):
		release()
		<-first
		<-seen
		t.Fatal("transfer index lookups waited for MarkVerified's DB write")
	}
	require.False(t, v.otherTransitioning)
	// The row may already be Verified: nothing the pending entry keeps closed
	// may open, and the old owner's tolerance must already be gone.
	require.True(t, v.transitioning)
	require.True(t, v.pending)
	require.False(t, v.sourceTolerated, "old owner's AgentSecret must not be tolerated while the transfer is being verified")
	require.ErrorIs(t, v.initiateErr, ErrServerAlreadyTransferring)

	// A second presentation of the handshake secret waits for the first and
	// then finds the promoted credential rather than a half-settled transfer.
	second := make(chan markVerifiedResult, 1)
	promoted := make(chan bool, 1)
	go func() {
		verified, _, err := c.MarkVerified(1, tr.ID)
		_, ok := c.LookupServerByVerifiedHandshakeSecret(tr.HandshakeSecret)
		promoted <- ok
		second <- markVerifiedResult{verified, err}
	}()

	release()
	got := <-first
	require.NoError(t, got.err)
	require.True(t, got.verified)
	require.True(t, <-promoted)
	got = <-second
	require.NoError(t, got.err)
	require.False(t, got.verified)
	require.False(t, c.HasPending(1))
	require.False(t, c.PendingSourceOwner(1, 100))
}

// With the index unlocked during the CAS, a Cancel / timeout sweep that loses
// the CAS no longer waits for MarkVerified before its self-heal drops the
// pending entry. The server must stay claimed until MarkVerified has applied
// its outcome: a transfer initiated in between would have its pending entry
// deleted and its server's verified credential overwritten by that outcome.
func TestServerTransferInitiateRefusedWhileMarkVerifiedIsSettling(t *testing.T) {
	c, cleanup := setupTransferFixture(t)
	defer cleanup()
	seedServerForTransfer(t, 1, 100)
	tr := initiateAndRegister(t, c, 1, 100, 200, 1)

	release, first := startMarkVerifiedParkedInDB(t, c, 1, tr.ID)

	// revertTransition's self-heal for a row that is no longer Pending. The
	// fixture pins the pool to the one connection the parked UPDATE holds, so
	// Cancel itself cannot run here.
	require.True(t, c.mu.TryLock(), "MarkVerified must not hold the transfer index during its DB write")
	delete(c.pending, 1)
	c.mu.Unlock()

	refused := make(chan error, 1)
	go func() {
		_, err := c.InitiateExclusive(1, 200, 300, 1)
		refused <- err
	}()
	select {
	case err := <-refused:
		require.ErrorIs(t, err, ErrServerAlreadyTransferring, "no new transfer may start while MarkVerified is settling the previous one")
	case <-time.After(5 * time.Second):
		release()
		<-first
		<-refused
		t.Fatal("InitiateExclusive went on to the DB while MarkVerified was settling the previous transfer")
	}

	release()
	got := <-first
	require.NoError(t, got.err)
	require.True(t, got.verified)
	_, ok := c.LookupServerByVerifiedHandshakeSecret(tr.HandshakeSecret)
	require.True(t, ok)

	next, err := c.InitiateExclusive(1, 200, 300, 1)
	require.NoError(t, err, "the server is free for a new transfer once MarkVerified has returned")
	require.True(t, c.PendingSourceOwner(1, 200))
	pending, ok := c.LookupPending(1)
	require.True(t, ok)
	require.Equal(t, next.ID, pending.ID)
}
