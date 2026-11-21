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
