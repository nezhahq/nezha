package controller

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// A full audit queue may shed anonymous failures, never the row of a
// credentialed call: a member could otherwise bury server.exec (or anyone
// else's calls) under a burst of cheap ones and leave no trace of the loss.
func TestMCPAuditWriteFullQueueKeepsCredentialedRows(t *testing.T) {
	cleanup, uid := setupMCPTestWithDatabase(t, filepath.Join(t.TempDir(), "mcp-audit.sqlite"))
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerExec}, nil)
	mcpAuditSync = false

	// Park the writer inside its first INSERT so the queue behind it fills up.
	parked, release := make(chan struct{}), make(chan struct{})
	var parkOnce sync.Once
	require.NoError(t, singleton.DB.Callback().Create().Before("gorm:create").Register("park_mcp_audit_writer", func(*gorm.DB) {
		parkOnce.Do(func() {
			close(parked)
			<-release
		})
	}))
	unpark := sync.OnceFunc(func() { close(release) })
	defer unpark()

	credentialed := func(tool string) model.MCPAuditLog {
		return model.MCPAuditLog{UserID: uid, TokenID: tok.ID, Tool: tool, Outcome: model.MCPOutcomeOK}
	}
	mcpAuditWrite(credentialed("meta.whoami"), nil)
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("audit writer never picked up the first row")
	}
	for range cap(mcpAuditQueue) {
		mcpAuditWrite(credentialed("meta.whoami"), nil)
	}

	// Best-effort: returns at once and is dropped.
	mcpAuditWrite(model.MCPAuditLog{Tool: "fs.upload", Outcome: model.MCPOutcomePermDenied}, nil)

	queued := make(chan struct{})
	go func() {
		mcpAuditWrite(credentialed("server.exec"), nil)
		close(queued)
	}()
	select {
	case <-queued:
		t.Fatal("audit row of a credentialed call was dropped because the queue was full")
	case <-time.After(100 * time.Millisecond):
	}

	unpark()
	<-queued
	require.Eventually(t, func() bool { return countAuditRows(t, "server.exec", "") == 1 },
		// 129 file-backed SQLite inserts take well over 5 s on Windows runners.
		60*time.Second, 10*time.Millisecond, "credentialed call must leave its audit row once the writer catches up")
	require.Equal(t, int64(cap(mcpAuditQueue)+1), countAuditRows(t, "meta.whoami", ""))
	require.Zero(t, countAuditRows(t, "fs.upload", ""), "anonymous failures stay best-effort when the queue is full")
}
