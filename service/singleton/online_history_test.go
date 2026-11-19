package singleton

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

func setupOnlineHistoryTestDB(t *testing.T) {
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
			t.Errorf("close online history test database: %v", err)
		}
	})

	require.NoError(t, DB.AutoMigrate(&model.Server{}, &model.OnlineSession{}))
	require.NoError(t, DB.Exec("INSERT INTO servers (id, name, uuid) VALUES (1, 'server-1', 'online-history-test-1')").Error)
}

func getOnlineSession(t *testing.T, id uint64) model.OnlineSession {
	t.Helper()
	var s model.OnlineSession
	require.NoError(t, DB.First(&s, id).Error)
	return s
}

func countOnlineSessions(t *testing.T, cond string, args ...any) int64 {
	t.Helper()
	var count int64
	require.NoError(t, DB.Model(&model.OnlineSession{}).Where(cond, args...).Count(&count).Error)
	return count
}

// injectCloseUpdateFailure 用 SQLite 触发器注入"SELECT 正常、仅关闭 UPDATE 失败"：
// 只拦截把 ongoing 置 0 的更新，LastActiveAt 节流写与 INSERT 均不受影响，
// 使后续 RecordOnline/恢复/清扫全部走真实执行路径。
func injectCloseUpdateFailure(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.Exec(`CREATE TRIGGER fail_online_session_close
BEFORE UPDATE ON online_sessions
WHEN NEW.ongoing = 0
BEGIN
    SELECT RAISE(ABORT, 'injected close failure');
END`).Error)
	t.Cleanup(func() { dropCloseUpdateFailure(t) })
}

func dropCloseUpdateFailure(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.Exec("DROP TRIGGER IF EXISTS fail_online_session_close").Error)
}

// 评审回归（P2：关库失败丢边界/重连续接待关闭会话）：RecordOffline 关库失败时
// 必须转入待关闭状态——会话 ID 与已观察到的断线时刻固定、后续上报不得覆盖；
// 重连开启的是新会话而非续接旧行；失败恢复后由清扫周期按固定边界完成持久化，
// 离线间隔不计入在线。时间自单一基准单调推进。
func TestRecordOfflineCloseFailurePreservesBoundary(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	atStart := base
	atLastActive := atStart.Add(model.OnlineActiveWindow) // 恰好达到节流窗口，触发落库
	atOffline := atLastActive.Add(15 * time.Second)       // 观察到的断线时刻
	atReconnect := atOffline.Add(10 * time.Minute)

	r.RecordOnline(1, atStart)
	r.RecordOnline(1, atLastActive)
	require.Len(t, r.open, 1)
	first := getOnlineSession(t, r.open[1].id)
	require.WithinDuration(t, atLastActive, first.LastActiveAt, time.Second)

	// 注入关闭失败：行保持 Ongoing 等待重试，会话转入待关闭而非继续当活跃会话
	injectCloseUpdateFailure(t)
	r.RecordOffline(1, atOffline)
	require.NotContains(t, r.open, uint64(1))
	stuck := getOnlineSession(t, first.ID)
	require.True(t, stuck.Ongoing)
	require.True(t, stuck.EndedAt.IsZero(), "关库失败时旧行必须保持未闭合等待重试")

	// 失败未恢复时 agent 重连：旧行不得被续接，必须开启新会话
	r.RecordOnline(1, atReconnect)
	require.Contains(t, r.open, uint64(1))
	require.NotEqual(t, first.ID, r.open[1].id, "待关闭会话不得被续接为活跃会话")
	afterRetry := getOnlineSession(t, first.ID)
	require.True(t, afterRetry.Ongoing, "失败未恢复时重试仍失败，旧行保持待关闭")
	second := getOnlineSession(t, r.open[1].id)
	require.True(t, second.Ongoing)
	require.WithinDuration(t, atReconnect, second.StartedAt, time.Second)

	// 失败恢复后清扫：旧行按观察到的断线时刻闭合，新会话仍活跃不受影响
	dropCloseUpdateFailure(t)
	r.SweepStale(atReconnect.Add(time.Second), func(uint64) (time.Time, bool) {
		return atReconnect, true
	})

	closed := getOnlineSession(t, first.ID)
	require.False(t, closed.Ongoing)
	require.WithinDuration(t, atOffline, closed.EndedAt, time.Second, "旧行必须按观察到的断线时刻闭合")
	require.WithinDuration(t, atStart, closed.StartedAt, time.Second)
	stillOngoing := getOnlineSession(t, second.ID)
	require.True(t, stillOngoing.Ongoing)

	// 离线间隔不计入在线：两行在 [atStart, atReconnect) 内的在线合计止于断线时刻
	var rows []model.OnlineSession
	require.NoError(t, DB.Where("server_id = ?", 1).Order("id").Find(&rows).Error)
	require.Len(t, rows, 2)
	var online time.Duration
	for _, s := range rows {
		end := s.EndedAt
		if s.Ongoing {
			end = atReconnect
		}
		online += end.Sub(s.StartedAt)
	}
	require.Equal(t, atOffline.Sub(atStart), online, "离线间隔不得计作在线")
}

// 评审回归（P2 启动恢复路径）：重启恢复对超窗会话结转关库失败时，条目必须
// 进入待关闭状态（经真实 NewOnlineHistoryRecorder 恢复路径验证，而非手工
// 构造内存状态）；重连不得续接该行，清扫周期按固定边界时刻完成持久化。
func TestRestoreStaleCloseFailureEntersPendingClose(t *testing.T) {
	setupOnlineHistoryTestDB(t)

	// 预置一条超窗 Ongoing 行（dashboard 停机前遗留），时间自单一基准单调推进
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	staleAt := base.Add(time.Hour)
	atReconnect := base.Add(2 * time.Hour)
	legacy := model.OnlineSession{ServerID: 1, StartedAt: base, LastActiveAt: staleAt, Ongoing: true}
	require.NoError(t, DB.Create(&legacy).Error)

	injectCloseUpdateFailure(t)
	r := NewOnlineHistoryRecorder()

	stuck := getOnlineSession(t, legacy.ID)
	require.True(t, stuck.Ongoing, "恢复结转关库失败时行必须保持 Ongoing 等待重试")

	// 重连：待关闭的超窗会话不得被续接，开启新会话
	r.RecordOnline(1, atReconnect)
	require.Contains(t, r.open, uint64(1))
	require.NotEqual(t, legacy.ID, r.open[1].id, "待关闭会话不得被续接为活跃会话")

	dropCloseUpdateFailure(t)
	r.SweepStale(atReconnect.Add(time.Second), func(uint64) (time.Time, bool) {
		return atReconnect, true
	})

	closed := getOnlineSession(t, legacy.ID)
	require.False(t, closed.Ongoing)
	require.WithinDuration(t, staleAt, closed.EndedAt, time.Second, "旧行必须按恢复时观察到的边界闭合")
	require.Equal(t, int64(2), countOnlineSessions(t, "server_id = ?", 1))
	require.Equal(t, int64(1), countOnlineSessions(t, "server_id = ? AND ongoing = ?", 1, true))
}

func TestRecordOnlineOpensSingleSession(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()

	now := time.Now()
	r.RecordOnline(1, now)
	r.RecordOnline(1, now.Add(time.Second))

	require.Equal(t, int64(1), countOnlineSessions(t, "server_id = ?", 1))
	require.Equal(t, int64(1), countOnlineSessions(t, "server_id = ? AND ongoing = ?", 1, true))
}

func TestRecordOnlinePersistsLastActiveThrottled(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()

	now := time.Now()
	r.RecordOnline(1, now)
	// 窗口内的重复上报不应触发落库
	r.RecordOnline(1, now.Add(5*time.Second))
	s := r.open[1]
	stored := getOnlineSession(t, s.id)
	require.WithinDuration(t, now, stored.LastActiveAt, time.Second)

	// 超过窗口后落库
	r.RecordOnline(1, now.Add(model.OnlineActiveWindow+time.Second))
	stored = getOnlineSession(t, s.id)
	require.WithinDuration(t, now.Add(model.OnlineActiveWindow+time.Second), stored.LastActiveAt, time.Second)
}

func TestRecordOfflineClosesSession(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()

	start := time.Now()
	r.RecordOnline(1, start)
	r.RecordOffline(1, start.Add(10*time.Minute))
	// 重复关闭应为无操作
	r.RecordOffline(1, start.Add(11*time.Minute))

	require.Equal(t, int64(1), countOnlineSessions(t, "server_id = ?", 1))
	require.Equal(t, int64(0), countOnlineSessions(t, "server_id = ? AND ongoing = ?", 1, true))
	var s model.OnlineSession
	require.NoError(t, DB.First(&s).Error)
	require.WithinDuration(t, start.Add(10*time.Minute), s.EndedAt, time.Second)
	require.WithinDuration(t, start, s.StartedAt, time.Second)
}

// The agent decides how often it reconnects; the number of rows it leaves
// behind must not follow that number. Reconnects inside the online window stay
// offline, and the first report after the window opens a session as usual.
func TestRecordOnlineLimitsSessionsOpenedByReconnects(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()

	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := start
	for range 200 {
		r.RecordOnline(1, at)
		at = at.Add(5 * time.Millisecond)
		r.RecordOffline(1, at)
		at = at.Add(5 * time.Millisecond)
	}
	require.Equal(t, int64(1), countOnlineSessions(t, "server_id = ?", 1))
	require.Equal(t, int64(0), countOnlineSessions(t, "server_id = ? AND ongoing = ?", 1, true))
	var first model.OnlineSession
	require.NoError(t, DB.First(&first).Error)
	require.WithinDuration(t, start.Add(5*time.Millisecond), first.EndedAt, time.Millisecond, "被跳过的重连不得延长已结束的会话")

	reopened := start.Add(model.OnlineActiveWindow)
	r.RecordOnline(1, reopened)
	require.Equal(t, int64(2), countOnlineSessions(t, "server_id = ?", 1))
	require.Equal(t, int64(1), countOnlineSessions(t, "server_id = ? AND ongoing = ?", 1, true))

	// 长会话断线后立即重连不受影响：限制按上一次新建时刻计算
	r.RecordOffline(1, reopened.Add(10*time.Minute))
	r.RecordOnline(1, reopened.Add(10*time.Minute+time.Second))
	require.Equal(t, int64(3), countOnlineSessions(t, "server_id = ?", 1))

	// 清扫回收已过窗口的记录
	r.SweepStale(reopened.Add(11*time.Minute), func(uint64) (time.Time, bool) { return reopened.Add(11 * time.Minute), true })
	require.Empty(t, r.lastOpened)
}

func TestRecordOnlineContinuesSessionAfterRestart(t *testing.T) {
	setupOnlineHistoryTestDB(t)

	// 模拟重启前遗留的进行中会话（最后活跃时间在在线窗口内：dashboard 只是短暂重启）
	now := time.Now()
	legacy := model.OnlineSession{ServerID: 1, StartedAt: now.Add(-time.Minute), LastActiveAt: now.Add(-5 * time.Second), Ongoing: true}
	require.NoError(t, DB.Create(&legacy).Error)

	// 新 recorder（构造时从库恢复）：新近会话应保留并续接，不产生新会话
	r := NewOnlineHistoryRecorder()
	r.RecordOnline(1, now)

	require.Equal(t, int64(1), countOnlineSessions(t, "server_id = ?", 1))
	continued := getOnlineSession(t, legacy.ID)
	require.True(t, continued.Ongoing)
	require.WithinDuration(t, now.Add(-time.Minute), continued.StartedAt, time.Second)
}

func TestRestoreOngoingSessionsClosesStaleSessions(t *testing.T) {
	setupOnlineHistoryTestDB(t)

	// 上次活跃时间远早于在线窗口：重启恢复时按最后已知活跃时刻结转
	staleStart := time.Now().Add(-time.Hour)
	stale := model.OnlineSession{ServerID: 1, StartedAt: staleStart, LastActiveAt: staleStart.Add(30 * time.Minute), Ongoing: true}
	require.NoError(t, DB.Create(&stale).Error)

	r := NewOnlineHistoryRecorder()
	r.RecordOnline(1, time.Now())

	closed := getOnlineSession(t, stale.ID)
	require.False(t, closed.Ongoing)
	require.WithinDuration(t, staleStart.Add(30*time.Minute), closed.EndedAt, time.Second)
	// 结转后新的上报开启新会话
	require.Equal(t, int64(1), countOnlineSessions(t, "server_id = ? AND ongoing = ?", 1, true))
}

func TestSweepStaleClosesOnlyStaleSessions(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	r := NewOnlineHistoryRecorder()

	now := time.Now()
	r.RecordOnline(1, now.Add(-time.Hour))
	// 第二台服务器：会话在 DB 但没有 servers 记录（服务器已被删除），手工放进内存 map
	require.NoError(t, DB.Exec("INSERT INTO online_sessions (server_id, started_at, last_active_at, ongoing, created_at, updated_at) VALUES (2, ?, ?, 1, ?, ?)", now.Add(-time.Hour), now.Add(-time.Hour), now, now).Error)
	var second model.OnlineSession
	require.NoError(t, DB.Where("server_id = ?", 2).First(&second).Error)
	r.mu.Lock()
	r.open[2] = &onlineSessionRuntime{id: second.ID, lastPersist: now.Add(-time.Hour)}
	r.mu.Unlock()

	// 服务器 1 活跃，服务器 2 不存在 → 仅关闭服务器 2 的会话
	lastActive := func(serverID uint64) (time.Time, bool) {
		if serverID == 1 {
			return now, true
		}
		return time.Time{}, false
	}
	r.SweepStale(now, lastActive)

	require.Equal(t, int64(1), countOnlineSessions(t, "server_id = ? AND ongoing = ?", 1, true))
	require.Equal(t, int64(0), countOnlineSessions(t, "server_id = ? AND ongoing = ?", 2, true))

	// 服务器 1 失联后也会被关闭，结束时刻为最后已知活跃时间
	r.SweepStale(now.Add(model.OnlineActiveWindow*2), func(uint64) (time.Time, bool) {
		return now, true
	})
	require.Equal(t, int64(0), countOnlineSessions(t, "server_id = ? AND ongoing = ?", 1, true))
	var closedFirst = getOnlineSession(t, func() uint64 {
		var s model.OnlineSession
		require.NoError(t, DB.Where("server_id = ?", 1).First(&s).Error)
		return s.ID
	}())
	require.WithinDuration(t, now, closedFirst.EndedAt, time.Second)
}

func TestPruneOnlineSessions(t *testing.T) {
	setupOnlineHistoryTestDB(t)
	previousConf := Conf
	Conf = &ConfigClass{Config: &model.Config{}}
	Conf.OnlineStatsDays = 7
	t.Cleanup(func() { Conf = previousConf })

	now := time.Now()
	old := now.AddDate(0, 0, -30)
	rows := []model.OnlineSession{
		{ServerID: 1, StartedAt: old, EndedAt: old.Add(time.Hour), LastActiveAt: old.Add(time.Hour)},   // 超窗口 → 清理
		{ServerID: 1, StartedAt: old, EndedAt: now.Add(-time.Hour), LastActiveAt: now.Add(-time.Hour)}, // 跨窗口边界 → 保留
		{ServerID: 1, StartedAt: now.Add(-time.Hour), EndedAt: now, LastActiveAt: now},                 // 窗口内 → 保留
		{ServerID: 1, StartedAt: now, Ongoing: true, LastActiveAt: now},                                // 进行中 → 保留
		{ServerID: 99, StartedAt: now.Add(-time.Hour), EndedAt: now, LastActiveAt: now},                // 服务器已删除 → 清理
	}
	for i := range rows {
		require.NoError(t, DB.Create(&rows[i]).Error)
	}

	PruneOnlineSessions()

	require.Equal(t, int64(3), countOnlineSessions(t, "1 = 1"))
	require.Equal(t, int64(1), countOnlineSessions(t, "ongoing = ?", true))
}
