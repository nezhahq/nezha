package singleton

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

func setupCleanMonitorHistoryTestDB(t *testing.T) {
	t.Helper()

	previousDB := DB
	t.Cleanup(func() { DB = previousDB })

	var err error
	DB, err = gorm.Open(openSQLiteDialector(filepath.Join(t.TempDir(), "dashboard.sqlite")), &gorm.Config{})
	require.NoError(t, err)
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
