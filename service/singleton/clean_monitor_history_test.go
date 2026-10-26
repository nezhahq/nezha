package singleton

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

func setupCleanMonitorHistoryTestDB(t *testing.T) {
	t.Helper()

	previousDB := DB
	var err error
	DB, err = gorm.Open(openSQLiteDialector(filepath.Join(t.TempDir(), "dashboard.sqlite")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		DB = previousDB
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close transfer cleanup test database: %v", err)
		}
	})

	require.NoError(t, DB.AutoMigrate(&model.Server{}, &model.Transfer{}, &model.AlertRule{}))
	require.NoError(t, DB.Exec("INSERT INTO servers (id, name, uuid) VALUES (1, 'server', 'clean-monitor-history-test')").Error)
}

func TestCleanMonitorHistoryWithoutRulesDeletesAllTransfers(t *testing.T) {
	setupCleanMonitorHistoryTestDB(t)
	require.NoError(t, DB.Create(&model.Transfer{ServerID: 1, In: 1}).Error)

	CleanMonitorHistory()

	var count int64
	require.NoError(t, DB.Model(&model.Transfer{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestCleanMonitorHistoryPreservesTransfersWhenAlertRulesCannotBeLoaded(t *testing.T) {
	setupCleanMonitorHistoryTestDB(t)
	require.NoError(t, DB.Create(&model.Transfer{ServerID: 1, In: 1}).Error)
	require.NoError(t, DB.Exec("INSERT INTO alert_rules (id, name, rules_raw, fail_trigger_tasks_raw, recover_trigger_tasks_raw) VALUES (1, 'broken', '{', '[]', '[]')").Error)

	var alerts []model.AlertRule
	require.Error(t, DB.Find(&alerts).Error, "precondition: malformed rules_raw must fail AlertRule.AfterFind")

	CleanMonitorHistory()

	var count int64
	require.NoError(t, DB.Model(&model.Transfer{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestHistoryCleanupCombinesGlobalAndSpecificRetention(t *testing.T) {
	setupCleanMonitorHistoryTestDB(t)
	require.NoError(t, DB.AutoMigrate(&model.MCPAuditLog{}))
	now := time.Now().UTC()
	globalStart, specificStart := now.Add(-48*time.Hour), now.Add(-12*time.Hour)
	rules := []*model.Rule{
		{Type: "transfer_in_cycle", CycleStart: &globalStart, CycleInterval: 1000, CycleUnit: "hour", Cover: model.RuleCoverAll},
		{Type: "transfer_in_cycle", CycleStart: &specificStart, CycleInterval: 1000, CycleUnit: "hour", Cover: model.RuleCoverIgnoreAll, Ignore: map[uint64]bool{1: true}},
	}
	require.NoError(t, DB.Create(&model.AlertRule{Name: "combined retention", Rules: rules}).Error)
	retained := model.Transfer{Common: model.Common{CreatedAt: now.Add(-24 * time.Hour)}, ServerID: 1, In: 1}
	expired := model.Transfer{Common: model.Common{CreatedAt: now.Add(-72 * time.Hour)}, ServerID: 1, In: 2}
	require.NoError(t, DB.Create(&retained).Error)
	require.NoError(t, DB.Create(&expired).Error)
	require.NoError(t, DB.Create(&[]model.MCPAuditLog{{CreatedAt: now.AddDate(0, 0, -91), Tool: "expired-audit"}, {CreatedAt: now, Tool: "retained-audit"}}).Error)
	CleanMonitorHistory()
	var transfers []model.Transfer
	require.NoError(t, DB.Find(&transfers).Error)
	require.Len(t, transfers, 1)
	require.Equal(t, retained.ID, transfers[0].ID)
	var audits []model.MCPAuditLog
	require.NoError(t, DB.Find(&audits).Error)
	require.Len(t, audits, 1)
	require.Equal(t, "retained-audit", audits[0].Tool)
}

func TestHistoryCleanupIgnoresFalseServerSelection(t *testing.T) {
	setupCleanMonitorHistoryTestDB(t)
	require.NoError(t, DB.AutoMigrate(&model.MCPAuditLog{}))
	start := time.Now().UTC().Add(-48 * time.Hour)
	require.NoError(t, DB.Create(&model.AlertRule{Name: "unselected server", Rules: []*model.Rule{{Type: "transfer_in_cycle", CycleStart: &start, CycleInterval: 1000, CycleUnit: "hour", Cover: model.RuleCoverIgnoreAll, Ignore: map[uint64]bool{1: false}}}}).Error)
	require.NoError(t, DB.Create(&model.Transfer{Common: model.Common{CreatedAt: start.Add(time.Hour)}, ServerID: 1, In: 1}).Error)
	CleanMonitorHistory()
	var count int64
	require.NoError(t, DB.Model(&model.Transfer{}).Count(&count).Error)
	require.Zero(t, count, "false selection must not retain unrelated transfer history")
}
