package singleton

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

func requirePrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Zerof(t, info.Mode().Perm()&0077, "%s has mode %04o", path, info.Mode().Perm())
}

// The database holds password hashes, agent secrets and session ids. SQLite
// would create it 0644, and data/ is not always a directory the Dashboard
// created itself (volume, bind mount), so the file has to be private on its own.
func TestInitDBFromPathCreatesPrivateDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not available on windows")
	}

	originalConf, originalDB := Conf, DB
	Conf = &ConfigClass{Config: &model.Config{}}
	t.Cleanup(func() {
		if DB != nil && DB != originalDB {
			if sqlDB, err := DB.DB(); err == nil {
				sqlDB.Close()
			}
		}
		Conf, DB = originalConf, originalDB
	})

	path := filepath.Join(t.TempDir(), "sqlite.db")
	require.NoError(t, InitDBFromPath(path))
	require.NoError(t, DB.Create(&model.User{Username: "admin", Password: "hash"}).Error)
	requirePrivateFile(t, path)
}

func TestRestrictSQLiteFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not available on windows")
	}

	worldReadable := func(t *testing.T, path string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, nil, 0600))
		// chmod is not subject to the umask of the test process
		require.NoError(t, os.Chmod(path, 0644))
	}

	t.Run("ExistingDatabaseAndSidecars", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sqlite.db")
		for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
			worldReadable(t, path+suffix)
		}
		require.NoError(t, restrictSQLiteFileMode(path))
		for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
			requirePrivateFile(t, path+suffix)
		}
	})

	t.Run("DSNOptions", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sqlite.db")
		worldReadable(t, path)
		require.NoError(t, restrictSQLiteFileMode(path+"?_busy_timeout=5000"))
		requirePrivateFile(t, path)
		require.NoFileExists(t, path+"?_busy_timeout=5000")
	})

	t.Run("NoDatabaseFile", func(t *testing.T) {
		t.Chdir(t.TempDir())
		for _, dsn := range []string{"", ":memory:", ":memory:?cache=shared", "file:memdb?mode=memory&cache=shared"} {
			require.NoError(t, restrictSQLiteFileMode(dsn))
		}
		entries, err := os.ReadDir(".")
		require.NoError(t, err)
		require.Empty(t, entries)
	})

	t.Run("MissingDirectory", func(t *testing.T) {
		require.Error(t, restrictSQLiteFileMode(filepath.Join(t.TempDir(), "missing", "sqlite.db")))
	})
}
