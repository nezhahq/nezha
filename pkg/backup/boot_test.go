package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serverNames(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query("SELECT name FROM servers ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		out = append(out, n)
	}
	return out
}

// stageRestore backs up the current data, changes it, and applies the
// backup up to the point where the dashboard restarts.
func stageRestore(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	_, err := e.db.Exec("DELETE FROM servers WHERE name = 'alpha'; INSERT INTO servers (name) VALUES ('gamma')")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(e.confPath, []byte("listen_port: 9999\nagent_secret_key: changed\n"), 0600))
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)
	require.NoError(t, e.apply(res, testPassword))
	// The dashboard shuts down before the swap.
	require.NoError(t, e.db.Close())
	return e
}

func TestBootRestoreSwapsAndCommits(t *testing.T) {
	e := stageRestore(t)
	assert.Equal(t, []string{"beta", "gamma"}, serverNames(t, e.dbPath))

	boot, err := BootRestore(e.confPath, e.dbPath+"?_busy_timeout=5000")
	require.NoError(t, err)
	require.True(t, boot.Applied())
	assert.Equal(t, []string{"alpha", "beta"}, serverNames(t, e.dbPath), "the data is back to the state of the backup")
	conf, err := os.ReadFile(e.confPath)
	require.NoError(t, err)
	assert.Contains(t, string(conf), "agent_secret_key: original")
	insp, err := inspectDatabase(e.dbPath)
	require.NoError(t, err)
	assert.Equal(t, int64(0), insp.tables["jwt_sessions"], "everyone signs in again")
	_, err = os.Stat(filepath.Join(e.dir, pendingDirName))
	assert.True(t, os.IsNotExist(err), "staging is consumed")
	_, err = os.Stat(filepath.Join(e.dir, rollbackDirName, FileDatabase))
	require.NoError(t, err, "previous data is kept until the next successful start")

	require.NoError(t, boot.Commit())
	r, err := readRestoreResult(e.dir)
	require.NoError(t, err)
	assert.Equal(t, "success", r.Status)

	// Next start: nothing pending, the old copy is removed.
	boot, err = BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	_, err = os.Stat(filepath.Join(e.dir, rollbackDirName))
	assert.True(t, os.IsNotExist(err))
}

func TestBootRestoreRollsBackFailedStart(t *testing.T) {
	e := stageRestore(t)
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	require.True(t, boot.Applied())
	// The restored database got a journal while starting; it must not
	// survive the rollback next to the previous database.
	require.NoError(t, os.WriteFile(e.dbPath+"-journal", []byte("restored journal"), 0600))

	boot.Rollback(errors.New("migration failed"))
	assert.Equal(t, []string{"beta", "gamma"}, serverNames(t, e.dbPath))
	conf, _ := os.ReadFile(e.confPath)
	assert.Contains(t, string(conf), "agent_secret_key: changed")
	_, err = os.Stat(e.dbPath + "-journal")
	assert.True(t, os.IsNotExist(err))
	r, err := readRestoreResult(e.dir)
	require.NoError(t, err)
	assert.Equal(t, "failed", r.Status)
	assert.True(t, r.RolledBack)
	assert.Contains(t, r.Error, "migration failed")
}

// The process dies after the swap and before Commit: the next start puts
// the previous data back.
func TestBootRestoreRecoversFromCrash(t *testing.T) {
	e := stageRestore(t)
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	require.True(t, boot.Applied())

	boot, err = BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	assert.Equal(t, []string{"beta", "gamma"}, serverNames(t, e.dbPath))
	r, err := readRestoreResult(e.dir)
	require.NoError(t, err)
	assert.Equal(t, "failed", r.Status)
	assert.True(t, r.RolledBack)
}

func TestBootRestoreRefusesModifiedStaging(t *testing.T) {
	e := stageRestore(t)
	entries, err := os.ReadDir(filepath.Join(e.dir, pendingDirName))
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() {
			require.NoError(t, os.WriteFile(filepath.Join(e.dir, pendingDirName, entry.Name(), stageFilesDir, FileDatabase), []byte("evil"), 0600))
		}
	}
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	assert.Equal(t, []string{"beta", "gamma"}, serverNames(t, e.dbPath), "nothing was swapped")
	r, err := readRestoreResult(e.dir)
	require.NoError(t, err)
	assert.Equal(t, "failed", r.Status)
	assert.False(t, r.RolledBack)
}

func TestBootRestoreIgnoresStaleStaging(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	_, err := e.prepare(b, testPassword) // checked, never applied
	require.NoError(t, err)
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	_, err = os.Stat(filepath.Join(e.dir, pendingDirName))
	assert.True(t, os.IsNotExist(err), "decrypted staging is not left behind")
	_, err = e.m.RestoreStatus()
	require.NoError(t, err)
}

func TestBootRestoreWithMemoryDatabase(t *testing.T) {
	boot, err := BootRestore("data/config.yaml", ":memory:")
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	_, err = NewManager(Options{DataDir: ""})
	require.ErrorIs(t, err, ErrDisabled)
	_ = context.Background()
}

// addWALOnlyRow commits a row that exists only in the -wal file of the
// database at dbPath (as after a crash, before any checkpoint).
func addWALOnlyRow(t *testing.T, dbPath, name string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0"} {
		_, err := db.Exec(stmt)
		require.NoError(t, err)
	}
	_, err = db.Exec("INSERT INTO servers (name) VALUES (?)", name)
	require.NoError(t, err)
	main, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	wal, err := os.ReadFile(dbPath + "-wal")
	require.NoError(t, err)
	require.NotEmpty(t, wal)
	require.NoError(t, db.Close()) // checkpoints; put the pre-checkpoint files back
	require.NoError(t, os.WriteFile(dbPath, main, 0600))
	require.NoError(t, os.WriteFile(dbPath+"-wal", wal, 0600))
	_ = os.Remove(dbPath + "-shm")
	// Without its WAL the database does not have the row.
	bare := filepath.Join(t.TempDir(), "bare.db")
	require.NoError(t, os.WriteFile(bare, main, 0600))
	require.NotContains(t, serverNames(t, bare), name)
}

// countBootOps returns the file operations of one complete swap.
func countBootOps(t *testing.T) []string {
	t.Helper()
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	var ops []string
	bootFault = func(op string) error { ops = append(ops, op); return nil }
	defer func() { bootFault = nil }()
	_, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	return ops
}

// A start that dies at any point of the swap is rolled back by the next
// start, and the previous database keeps the transactions committed only in
// its WAL.
func TestBootRestoreCrashDuringSwapKeepsOldData(t *testing.T) {
	ops := countBootOps(t)
	require.GreaterOrEqual(t, len(ops), 4, "%v", ops)
	for i := range ops {
		if i == 0 && strings.HasPrefix(ops[i], "write ") {
			// Before the plan exists nothing has happened; covered by
			// TestBootRestoreCrashBeforePlanWrittenRetries.
			continue
		}
		t.Run(ops[i], func(t *testing.T) {
			e := stageRestore(t)
			addWALOnlyRow(t, e.dbPath, "delta")
			n := 0
			bootFault = func(string) error {
				if n == i {
					return errSimulatedCrash
				}
				n++
				return nil
			}
			_, err := BootRestore(e.confPath, e.dbPath)
			bootFault = nil
			require.ErrorIs(t, err, errSimulatedCrash)

			boot, err := BootRestore(e.confPath, e.dbPath)
			require.NoError(t, err)
			assert.False(t, boot.Applied())
			assert.Equal(t, []string{"beta", "gamma", "delta"}, serverNames(t, e.dbPath), "old data incl. its WAL")
			conf, _ := os.ReadFile(e.confPath)
			assert.Contains(t, string(conf), "agent_secret_key: changed")
			r, err := readRestoreResult(e.dir)
			require.NoError(t, err)
			assert.Equal(t, "failed", r.Status)
			assert.True(t, r.RolledBack)
			_, err = os.Stat(filepath.Join(e.dir, stateFileName))
			assert.True(t, os.IsNotExist(err))
			_, err = os.Stat(filepath.Join(e.dir, pendingDirName))
			assert.True(t, os.IsNotExist(err), "a failed restore is not retried")
		})
	}
}

// A rollback that dies half-way is finished by the next start.
func TestBootRestoreCrashDuringRollbackKeepsOldData(t *testing.T) {
	ops := countBootOps(t)
	for k := range ops { // rollback has one operation per planned path at most
		t.Run(ops[k], func(t *testing.T) {
			e := stageRestore(t)
			addWALOnlyRow(t, e.dbPath, "delta")
			boot, err := BootRestore(e.confPath, e.dbPath)
			require.NoError(t, err)
			require.True(t, boot.Applied())
			// The restored database created its own WAL before startup failed.
			require.NoError(t, os.WriteFile(e.dbPath+"-wal", []byte("restored wal"), 0600))

			n := 0
			bootFault = func(op string) error {
				if !strings.HasPrefix(op, "rollback ") {
					return nil
				}
				if n == k {
					return errSimulatedCrash
				}
				n++
				return nil
			}
			boot.Rollback(errors.New("listener failed"))
			bootFault = nil

			boot, err = BootRestore(e.confPath, e.dbPath)
			require.NoError(t, err)
			assert.False(t, boot.Applied())
			assert.Equal(t, []string{"beta", "gamma", "delta"}, serverNames(t, e.dbPath))
			conf, _ := os.ReadFile(e.confPath)
			assert.Contains(t, string(conf), "agent_secret_key: changed")
			_, err = os.Stat(filepath.Join(e.dir, stateFileName))
			assert.True(t, os.IsNotExist(err))
		})
	}
}

// A rollback that cannot finish keeps the plan and the kept copies, and
// the next start refuses to run on the mixed data until it can finish.
func TestBootRestoreRollbackFailureIsRetried(t *testing.T) {
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	bootFault = func(op string) error {
		if strings.HasPrefix(op, "rollback ") {
			return errors.New("disk busy")
		}
		return nil
	}
	boot.Rollback(errors.New("listener failed"))
	_, err = BootRestore(e.confPath, e.dbPath)
	require.Error(t, err, "do not start on half-restored data")
	bootFault = nil
	_, err = os.Stat(filepath.Join(e.dir, rollbackDirName, FileDatabase))
	require.NoError(t, err, "the previous database is kept until the rollback succeeds")

	boot, err = BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	assert.Equal(t, []string{"beta", "gamma", "delta"}, serverNames(t, e.dbPath))
}

func crashAt(target string) {
	bootFault = func(op string) error {
		if op == target || strings.HasPrefix(op, target+" ") {
			return errSimulatedCrash
		}
		return nil
	}
}

func assertOldDataAndNoRetry(t *testing.T, e *testEnv) {
	t.Helper()
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	assert.Equal(t, []string{"beta", "gamma", "delta"}, serverNames(t, e.dbPath))
	// One more start: the restore that was rolled back is never applied again.
	boot, err = BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	assert.Equal(t, []string{"beta", "gamma", "delta"}, serverNames(t, e.dbPath))
	_, err = os.Stat(filepath.Join(e.dir, pendingDirName))
	assert.True(t, os.IsNotExist(err))
}

// The pending request is invalidated right after the plan is persisted and
// before any file moves: once old files have been moved, no start can
// treat the request as new again.
func TestBootRestorePendingInvalidatedBeforeFilesMove(t *testing.T) {
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	crashAt("aside") // first file move
	_, err := BootRestore(e.confPath, e.dbPath)
	bootFault = nil
	require.ErrorIs(t, err, errSimulatedCrash)
	_, err = os.Stat(filepath.Join(e.dir, pendingDirName, pendingFileName))
	assert.True(t, os.IsNotExist(err), "the pending request is gone before anything moved")
	assertOldDataAndNoRetry(t, e)
}

// Crash after the plan was written but before the pending request was
// invalidated: the next start invalidates it before rolling back.
func TestBootRestoreCrashBeforePendingInvalidated(t *testing.T) {
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	crashAt("invalidate pending")
	_, err := BootRestore(e.confPath, e.dbPath)
	bootFault = nil
	require.ErrorIs(t, err, errSimulatedCrash)
	_, err = os.Stat(filepath.Join(e.dir, pendingDirName, pendingFileName))
	require.NoError(t, err, "test setup: the request is still pending")
	assertOldDataAndNoRetry(t, e)
}

// A crash after the rollback restored the files but before the plan was
// removed: the next start repeats the (idempotent) rollback and the request
// stays invalid. This is the interruption between rollback and cleanup.
func TestBootRestoreCrashBeforePlanRemoved(t *testing.T) {
	for _, first := range []string{"aside", "install"} {
		t.Run(first, func(t *testing.T) {
			e := stageRestore(t)
			addWALOnlyRow(t, e.dbPath, "delta")
			crashAt(first)
			_, err := BootRestore(e.confPath, e.dbPath)
			require.ErrorIs(t, err, errSimulatedCrash)
			crashAt("rollback finish")
			_, err = BootRestore(e.confPath, e.dbPath)
			bootFault = nil
			require.ErrorIs(t, err, errSimulatedCrash)
			_, err = os.Stat(filepath.Join(e.dir, stateFileName))
			require.NoError(t, err, "test setup: the plan is still there")
			assertOldDataAndNoRetry(t, e)
		})
	}
}

// An unreadable kept copy is not mistaken for a missing one: the rollback
// keeps the plan and the copies, the dashboard refuses to start, and a
// later start finishes the rollback.
func TestBootRestoreRollbackKeepsPlanOnPermissionError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	rollbackDir := filepath.Join(e.dir, rollbackDirName)
	require.NoError(t, os.Chmod(rollbackDir, 0))
	boot.Rollback(errors.New("listener failed"))
	_, err = BootRestore(e.confPath, e.dbPath)
	require.NoError(t, os.Chmod(rollbackDir, 0700))
	require.Error(t, err, "do not start while the previous data cannot be checked")
	_, err = os.Stat(filepath.Join(e.dir, stateFileName))
	require.NoError(t, err, "the plan is kept for a retry")
	assertOldDataAndNoRetry(t, e)
}

// The commit cannot be recorded: Commit fails (the dashboard does not
// start) and the next start rolls the restore back.
func TestBootRestoreCommitFailureKeepsPlan(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(e.dir, 0500))
	err = boot.Commit()
	require.NoError(t, os.Chmod(e.dir, 0700))
	require.Error(t, err)
	assert.True(t, boot.Applied())
	assertOldDataAndNoRetry(t, e)
}

// Invalidating the pending request fails for real (permissions): nothing
// was moved, the plan is kept and the dashboard does not start; the next
// start invalidates the request first and only then rolls back, so the
// request is never replayed.
func TestBootRestoreKeepsPlanWhenPendingCannotBeInvalidated(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	pendingDir := filepath.Join(e.dir, pendingDirName)
	require.NoError(t, os.Chmod(pendingDir, 0500))
	_, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, os.Chmod(pendingDir, 0700))
	require.ErrorContains(t, err, "cannot invalidate the pending restore")
	require.ErrorIs(t, err, os.ErrPermission)
	_, err = os.Stat(filepath.Join(e.dir, stateFileName))
	require.NoError(t, err, "the plan is kept")
	_, err = os.Stat(filepath.Join(pendingDir, pendingFileName))
	require.NoError(t, err, "the request is still there")
	entries, _ := os.ReadDir(filepath.Join(e.dir, rollbackDirName))
	assert.Empty(t, entries, "nothing was moved")
	assertOldDataAndNoRetry(t, e)
}

// failSync makes the n-th directory sync after the operation `after` fail
// with an I/O error.
func failSync(after string) *bool {
	seen := after == ""
	failed := new(bool)
	bootFault = func(op string) error {
		if !seen && (op == after || strings.HasPrefix(op, after+" ")) {
			seen = true
			return nil
		}
		if seen && !*failed && strings.HasPrefix(op, "sync ") {
			*failed = true
			return errors.New("injected I/O error")
		}
		return nil
	}
	return failed
}

// The plan cannot be confirmed on disk: no file is moved and the dashboard
// does not start.
func TestBootRestoreRefusesWhenPlanSyncFails(t *testing.T) {
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	failed := failSync("")
	var moved bool
	inner := bootFault
	bootFault = func(op string) error {
		if strings.HasPrefix(op, "aside ") || strings.HasPrefix(op, "install ") {
			moved = true
		}
		return inner(op)
	}
	_, err := BootRestore(e.confPath, e.dbPath)
	bootFault = nil
	require.True(t, *failed)
	require.ErrorContains(t, err, "injected I/O error")
	assert.False(t, moved, "no file may move before the plan is on disk")
	assertOldDataAndNoRetry(t, e)
}

// The swapped entries cannot be confirmed on disk: the plan stays and the
// dashboard does not start; the next start rolls back.
func TestBootRestoreRefusesWhenSwapSyncFails(t *testing.T) {
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	installs := 0
	for _, op := range countBootOps(t) {
		if strings.HasPrefix(op, "install ") {
			installs++
		}
	}
	require.Positive(t, installs)
	failed := new(bool)
	seen := 0
	bootFault = func(op string) error {
		if strings.HasPrefix(op, "install ") {
			seen++
		}
		if seen == installs && !*failed && strings.HasPrefix(op, "sync ") {
			*failed = true
			return errors.New("injected I/O error")
		}
		return nil
	}
	_, err := BootRestore(e.confPath, e.dbPath)
	bootFault = nil
	require.True(t, *failed)
	require.ErrorContains(t, err, "cannot persist the swapped files")
	_, err = os.Stat(filepath.Join(e.dir, stateFileName))
	require.NoError(t, err, "the plan is kept")
	assertOldDataAndNoRetry(t, e)
}

// restartRollsBack simulates the supervisor restarting the dashboard and
// checks what that start finds on disk: the previous database (including
// its WAL data), the previous configuration, a rolled-back result, and no
// replay later.
func restartRollsBack(t *testing.T, e *testEnv) {
	t.Helper()
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	assert.Equal(t, []string{"beta", "gamma", "delta"}, serverNames(t, e.dbPath), "previous database incl. WAL data")
	conf, err := os.ReadFile(e.confPath)
	require.NoError(t, err)
	assert.Contains(t, string(conf), "agent_secret_key: changed", "previous configuration")
	r, err := readRestoreResult(e.dir)
	require.NoError(t, err)
	assert.Equal(t, "failed", r.Status)
	assert.True(t, r.RolledBack)
	assertOldDataAndNoRetry(t, e)
}

// restartKeepsRestore checks that the next start finishes a commit: the
// restored data and configuration stay, the result is a success and the
// plan, the commit record and the previous copies are gone.
func restartKeepsRestore(t *testing.T, e *testEnv) {
	t.Helper()
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	assert.Equal(t, []string{"alpha", "beta"}, serverNames(t, e.dbPath))
	conf, err := os.ReadFile(e.confPath)
	require.NoError(t, err)
	assert.Contains(t, string(conf), "agent_secret_key: original")
	r, err := readRestoreResult(e.dir)
	require.NoError(t, err)
	assert.Equal(t, "success", r.Status)
	assert.Equal(t, e.restoreName, r.Name, "the status shows this restore, not an earlier one")
	assert.Equal(t, e.restoreSHA, r.SHA256)
	for _, name := range []string{stateFileName, commitFileName, rollbackDirName} {
		_, err := os.Stat(filepath.Join(e.dir, name))
		assert.True(t, os.IsNotExist(err), name)
	}
}

func swappedEnv(t *testing.T) (*testEnv, *Boot) {
	t.Helper()
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	require.True(t, boot.Applied())
	require.Equal(t, []string{"alpha", "beta"}, serverNames(t, e.dbPath), "restored data in place")
	e.restoreName, e.restoreSHA = boot.state.Name, boot.state.SHA256
	// An earlier restore's result is on disk; it must be replaced.
	writeRestoreResult(e.dir, RestoreResult{Status: "failed", Name: "nezha-backup-20200101-000000-manual-000000", Error: "earlier"})
	return e, boot
}

// The commit record cannot be written at all (fails before the rename):
// Commit fails, and the restart, which finds the plan without a record,
// rolls back.
func TestBootRestoreCommitRecordWriteFails(t *testing.T) {
	e, boot := swappedEnv(t)
	bootFault = func(op string) error {
		if strings.HasPrefix(op, "write ") && strings.HasSuffix(op, commitFileName) {
			return errors.New("injected I/O error")
		}
		return nil
	}
	err := boot.Commit()
	bootFault = nil
	require.ErrorContains(t, err, "injected I/O error")
	_, err = os.Stat(filepath.Join(e.dir, commitFileName))
	require.True(t, os.IsNotExist(err))
	restartRollsBack(t, e)
}

// The record was renamed into place but its directory sync failed: Commit
// fails and withdraws the record, so the restart rolls back.
func TestBootRestoreCommitRecordSyncFailsIsWithdrawn(t *testing.T) {
	e, boot := swappedEnv(t)
	failed := failSync("write")
	err := boot.Commit()
	bootFault = nil
	require.True(t, *failed)
	require.ErrorContains(t, err, "injected I/O error")
	restartRollsBack(t, e)
}

// Same, but the record cannot be withdrawn either: the restart finds the
// record, confirms it on disk and finishes the commit (the dashboard had not
// served anything, so the restored data is complete and consistent). If the
// record could not be confirmed on disk, the start would refuse to run.
func TestBootRestoreCommitRecordUnconfirmedAndNotWithdrawn(t *testing.T) {
	e, boot := swappedEnv(t)
	seenWrite := false
	bootFault = func(op string) error {
		switch {
		case strings.HasPrefix(op, "write ") && strings.HasSuffix(op, commitFileName):
			seenWrite = true
		case seenWrite && strings.HasPrefix(op, "sync "):
			seenWrite = false
			return errors.New("injected I/O error")
		case op == "withdraw commit":
			return errors.New("injected I/O error")
		}
		return nil
	}
	err := boot.Commit()
	require.ErrorContains(t, err, "injected I/O error")

	// While the record cannot be confirmed on disk, no start proceeds.
	bootFault = func(op string) error {
		if strings.HasPrefix(op, "sync ") {
			return errors.New("injected I/O error")
		}
		return nil
	}
	_, err = BootRestore(e.confPath, e.dbPath)
	bootFault = nil
	require.ErrorContains(t, err, "cannot confirm the commit record")
	_, err = os.Stat(filepath.Join(e.dir, rollbackDirName, FileDatabase))
	require.NoError(t, err, "the previous copies are kept")

	restartKeepsRestore(t, e)
}

// The process dies after the commit record is on disk, before the plan is
// removed (the interruption right around the removal): the restart
// finishes the commit. Dying after the plan was removed but before the
// record was: the restart removes the stale record and keeps the data.
func TestBootRestoreCrashDuringCommitCleanup(t *testing.T) {
	for _, op := range []string{"commit cleanup", "plan cleanup", "commit record cleanup"} {
		t.Run(op, func(t *testing.T) {
			e, boot := swappedEnv(t)
			crashAt(op)
			err := boot.Commit()
			bootFault = nil
			require.ErrorIs(t, err, errSimulatedCrash)
			restartKeepsRestore(t, e)
		})
	}
}

// A crash before the commit record exists, and the plan was never removed:
// the restart rolls back.
func TestBootRestoreCrashBeforeCommitRecord(t *testing.T) {
	e, boot := swappedEnv(t)
	crashAt("commit")
	require.ErrorIs(t, boot.Commit(), errSimulatedCrash)
	bootFault = nil
	restartRollsBack(t, e)
}

// The plan is removed only after the commit record: at no point does the
// disk hold neither, so a missing plan never stands for an unknown commit.
func TestBootRestorePlanRemovedOnlyAfterRecord(t *testing.T) {
	e, boot := swappedEnv(t)
	var order []string
	bootFault = func(op string) error {
		if strings.HasPrefix(op, "write ") || strings.HasSuffix(op, "cleanup") {
			order = append(order, op)
		}
		return nil
	}
	require.NoError(t, boot.Commit())
	bootFault = nil
	// record on disk, then the result on disk, then the plan, then the record
	require.Len(t, order, 5, "%v", order)
	assert.True(t, strings.HasSuffix(order[0], commitFileName))
	assert.Equal(t, "commit cleanup", order[1])
	assert.True(t, strings.HasSuffix(order[2], resultFileName))
	assert.Equal(t, "plan cleanup", order[3])
	assert.Equal(t, "commit record cleanup", order[4])
	restartKeepsRestore(t, e)
}

// The rolled-back entries cannot be confirmed on disk: the plan stays, the
// dashboard does not start, and a later start finishes the rollback.
func TestBootRestoreRollbackSyncFailureKeepsPlan(t *testing.T) {
	e := stageRestore(t)
	addWALOnlyRow(t, e.dbPath, "delta")
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	failed := failSync("rollback")
	boot.Rollback(errors.New("listener failed"))
	bootFault = nil
	require.True(t, *failed)
	_, err = os.Stat(filepath.Join(e.dir, stateFileName))
	require.NoError(t, err, "the plan is kept")
	assertOldDataAndNoRetry(t, e)
}

// A crash before the plan file exists: nothing was moved and the request is
// still pending, so the next start simply applies the restore.
func TestBootRestoreCrashBeforePlanWrittenRetries(t *testing.T) {
	e := stageRestore(t)
	crashAt("write")
	_, err := BootRestore(e.confPath, e.dbPath)
	bootFault = nil
	require.ErrorIs(t, err, errSimulatedCrash)
	_, err = os.Stat(filepath.Join(e.dir, stateFileName))
	require.True(t, os.IsNotExist(err))
	assert.Equal(t, []string{"beta", "gamma"}, serverNames(t, e.dbPath), "nothing moved")
	boot, err := BootRestore(e.confPath, e.dbPath)
	require.NoError(t, err)
	assert.True(t, boot.Applied())
	assert.Equal(t, []string{"alpha", "beta"}, serverNames(t, e.dbPath))
}

// The result cannot be written while the commit is finished: Commit still
// succeeds (the restore is committed and the dashboard may serve it), the
// plan and the record stay, and the next start publishes this restore's
// result without rolling the data back.
func TestBootRestoreResultWriteFailsIsPublishedOnRestart(t *testing.T) {
	e, boot := swappedEnv(t)
	bootFault = func(op string) error {
		if strings.HasPrefix(op, "write ") && strings.HasSuffix(op, resultFileName) {
			return errors.New("injected I/O error")
		}
		return nil
	}
	require.NoError(t, boot.Commit())
	bootFault = nil
	for _, name := range []string{stateFileName, commitFileName} {
		_, err := os.Stat(filepath.Join(e.dir, name))
		require.NoError(t, err, name)
	}
	r, err := readRestoreResult(e.dir)
	require.NoError(t, err)
	assert.Equal(t, "earlier", r.Error, "not published yet")
	restartKeepsRestore(t, e)
}

// A commit record whose plan is already gone (crash right after the plan
// was removed): the restart publishes the record's result before removing
// it, so the status shows this restore.
func TestBootRestoreLeftoverRecordPublishesResult(t *testing.T) {
	e, boot := swappedEnv(t)
	bootFault = func(op string) error {
		if op == "commit record cleanup" {
			return errSimulatedCrash
		}
		if strings.HasPrefix(op, "write ") && strings.HasSuffix(op, resultFileName) {
			return errors.New("injected I/O error") // pretend it never reached the disk
		}
		return nil
	}
	err := boot.Commit()
	bootFault = nil
	// The failed result write stops before the plan removal; finish it by
	// hand to model "plan removed, result missing, record left".
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(e.dir, stateFileName)))
	r, _ := readRestoreResult(e.dir)
	require.Equal(t, "earlier", r.Error)
	restartKeepsRestore(t, e)
}
