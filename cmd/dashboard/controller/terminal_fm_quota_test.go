package controller

// TDD regression tests for GHSA-jx78-55p5-rwv5 (CVE-2026-53522):
// Unbounded WebSocket Streams — Resource Exhaustion DoS.
//
// The vulnerability: POST /api/v1/terminal and POST /api/v1/file insert a new
// context into an unbounded map with no per-user rate limit, global semaphore,
// or per-server connection cap, letting any authenticated user exhaust server
// resources until the dashboard crashes.
//
// The fix: createStreamLocked enforces maxStreamsPerUser (20) and
// maxStreamsPerServer (40). These tests verify the fix is effective end-to-end
// through the HTTP controller handlers, not just at the rpc layer.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

const (
	// Must match rpc.maxStreamsPerUser so the test fills exactly the right cap.
	quotaTestUserCap = 20
	// Must match rpc.maxStreamsPerServer.
	quotaTestServerCap = 40
)

// setupQuotaTest initialises the shared fixtures used by all quota tests:
// a fresh NezhaHandler, a server (ID 7) owned by the test user (ID 100),
// and a task stream that succeeds so that created streams stay in the registry.
func setupQuotaTest(t *testing.T) (cleanup func(), successStream *failingRequestTaskStream) {
	t.Helper()
	cleanupFixture, _ := setupMCPTest(t)
	originalHandler := rpc.NezhaHandlerSingleton
	rpc.NezhaHandlerSingleton = rpc.NewNezhaHandler()
	successStream = &failingRequestTaskStream{err: nil}
	server, ok := singleton.ServerShared.Get(7)
	require.True(t, ok)
	server.SetTaskStream(successStream)
	return func() {
		rpc.NezhaHandlerSingleton = originalHandler
		cleanupFixture()
	}, successStream
}

// TestCreateTerminalEnforcesPerUserStreamQuota verifies that once a user has
// reached the per-user stream cap, subsequent createTerminal calls are rejected
// with ErrTooManyStreamsForUser. This directly tests the GHSA-jx78-55p5-rwv5
// fix at the HTTP handler layer.
func TestCreateTerminalEnforcesPerUserStreamQuota(t *testing.T) {
	cleanup, _ := setupQuotaTest(t)
	defer cleanup()

	// Fill the per-user quota.
	for i := 0; i < quotaTestUserCap; i++ {
		req := newAuthorizedControllerContext(t, "POST", "/terminal", model.TerminalForm{ServerID: 7})
		_, err := createTerminal(req)
		require.NoError(t, err, "terminal %d must succeed within per-user quota", i+1)
	}

	// The (quotaTestUserCap+1)-th call must be rejected.
	req := newAuthorizedControllerContext(t, "POST", "/terminal", model.TerminalForm{ServerID: 7})
	_, err := createTerminal(req)
	require.Error(t, err, "createTerminal must return an error when user quota is exhausted")
	require.True(t, errors.Is(err, rpc.ErrTooManyStreamsForUser),
		"error must be ErrTooManyStreamsForUser when user quota is exhausted, got: %v", err)
}

// TestCreateFMEnforcesPerUserStreamQuota is the FM counterpart of the terminal
// quota test: POST /file must also be blocked once the per-user stream cap is
// reached.
func TestCreateFMEnforcesPerUserStreamQuota(t *testing.T) {
	cleanup, _ := setupQuotaTest(t)
	defer cleanup()

	for i := 0; i < quotaTestUserCap; i++ {
		req := newAuthorizedControllerContext(t, "POST", "/file?id=7", nil)
		req.Request.URL.RawQuery = "id=7"
		_, err := createFM(req)
		require.NoError(t, err, "FM session %d must succeed within per-user quota", i+1)
	}

	req := newAuthorizedControllerContext(t, "POST", "/file?id=7", nil)
	req.Request.URL.RawQuery = "id=7"
	_, err := createFM(req)
	require.Error(t, err, "createFM must return an error when user quota is exhausted")
	require.True(t, errors.Is(err, rpc.ErrTooManyStreamsForUser),
		"error must be ErrTooManyStreamsForUser when user quota is exhausted, got: %v", err)
}

// TestCreateTerminalEnforcesPerServerStreamQuota verifies that even when a
// single user's quota is not yet reached, createTerminal rejects streams once
// the per-server cap is hit. This guards against a distributed attack where
// many users flood one server.
func TestCreateTerminalEnforcesPerServerStreamQuota(t *testing.T) {
	cleanup, _ := setupQuotaTest(t)
	defer cleanup()

	// Pre-fill the per-server quota with dashboard-internal streams
	// (creatorUserID=0 bypasses the per-user cap so we can reach the server cap
	// without needing quotaTestServerCap distinct users).
	for i := 0; i < quotaTestServerCap; i++ {
		require.NoError(t,
			rpc.NezhaHandlerSingleton.CreateStream(fmt.Sprintf("server-filler-%d", i), 0, 7),
			"pre-fill server quota stream %d must succeed", i+1,
		)
	}

	// User 100 has used 0 of their personal quota; the server is saturated.
	req := newAuthorizedControllerContext(t, "POST", "/terminal", model.TerminalForm{ServerID: 7})
	_, err := createTerminal(req)
	require.Error(t, err, "createTerminal must return an error when server quota is exhausted")
	require.True(t, errors.Is(err, rpc.ErrTooManyStreamsForServer),
		"error must be ErrTooManyStreamsForServer when server quota is exhausted, got: %v", err)
}

// TestCreateFMEnforcesPerServerStreamQuota is the FM counterpart: POST /file
// must also be blocked once the per-server stream cap is reached.
func TestCreateFMEnforcesPerServerStreamQuota(t *testing.T) {
	cleanup, _ := setupQuotaTest(t)
	defer cleanup()

	for i := 0; i < quotaTestServerCap; i++ {
		require.NoError(t,
			rpc.NezhaHandlerSingleton.CreateStream(fmt.Sprintf("server-filler-fm-%d", i), 0, 7),
			"pre-fill server quota stream %d must succeed", i+1,
		)
	}

	req := newAuthorizedControllerContext(t, "POST", "/file?id=7", nil)
	req.Request.URL.RawQuery = "id=7"
	_, err := createFM(req)
	require.Error(t, err, "createFM must return an error when server quota is exhausted")
	require.True(t, errors.Is(err, rpc.ErrTooManyStreamsForServer),
		"error must be ErrTooManyStreamsForServer when server quota is exhausted, got: %v", err)
}
