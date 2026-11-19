package model

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// UnblockIP runs on every successful authentication and almost never has a
// record to remove. It must not need the SQLite write lock then, or every
// authenticated request waits for whoever is writing.
func TestUnblockIPWithoutRecordDoesNotNeedWriteLock(t *testing.T) {
	// A file, so that both connections see the same database; with no busy
	// timeout a statement that needs the write lock fails instead of waiting.
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "waf.db")+"?_busy_timeout=0"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&WAF{}))
	require.NoError(t, BlockIP(db, "192.0.2.1", WAFBlockReasonTypeAgentAuthFail, BlockIDgRPC))
	require.NoError(t, BlockIP(db, "192.0.2.1", WAFBlockReasonTypeBruteForceToken, BlockIDToken))

	writer := db.Begin()
	require.NoError(t, writer.Error)
	require.NoError(t, writer.Create(&WAF{IP: make([]byte, 16), BlockIdentifier: 42}).Error)
	require.NoError(t, UnblockIP(db, "192.0.2.2", BlockIDgRPC), "no record for this address")
	require.NoError(t, UnblockIP(db, "192.0.2.1", BlockIDUnknownUser), "no record for this identifier")
	require.Error(t, UnblockIP(db, "192.0.2.1", BlockIDgRPC), "removing a record needs the write lock")
	require.NoError(t, writer.Rollback().Error)

	// An existing record is still removed, and only the one asked for.
	require.NoError(t, UnblockIP(db, "192.0.2.1", BlockIDgRPC))
	var left []int64
	require.NoError(t, db.Model(&WAF{}).Pluck("block_identifier", &left).Error)
	require.Equal(t, []int64{BlockIDToken}, left)
}
