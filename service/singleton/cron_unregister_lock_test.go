package singleton

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

// blockingSchedule keeps the scheduler goroutine busy while it computes the
// next run of a newly added entry, so a Remove cannot finish until release.
type blockingSchedule struct{ computing, release chan struct{} }

func (s blockingSchedule) Next(time.Time) time.Time {
	close(s.computing)
	<-s.release
	return time.Time{}
}

func blockCronScheduler(t *testing.T, c *CronClass) (release func()) {
	t.Helper()
	busy := blockingSchedule{make(chan struct{}), make(chan struct{})}
	c.Schedule(busy, cron.FuncJob(func() {}))
	<-busy.computing
	return func() { close(busy.release) }
}

// awaitWhileUnregistering polls a lookup that needs the lock under test until
// it reports the deletion, which proves the lock was released although the
// scheduler has not unregistered the job yet.
func awaitWhileUnregistering(t *testing.T, deleted func() bool, message string) {
	t.Helper()
	found := make(chan struct{})
	go func() {
		for !deleted() {
			time.Sleep(time.Millisecond)
		}
		close(found)
	}()
	select {
	case <-found:
	case <-time.After(5 * time.Second):
		t.Error(message)
	}
}

func TestCronDeleteDoesNotHoldListLockWhileUnregistering(t *testing.T) {
	c := &CronClass{class: class[uint64, *model.Cron]{list: map[uint64]*model.Cron{}}, Cron: cron.New(cron.WithSeconds())}
	c.Start()
	defer c.Close()

	job, err := c.AddFunc("0 0 3 29 2 *", func() {})
	require.NoError(t, err)
	c.Update(&model.Cron{Common: model.Common{ID: 1}, CronJobID: job})

	release := blockCronScheduler(t, c)
	done := make(chan struct{})
	go func() {
		c.Delete([]uint64{1})
		close(done)
	}()

	awaitWhileUnregistering(t, func() bool {
		_, ok := c.Get(1)
		return !ok
	}, "task lookup blocked behind the scheduler")
	release()
	<-done

	require.False(t, c.Entry(job).Valid(), "the deleted task must still be unregistered")
}

func TestServiceSentinelDeleteDoesNotHoldLocksWhileUnregistering(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "reporter"},
	)
	c := CronShared
	c.Start()
	t.Cleanup(c.Close)

	service := &model.Service{
		Common:      model.Common{ID: 10, UserID: 1},
		Name:        "service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	}
	addServiceMonitorSecurityService(t, ss, service)

	release := blockCronScheduler(t, c)
	done := make(chan struct{})
	go func() {
		ss.Delete([]uint64{service.ID})
		close(done)
	}()

	awaitWhileUnregistering(t, func() bool {
		_, ok := ss.Get(service.ID)
		return !ok
	}, "service lookup blocked behind the scheduler")
	release()
	<-done

	require.False(t, c.Entry(service.CronJobID).Valid(), "the deleted service must still be unregistered")
}

func TestOnUserDeleteDoesNotHoldUserLockWhileUnregistering(t *testing.T) {
	_, cleanup := setupOnUserDeleteFixture(t)
	defer cleanup()

	const user = uint64(100)

	require.NoError(t, DB.AutoMigrate(&model.User{}))
	require.NoError(t, DB.Create(&model.User{
		Common:      model.Common{ID: user},
		Username:    "alice",
		AgentSecret: "alice-secret",
	}).Error)
	UserLock.Lock()
	UserInfoMap[user] = model.UserInfo{Role: model.RoleMember, Username: "alice", AgentSecret: "alice-secret"}
	UserLock.Unlock()

	c := CronShared
	c.Cron = cron.New(cron.WithSeconds())
	c.Start()
	defer c.Close()

	job, err := c.AddFunc("0 0 3 29 2 *", func() {})
	require.NoError(t, err)
	cr := &model.Cron{Common: model.Common{ID: 1, UserID: user}, Name: "task", Scheduler: "0 0 3 29 2 *", CronJobID: job}
	require.NoError(t, DB.Create(cr).Error)
	c.Update(cr)

	release := blockCronScheduler(t, c)
	done := make(chan error, 1)
	go func() {
		done <- OnUserDelete([]uint64{user}, func(format string, args ...any) error {
			return nil
		})
	}()

	awaitWhileUnregistering(t, func() bool {
		_, ok := lookupServerOwner(user)
		return !ok
	}, "user lookup blocked behind the scheduler")
	release()
	require.NoError(t, <-done)

	_, ok := c.Get(cr.ID)
	require.False(t, ok, "the deleted user's task must still be removed from the list")
	require.False(t, c.Entry(job).Valid(), "the deleted user's task must still be unregistered")
}
