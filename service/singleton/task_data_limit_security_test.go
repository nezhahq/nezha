package singleton

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
)

func TestServiceSentinelUpdateRejectsOversizedTarget(t *testing.T) {
	ss := &ServiceSentinel{}
	require.Error(t, ss.Update(&model.Service{
		Type:   model.TaskTypeTCPPing,
		Target: strings.Repeat("4", model.MaxServiceTargetBytes+1),
	}), "an oversized target must not be scheduled")
}

func TestServiceSentinelQuarantinesOversizedPersistedTargets(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t)

	// Rows written before the limit existed bypass Service.BeforeSave.
	insert := `INSERT INTO services
		(id, user_id, name, type, target, duration, cover, skip_servers_raw, fail_trigger_tasks_raw, recover_trigger_tasks_raw)
		VALUES (?, 100, ?, ?, ?, 3600, ?, '{}', '[]', '[]')`
	require.NoError(t, DB.Exec(insert, 91, "legacy-oversized", model.TaskTypeTCPPing, strings.Repeat("4", model.MaxServiceTargetBytes+1), model.ServiceCoverAll).Error)
	require.NoError(t, DB.Exec(insert, 92, "largest-allowed", model.TaskTypeTCPPing, strings.Repeat("4", model.MaxServiceTargetBytes), model.ServiceCoverAll).Error)

	require.NoError(t, ss.loadServiceHistory())
	_, oversizedLoaded := ss.Get(91)
	_, allowedLoaded := ss.Get(92)
	require.False(t, oversizedLoaded)
	require.True(t, allowedLoaded)
}

func TestNewCronClassQuarantinesOversizedPersistedCommands(t *testing.T) {
	newServiceMonitorSecurityHarness(t)
	require.NoError(t, DB.AutoMigrate(model.Cron{}))
	originalLocalizer := Localizer
	Localizer = i18n.NewLocalizer("en_US", domain, "translations", i18n.Translations)
	t.Cleanup(func() { Localizer = originalLocalizer })

	for id, taskType := range map[uint64]uint8{
		91: model.CronTypeCronTask,
		92: model.CronTypeTriggerTask,
		93: model.CronTypeCronTask,
		94: model.CronTypeTriggerTask,
	} {
		require.NoError(t, DB.Create(&model.Cron{
			Common:    model.Common{ID: id, UserID: 100},
			TaskType:  taskType,
			Scheduler: "0 0 0 1 1 *",
			Command:   strings.Repeat("#", model.MaxCronCommandBytes),
			Cover:     model.CronCoverAll,
		}).Error)
	}
	// Rows written before the limit existed bypass Cron.BeforeSave.
	require.NoError(t, DB.Model(&model.Cron{}).Where("id IN ?", []uint64{91, 92}).
		UpdateColumn("command", strings.Repeat("#", model.MaxCronCommandBytes+1)).Error)

	crons := NewCronClass()
	t.Cleanup(crons.Close)

	for _, id := range []uint64{91, 92} {
		_, loaded := crons.Get(id)
		require.False(t, loaded, "task %d must be neither scheduled nor triggerable", id)
	}
	for _, id := range []uint64{93, 94} {
		_, loaded := crons.Get(id)
		require.True(t, loaded, "task %d must remain registered", id)
	}
	require.Len(t, crons.Entries(), 1)
}
