package controller

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// Specs robfig/cron accepts or panics on although they name no run time: a
// field made only of commas parses to an empty bit mask, and searching for its
// next run occupies the scheduler goroutine shared by every job for seconds;
// a TZ= prefix without fields slices out of range inside the parser.
var unschedulableCronSpecs = []string{
	", * * * * *",
	"* , * * * *",
	"* * ,, * * *",
	"* * * , * *",
	"* * * * , *",
	"* * * * * ,",
	"TZ=Europe/London , * * * * *",
	"CRON_TZ=UTC * * * * * ,",
	"TZ=UTC",
	"CRON_TZ=UTC",
}

func TestCronScheduler_RejectsUnschedulableSpec(t *testing.T) {
	setupCoverPATFixture(t)

	form := model.CronForm{
		TaskType:  model.CronTypeCronTask,
		Name:      "valid",
		Scheduler: "TZ=Europe/London 0 0 3 * * *",
		Command:   "true",
		Servers:   []uint64{1},
		Cover:     model.CronCoverIgnoreAll,
	}
	id, err := createCron(ctxAsMemberWithBody(100, form))
	require.NoError(t, err)
	before := len(singleton.CronShared.Entries())

	for _, scheduler := range unschedulableCronSpecs {
		form.Scheduler = scheduler
		_, err := createCron(ctxAsMemberWithBody(100, form))
		require.Error(t, err, scheduler)

		c := ctxAsMemberWithBody(100, form)
		c.AddParam("id", itoa(id))
		_, err = updateCron(c)
		require.Error(t, err, scheduler)
	}

	require.Len(t, singleton.CronShared.Entries(), before, "a refused spec must not reach the shared scheduler")
	var rows []model.Cron
	require.NoError(t, singleton.DB.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, id, rows[0].ID)
	require.Equal(t, "TZ=Europe/London 0 0 3 * * *", rows[0].Scheduler)
}

// Rows stored before the check existed, or with a task type the handlers do not
// register, must not reach the scheduler when the Dashboard starts.
func TestCronScheduler_DoesNotLoadUnschedulableSpec(t *testing.T) {
	setupCoverPATFixture(t)

	for _, scheduler := range unschedulableCronSpecs {
		require.NoError(t, singleton.DB.Create(&model.Cron{TaskType: 2, Scheduler: scheduler}).Error)
	}
	valid := model.Cron{Scheduler: "0 0 3 * * *"}
	require.NoError(t, singleton.DB.Create(&valid).Error)

	loaded := singleton.NewCronClass()
	defer loaded.Close()

	require.Len(t, loaded.Entries(), 1)
	for _, cr := range loaded.GetSortedList() {
		_, registered := loaded.Get(cr.ID)
		require.Equal(t, cr.ID == valid.ID, registered, cr.Scheduler)
	}
}
