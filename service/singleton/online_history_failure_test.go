package singleton

import (
	"errors"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOnlineHistoryRetriesFailedHeartbeatImmediately(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()
	start := time.Now().UTC().Truncate(time.Second)
	r.RecordOnline(1, start)
	id := r.open[1].id
	require.NoError(t, DB.Exec(`CREATE TRIGGER reject_heartbeat BEFORE UPDATE OF last_active_at ON online_sessions BEGIN SELECT RAISE(ABORT, 'heartbeat failure'); END`).Error)
	r.RecordOnline(1, start.Add(30*time.Second))
	require.NoError(t, DB.Exec("DROP TRIGGER reject_heartbeat").Error)
	r.RecordOnline(1, start.Add(31*time.Second))
	require.True(t, getOnlineSession(t, id).LastActiveAt.Equal(start.Add(31*time.Second)))
}

func TestOnlineHistoryRetriesStartupReadBeforeNewReport(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	old := model.OnlineSession{ServerID: 1, StartedAt: start, LastActiveAt: start, Ongoing: true}
	require.NoError(t, DB.Create(&old).Error)
	require.NoError(t, DB.Callback().Query().Before("gorm:query").Register("reject_history_restore", func(db *gorm.DB) { db.AddError(errors.New("restore unavailable")) }))
	r := NewOnlineHistoryRecorder()
	require.NoError(t, DB.Callback().Query().Remove("reject_history_restore"))
	r.RecordOnline(1, time.Now())
	stored := getOnlineSession(t, old.ID)
	require.False(t, stored.Ongoing)
	require.True(t, stored.EndedAt.Equal(start))
	require.NotEqual(t, old.ID, r.open[1].id)
	require.Equal(t, int64(1), countOnlineSessions(t, "ongoing = ?", true))
}

func TestOnlineHistorySweepKeepsSessionReconnectedDuringObservation(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()
	start := time.Now().Add(-time.Hour)
	r.RecordOnline(1, start)
	observed := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.SweepStale(time.Now(), func(uint64) (time.Time, bool) { close(observed); <-release; return start, true })
	}()
	<-observed
	reconnected := make(chan struct{})
	go func() { r.RecordOffline(1, start.Add(time.Second)); r.RecordOnline(1, time.Now()); close(reconnected) }()
	// The observer must not hold the recorder lock: RPC updates take the runtime
	// lock first and then enter the recorder. Holding both in reverse order deadlocks.
	select {
	case <-reconnected:
	case <-time.After(time.Second):
		close(release)
		<-done
		<-reconnected
		t.Fatal("sweep holds recorder lock while consulting runtime state")
	}
	id := r.open[1].id
	close(release)
	<-done
	require.Contains(t, r.open, uint64(1))
	require.Equal(t, id, r.open[1].id)
	require.True(t, getOnlineSession(t, id).Ongoing)
}

func TestOnlineHistoryQueryNormalizesOffsetsAndUsesWindowIndexes(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	east := time.FixedZone("UTC+8", 8*3600)
	start := time.Date(2025, 1, 1, 12, 0, 0, 0, east)
	r := NewOnlineHistoryRecorder()
	r.RecordOnline(1, start)
	r.RecordOffline(1, start.Add(time.Hour))
	rows, err := r.Sessions(1, start.UTC().Add(-time.Minute), start.UTC().Add(2*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.True(t, rows[0].StartedAt.Equal(start))
	require.True(t, rows[0].EndedAt.Equal(start.Add(time.Hour)))
	for _, test := range []struct {
		query, index string
		args         []any
	}{
		{"EXPLAIN QUERY PLAN SELECT * FROM online_sessions WHERE server_id = ? AND ongoing = ? AND ended_at > ?", "idx_online_sessions_server_closed", []any{1, false, start.UTC()}},
		{"EXPLAIN QUERY PLAN SELECT * FROM online_sessions WHERE server_id = ? AND ongoing = ? AND started_at < ?", "idx_online_sessions_server_open", []any{1, true, start.Add(2 * time.Hour).UTC()}},
	} {
		var plan []struct{ Detail string }
		require.NoError(t, DB.Raw(test.query, test.args...).Scan(&plan).Error)
		require.NotEmpty(t, plan)
		require.Contains(t, plan[0].Detail, test.index)
	}
}

func TestOnlineHistorySweepKeepsReportReceivedDuringObservation(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()
	start := time.Now().Add(-time.Hour)
	r.RecordOnline(1, start)
	id := r.open[1].id
	r.SweepStale(time.Now(), func(uint64) (time.Time, bool) {
		r.RecordOnline(1, time.Now())
		return start, true
	})
	require.Contains(t, r.open, uint64(1))
	// A report after an hour starts a new session; the stale sweep must not close it.
	require.NotEqual(t, id, r.open[1].id)
	require.False(t, getOnlineSession(t, id).Ongoing)
	require.True(t, getOnlineSession(t, r.open[1].id).Ongoing)
}

func TestOnlineHistoryStartupReadFailureBlocksQueryAndSweepRetries(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	start := time.Now().Add(-time.Hour)
	old := model.OnlineSession{ServerID: 1, StartedAt: start, LastActiveAt: start, Ongoing: true}
	require.NoError(t, DB.Create(&old).Error)
	require.NoError(t, DB.Callback().Query().Before("gorm:query").Register("reject_history_restore", func(db *gorm.DB) { db.AddError(errors.New("restore unavailable")) }))
	r := NewOnlineHistoryRecorder()
	_, err := r.Sessions(1, start, time.Now())
	require.ErrorContains(t, err, "recovery unavailable")
	r.RecordOnline(1, time.Now())
	require.Empty(t, r.open)
	require.NoError(t, DB.Callback().Query().Remove("reject_history_restore"))
	r.SweepStale(time.Now(), func(uint64) (time.Time, bool) { return time.Time{}, false })
	require.False(t, getOnlineSession(t, old.ID).Ongoing)
	require.Equal(t, int64(0), countOnlineSessions(t, "ongoing = ?", true))
}

func TestOnlineHistoryDisconnectRetriesStartupRead(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	start := time.Now().Add(-10 * time.Second)
	old := model.OnlineSession{ServerID: 1, StartedAt: start, LastActiveAt: start, Ongoing: true}
	require.NoError(t, DB.Create(&old).Error)
	require.NoError(t, DB.Callback().Query().Before("gorm:query").Register("reject_history_restore", func(db *gorm.DB) { db.AddError(errors.New("restore unavailable")) }))
	r := NewOnlineHistoryRecorder()
	require.NoError(t, DB.Callback().Query().Remove("reject_history_restore"))
	end := start.Add(time.Second)
	r.RecordOffline(1, end)
	require.False(t, getOnlineSession(t, old.ID).Ongoing)
	require.True(t, getOnlineSession(t, old.ID).EndedAt.Equal(end))
	r.RecordOnline(1, time.Now())
	require.NotEqual(t, old.ID, r.open[1].id)
}

func TestOnlineHistoryReportAfterTimeoutSplitsBeforeSweep(t *testing.T) {
	for _, failClose := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted", true: "retry_close"}[failClose], func(t *testing.T) {
			setupOnlineHistoryTestDB(t)
			r := NewOnlineHistoryRecorder()
			start := time.Now().UTC().Truncate(time.Second)
			r.RecordOnline(1, start)
			r.RecordOnline(1, start.Add(time.Second))
			first := r.open[1].id
			r.SweepStale(start.Add(30*time.Second), func(uint64) (time.Time, bool) { return start.Add(time.Second), true })
			require.Equal(t, first, r.open[1].id)
			if failClose {
				injectCloseUpdateFailure(t)
			}
			r.RecordOnline(1, start.Add(45*time.Second))
			require.NotEqual(t, first, r.open[1].id)
			second := r.open[1].id
			rows, err := r.Sessions(1, start, start.Add(46*time.Second))
			require.NoError(t, err)
			require.Len(t, rows, 2)
			require.False(t, rows[0].Ongoing)
			require.True(t, rows[0].EndedAt.Equal(start.Add(time.Second)))
			require.True(t, rows[1].StartedAt.Equal(start.Add(45*time.Second)))
			if failClose {
				require.Len(t, r.closing[1], 1)
				dropCloseUpdateFailure(t)
			}
			r.SweepStale(start.Add(46*time.Second), func(uint64) (time.Time, bool) { return start.Add(45 * time.Second), true })
			require.Empty(t, r.closing)
			require.False(t, getOnlineSession(t, first).Ongoing)
			require.True(t, getOnlineSession(t, first).EndedAt.Equal(start.Add(time.Second)))
			require.True(t, getOnlineSession(t, second).Ongoing)
		})
	}
}

func TestOnlineHistoryDisconnectSurvivesUnavailableRecovery(t *testing.T) {
	for _, failClose := range []bool{false, true} {
		t.Run(map[bool]string{false: "close_success", true: "close_retry"}[failClose], func(t *testing.T) {
			setupOnlineHistoryTestDB(t)
			start := time.Now().UTC().Add(-10 * time.Second)
			old := model.OnlineSession{ServerID: 1, StartedAt: start, LastActiveAt: start, Ongoing: true}
			require.NoError(t, DB.Create(&old).Error)
			require.NoError(t, DB.Callback().Query().Before("gorm:query").Register("unavailable_disconnect_restore", func(db *gorm.DB) { db.AddError(errors.New("restore unavailable")) }))
			r := NewOnlineHistoryRecorder()
			end := start.Add(time.Second)
			r.RecordOffline(1, end)
			// Later disconnects while recovery is unavailable must not extend the first session.
			r.RecordOffline(1, end.Add(time.Second))
			require.NoError(t, DB.Callback().Query().Remove("unavailable_disconnect_restore"))
			if failClose {
				injectCloseUpdateFailure(t)
			}
			reconnect := start.Add(5 * time.Second)
			r.RecordOnline(1, reconnect)
			require.NotEqual(t, old.ID, r.open[1].id)
			rows, err := r.Sessions(1, start, reconnect.Add(time.Second))
			require.NoError(t, err)
			require.Len(t, rows, 2)
			require.False(t, rows[0].Ongoing)
			require.True(t, rows[0].EndedAt.Equal(end))
			require.True(t, rows[1].StartedAt.Equal(reconnect))
			if failClose {
				require.Len(t, r.closing[1], 1)
				dropCloseUpdateFailure(t)
			}
			r.SweepStale(reconnect.Add(time.Second), func(uint64) (time.Time, bool) { return reconnect, true })
			require.Empty(t, r.closing)
			require.False(t, getOnlineSession(t, old.ID).Ongoing)
			require.True(t, getOnlineSession(t, old.ID).EndedAt.Equal(end))
			require.True(t, getOnlineSession(t, r.open[1].id).Ongoing)
		})
	}
}

// Every agent's state report goes through RecordOnline. A report that has
// nothing to store must not wait for another server's database write.
func TestOnlineHistoryReportDoesNotWaitForAnotherServersWrite(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()
	start := time.Now().UTC().Truncate(time.Second)
	r.RecordOnline(1, start)
	id := r.open[1].id

	writing, release := make(chan struct{}), make(chan struct{})
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register("park_session_insert", func(*gorm.DB) {
		close(writing)
		<-release
	}))
	other := make(chan struct{})
	go func() {
		r.RecordOnline(2, start)
		close(other)
	}()
	<-writing

	due := start.Add(model.OnlineActiveWindow)
	reported := make(chan struct{})
	go func() {
		r.RecordOnline(1, start.Add(time.Second))
		// Due for last_active_at: left to a later report while the writer is busy.
		r.RecordOnline(1, due)
		close(reported)
	}()
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Error("a report of server 1 waited for the session insert of server 2")
	}
	close(release)
	<-other
	<-reported

	require.Equal(t, id, r.open[1].id)
	require.NotNil(t, r.open[2])
	require.True(t, getOnlineSession(t, id).LastActiveAt.Equal(start))
	r.RecordOnline(1, due.Add(time.Second))
	require.True(t, getOnlineSession(t, id).LastActiveAt.Equal(due.Add(time.Second)))
}

// Reports skip the throttled last_active_at write while other servers write.
// The sweep makes up for it, or a restart would end the session at an old time.
func TestOnlineHistorySweepPersistsLastActiveSkippedByReports(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()
	start := time.Now().UTC().Truncate(time.Second)
	r.RecordOnline(1, start)
	id := r.open[1].id
	due := start.Add(model.OnlineActiveWindow)
	r.dbMu.Lock()
	r.RecordOnline(1, due)
	r.dbMu.Unlock()
	require.True(t, getOnlineSession(t, id).LastActiveAt.Equal(start))

	r.SweepStale(due, func(uint64) (time.Time, bool) { return due, true })

	stored := getOnlineSession(t, id)
	require.True(t, stored.Ongoing)
	require.True(t, stored.LastActiveAt.Equal(due))
	require.True(t, r.open[1].lastPersist.Equal(due))
}
