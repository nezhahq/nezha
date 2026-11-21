package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
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

	boot.Commit()
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
