package model

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/pkg/utils"
)

func newWAFTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "waf.sqlite")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&WAF{}))
	return db
}

func countWAFRecords(t *testing.T, db *gorm.DB, ip string) int64 {
	t.Helper()
	binary, err := utils.IPStringToBinary(ip)
	require.NoError(t, err)
	var rows int64
	require.NoError(t, db.Model(&WAF{}).Where("ip = ?", binary).Count(&rows).Error)
	return rows
}

func TestPruneExpiredBlocksKeepsActiveAndManualBlocks(t *testing.T) {
	db := newWAFTestDB(t)
	now := uint64(time.Now().Unix())
	record := func(ip string, id int64, reason uint8, age, count uint64) WAF {
		binary, err := utils.IPStringToBinary(ip)
		require.NoError(t, err)
		return WAF{IP: binary, BlockIdentifier: id, BlockReason: reason, BlockTimestamp: now - age, Count: count}
	}
	records := []WAF{
		record("192.0.2.1", BlockIDToken, WAFBlockReasonTypeBruteForceToken, 10, 1),             // 3 s block is over
		record("2001:db8::1", BlockIDgRPC, WAFBlockReasonTypeAgentAuthFail, 700, 5),             // 5^4 s block is over
		record("192.0.2.2", BlockIDToken, WAFBlockReasonTypeBruteForceToken, 10, 5),             // 5^4 s block is not over
		record("192.0.2.3", BlockIDToken, WAFBlockReasonTypeBruteForceToken, 100, 1),            // counted together with the next one:
		record("192.0.2.3", BlockIDUnknownUser, WAFBlockReasonTypeLoginFail, 10, 4),             // (1+4)^4 s from here, not over
		record("192.0.2.4", BlockIDManual, WAFBlockReasonTypeManual, 400*24*3600, 99999),        // manual block never ends
		record("192.0.2.4", 1, WAFBlockReasonTypeLoginFail, 400*24*3600, 1),                     // kept with the manual block
		record("192.0.2.5", BlockIDToken, WAFBlockReasonTypeBruteForceToken, 10, ^uint64(0)>>1), // saturates like powAdd
		record("192.0.2.5", BlockIDUnknownUser, WAFBlockReasonTypeLoginFail, 10, ^uint64(0)>>1), // integer SUM would overflow
	}
	require.NoError(t, db.Create(&records).Error)

	require.NoError(t, PruneExpiredBlocks(db))

	for ip, kept := range map[string]int64{
		"192.0.2.1": 0, "2001:db8::1": 0, "192.0.2.2": 1, "192.0.2.3": 2, "192.0.2.4": 2, "192.0.2.5": 2,
	} {
		require.Equal(t, kept, countWAFRecords(t, db, ip), ip)
	}
	// Pruning must not change what CheckIP answers.
	for _, ip := range []string{"192.0.2.1", "2001:db8::1"} {
		require.NoError(t, CheckIP(db, ip), ip)
	}
	for _, ip := range []string{"192.0.2.2", "192.0.2.3", "192.0.2.4"} {
		require.Error(t, CheckIP(db, ip), ip)
	}
}

// One failed credential per source address leaves one record per address; once
// the blocks are over all of them have to go, across batches.
func TestPruneExpiredBlocksDeletesMoreThanOneBatch(t *testing.T) {
	db := newWAFTestDB(t)
	const addresses = 10500
	stale := uint64(time.Now().Unix()) - 10
	records := make([]WAF, 0, addresses)
	for i := range addresses {
		binary, err := utils.IPStringToBinary(fmt.Sprintf("2001:db8:1::%x", i+1))
		require.NoError(t, err)
		records = append(records, WAF{IP: binary, BlockIdentifier: BlockIDToken, BlockReason: WAFBlockReasonTypeBruteForceToken, BlockTimestamp: stale, Count: 1})
	}
	require.NoError(t, db.CreateInBatches(records, 500).Error)
	for range 3 { // 3^4 s from now: still blocked while the test runs
		require.NoError(t, BlockIP(db, "192.0.2.10", WAFBlockReasonTypeLoginFail, BlockIDUnknownUser))
	}

	require.NoError(t, PruneExpiredBlocks(db))

	var rows int64
	require.NoError(t, db.Model(&WAF{}).Count(&rows).Error)
	require.Equal(t, int64(1), rows)
	require.Equal(t, int64(1), countWAFRecords(t, db, "192.0.2.10"))
	require.Error(t, CheckIP(db, "192.0.2.10"))
}
