package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pre-restore backup runs inside the apply request. A reverse proxy
// timing out or the browser going away must not abort it halfway.
func TestApplyPreRestoreBackupOutlivesRequestCancel(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = e.m.Apply(ctx, ApplyRequest{Token: res.RestoreToken, Password: testPassword, Confirm: ConfirmWord(res.Name), Reauth: okReauth, Actor: admin})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return e.restarts.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
}

// While the pre-restore backup runs, other requests must answer at once
// instead of waiting for it behind the restore lock.
func TestApplyPreRestoreBackupDoesNotBlockOtherOperations(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	snapshot := e.m.opts.Snapshot
	e.m.opts.Snapshot = func(ctx context.Context, dst string) error {
		close(entered)
		<-release
		return snapshot(ctx, dst)
	}
	applied := make(chan error, 1)
	go func() {
		applied <- e.m.Apply(context.Background(), ApplyRequest{Token: res.RestoreToken, Password: testPassword, Confirm: ConfirmWord(res.Name), Reauth: okReauth, Actor: admin})
	}()
	<-entered

	started := make(chan error, 1)
	go func() {
		_, err := e.m.StartBackup(TriggerManual, admin)
		started <- err
	}()
	select {
	case err := <-started:
		require.ErrorIs(t, err, ErrBusy)
	case <-time.After(2 * time.Second):
		close(release)
		<-applied
		t.Fatal("StartBackup waited for the pre-restore backup")
	}
	// The detached stage cannot be cancelled or expired from under the apply.
	e.m.Cancel(res.RestoreToken, admin)
	close(release)
	require.NoError(t, <-applied)
	_, err = os.Stat(filepath.Join(e.dir, pendingDirName, pendingFileName))
	require.NoError(t, err)
}

// A failed pre-restore backup leaves the check usable for another attempt.
func TestApplyKeepsStageWhenPreRestoreBackupFails(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)

	snapshot := e.m.opts.Snapshot
	e.m.opts.Snapshot = func(context.Context, string) error { return errors.New("injected snapshot failure") }
	err = e.apply(res, testPassword)
	require.ErrorIs(t, err, ErrPreRestore)
	e.m.opts.Snapshot = snapshot
	require.NoError(t, e.apply(res, testPassword))
}

// The limiter must not keep an entry for every user and action forever.
func TestRateLimiterForgetsIdleUsers(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	l := newRateLimiter(func() time.Time { return now })
	for id := uint64(1); id <= 50; id++ {
		require.NoError(t, l.allow(id, "restore"))
	}
	now = now.Add(rateWindow)
	require.NoError(t, l.allow(51, "restore"))
	assert.Len(t, l.hits, 1, "entries whose attempts all left the window are dropped")
}

// The metadata cache only keeps objects that are still listed.
func TestObjectMetaCacheDropsDeletedBackups(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	first := e.backup(TriggerManual)
	e.backup(TriggerManual)
	_, err := e.m.listStored(context.Background(), e.settings())
	require.NoError(t, err)
	assert.Len(t, e.m.statCache, 2)
	require.NoError(t, e.m.Delete(context.Background(), []string{first.Name}, admin))
	_, err = e.m.listStored(context.Background(), e.settings())
	require.NoError(t, err)
	assert.Len(t, e.m.statCache, 1)
}

// The downloaded or uploaded file is removed once its contents are staged.
// It must be closed first: Windows cannot remove an open file.
func TestPrepareRemovesTheCheckedBackupFile(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	_, err := e.prepare(b, testPassword)
	require.NoError(t, err)
	require.NotNil(t, e.m.stage)
	_, err = os.Stat(filepath.Join(e.m.stage.dir, stageBackupFile))
	assert.True(t, os.IsNotExist(err), "staged backup file left behind: %v", err)
}

func (e *testEnv) settings() Settings {
	s, _ := e.m.effective()
	return s
}
