package singleton

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

// OnlineHistoryRecorder 维护每台服务器当前进行中的在线会话。会话的打开与
// 关闭由 agent 状态流的建立/断开驱动（service/rpc），并由定时清扫兜底关闭
// 失联（未正常断流）的会话；dashboard 重启时通过数据库中 Ongoing 会话恢复。
type OnlineHistoryRecorder struct {
	// dbMu serializes session transitions together with their database calls,
	// which can wait for a connection, for SQLite's lock or for the disk.
	dbMu sync.Mutex
	// mu guards the fields below. Every state report of every agent takes it, so
	// it is only held across a database call during startup recovery. Changes to
	// the maps additionally need dbMu, which is taken first.
	mu       sync.Mutex
	restored bool
	open     map[uint64]*onlineSessionRuntime
	// closing 是关库失败后的待关闭状态：断线边界已被观察到，会话 ID 与
	// 结束时间在此固定、不可被后续上报覆盖；重试只为旧会话完成持久化，
	// 重连不得把它续接成活跃会话（同服务器可能同时有多条待关闭会话）。
	closing map[uint64][]*onlineSessionClosing
	// Preserve the first disconnect while startup reads are unavailable.
	recoveryDisconnects map[uint64]time.Time
	// lastOpened 是每台服务器最近一次新建会话行的时刻。重连频率由 agent 决定，
	// 据此把新建会话限制为每个在线窗口至多一条，避免反复重连无限写入会话行。
	lastOpened map[uint64]time.Time
}

type onlineSessionRuntime struct {
	id           uint64
	lastObserved time.Time
	revision     uint64
	// lastPersist 是 LastActiveAt 最近一次落库时间，用于节流写库。
	lastPersist time.Time
}

type onlineSessionClosing struct {
	id uint64
	// end 是已观察到的会话结束时刻，重试持久化时原样使用。
	end time.Time
}

var OnlineHistoryShared *OnlineHistoryRecorder

func NewOnlineHistoryRecorder() *OnlineHistoryRecorder {
	r := &OnlineHistoryRecorder{
		open:                make(map[uint64]*onlineSessionRuntime),
		closing:             make(map[uint64][]*onlineSessionClosing),
		recoveryDisconnects: make(map[uint64]time.Time),
		lastOpened:          make(map[uint64]time.Time),
	}
	r.mu.Lock()
	r.restoreOngoingSessions()
	r.mu.Unlock()
	return r
}

// restoreOngoingSessions 每台服务器仅允许最新且仍在在线窗口内的会话续接，
// 其余会话按最后持久化的活跃时间结转。崩溃会丢失内存中的待关闭边界，
// 恢复时采用 LastActiveAt 保守结转，不推测未持久化的断线时间。
func (r *OnlineHistoryRecorder) restoreOngoingSessions() bool {
	if DB == nil {
		return false
	}
	var ongoing []model.OnlineSession
	if err := DB.Where("ongoing = ?", true).
		Order("server_id ASC").Order("started_at DESC").Order("id DESC").Find(&ongoing).Error; err != nil {
		log.Printf("NEZHA>> Failed to load ongoing online sessions: %v", err)
		return false
	}
	now := time.Now()
	seenServers := make(map[uint64]bool)
	for _, s := range ongoing {
		// 关闭失败后重连可留下同服务器多条 Ongoing 行；直接赋值 open 会覆盖
		// 旧 ID，导致其永久脱管。按开始时间（同时间按 ID）选最新行，其余
		// 必须关闭或进入 closing 重试，即使最新行本身已经过期也不能复活旧行。
		duplicate := seenServers[s.ServerID]
		seenServers[s.ServerID] = true
		disconnect, observedDisconnect := r.recoveryDisconnects[s.ServerID]
		observedDisconnect = observedDisconnect && !duplicate && !disconnect.Before(s.StartedAt)
		if observedDisconnect || duplicate || s.LastActiveAt.Before(now.Add(-model.OnlineActiveWindow)) {
			end := s.LastActiveAt
			if observedDisconnect && disconnect.After(end) {
				end = disconnect
			}
			if end.Before(s.StartedAt) {
				end = s.StartedAt
			}
			if err := closeOnlineSessionRow(s.ID, end); err != nil {
				log.Printf("NEZHA>> Failed to close recovered online session %d: %v", s.ID, err)
				// 关库失败不能跳过，否则会留下永不闭合、也无人重试的
				// Ongoing 行；进入待关闭状态交由清扫周期重试。过期或
				// 被更新会话替代的行均不得被重连续接为活跃会话
				r.closing[s.ServerID] = append(r.closing[s.ServerID],
					&onlineSessionClosing{id: s.ID, end: end})
			}
			continue
		}
		r.open[s.ServerID] = &onlineSessionRuntime{id: s.ID, lastPersist: s.LastActiveAt, lastObserved: s.LastActiveAt}
	}
	clear(r.recoveryDisconnects)
	r.restored = true
	return true
}

// flushClosing 尽力重试关闭该服务器的待关闭会话：按入列时固定的结束时间
// 持久化，成功才移除；失败的条目原样保留，结束时间不得被后续上报改写。
// The caller holds dbMu, not mu.
func (r *OnlineHistoryRecorder) flushClosing(serverID uint64) {
	pendings, ok := r.closing[serverID]
	if !ok {
		return
	}
	kept := pendings[:0]
	for _, pc := range pendings {
		if err := closeOnlineSessionRow(pc.id, pc.end); err != nil {
			log.Printf("NEZHA>> Failed to retry closing online session %d: %v", pc.id, err)
			kept = append(kept, pc)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(kept) == 0 {
		delete(r.closing, serverID)
	} else {
		r.closing[serverID] = kept
	}
}

// RecordOnline 记录一次状态上报：无进行中会话时开启新会话（重启遗留且仍在
// 窗口内的 Ongoing 会话直接续接）；已有会话时节流落库 LastActiveAt。
// 每台服务器在一个在线窗口内至多新建一条会话：窗口内再次重连不落库，
// 这段时间保持离线（不并入任何会话），窗口过后的下一次上报照常开启新会话。
func (r *OnlineHistoryRecorder) RecordOnline(serverID uint64, at time.Time) {
	at = at.UTC()
	if r.observe(serverID, at) {
		return
	}
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	// A failed startup read is not an empty history: retry before accepting reports.
	if !r.ensureRestored() {
		return
	}
	r.flushClosing(serverID)

	r.mu.Lock()
	rt := r.open[serverID]
	// A report can arrive after expiry but before the next sweep. Close at the
	// previous observation before overwriting it, preserving failed-close retries.
	expired := rt != nil && at.Sub(rt.lastObserved) > model.OnlineActiveWindow
	persist := false
	if expired {
		delete(r.open, serverID)
	} else if rt != nil {
		rt.lastObserved = at
		rt.revision++
		persist = at.Sub(rt.lastPersist) >= model.OnlineActiveWindow
	}
	r.mu.Unlock()
	if expired {
		r.closeSession(serverID, rt, rt.lastObserved)
	} else if rt != nil {
		if persist {
			r.persistLastActive(rt, at)
		}
		return
	}

	// All ongoing rows are owned by open or closing after successful recovery.

	// The agent decides how often it reconnects; without this bound every
	// reconnect inserts a row that is retained for the whole statistics window.
	// A clock stepped backwards must not keep the server offline.
	if opened, ok := r.lastOpened[serverID]; ok && !at.Before(opened) && at.Sub(opened) < model.OnlineActiveWindow {
		return
	}
	s := model.OnlineSession{ServerID: serverID, StartedAt: at, LastActiveAt: at, Ongoing: true}
	if err := DB.Create(&s).Error; err != nil {
		log.Printf("NEZHA>> Failed to create online session: %v", err)
		return
	}
	// lastOpened is guarded by dbMu, which every writer of it holds.
	r.lastOpened[serverID] = at
	r.mu.Lock()
	r.open[serverID] = &onlineSessionRuntime{id: s.ID, lastPersist: at, lastObserved: at}
	r.mu.Unlock()
}

// observe records a report of an open session in memory and returns false if
// the report needs a session transition. Nearly every report ends here, so it
// never waits for another server's database call: while dbMu is taken the
// throttled last_active_at write is left to a later report or to the sweep.
func (r *OnlineHistoryRecorder) observe(serverID uint64, at time.Time) bool {
	r.mu.Lock()
	rt := r.open[serverID]
	if !r.restored || rt == nil || len(r.closing[serverID]) > 0 || at.Sub(rt.lastObserved) > model.OnlineActiveWindow {
		r.mu.Unlock()
		return false
	}
	rt.lastObserved = at
	rt.revision++
	// dbMu is ordered before mu: it can be tried here, but not waited for.
	persist := at.Sub(rt.lastPersist) >= model.OnlineActiveWindow && r.dbMu.TryLock()
	r.mu.Unlock()
	if persist {
		defer r.dbMu.Unlock()
		r.persistLastActive(rt, at)
	}
	return true
}

// persistLastActive is called with dbMu held, not mu.
func (r *OnlineHistoryRecorder) persistLastActive(rt *onlineSessionRuntime, at time.Time) {
	if err := DB.Model(&model.OnlineSession{}).Where("id = ?", rt.id).Update("last_active_at", at).Error; err != nil {
		log.Printf("NEZHA>> Failed to persist online session last_active_at: %v", err)
		return
	}
	// Only committed writes advance the throttle; failures retry on the next report.
	r.mu.Lock()
	rt.lastPersist = at
	r.mu.Unlock()
}

// ensureRestored retries a failed startup read. The caller holds dbMu, not mu.
func (r *OnlineHistoryRecorder) ensureRestored() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restored || r.restoreOngoingSessions()
}

// RecordOffline 关闭该服务器当前进行中的会话。at 是会话结束时刻：正常断流
// 为当前时间，清扫/结转为最后一次已知活跃时间。
// 关库失败时把会话转入待关闭状态而非保留为活跃条目：会话 ID 与已观察到的
// 结束时刻（at 与 lastPersist 的较大者）就此固定，后续上报不得覆盖；重试
// 只为旧会话完成持久化，重连则开启新会话——断线后的离线间隔不得计作在线。
func (r *OnlineHistoryRecorder) RecordOffline(serverID uint64, at time.Time) {
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	// Disconnect may be the first event after startup storage recovers.
	if !r.ensureRestored() {
		// Retrying SELECT alone loses confirmed offline gaps on a quick reconnect.
		// Keep the first boundary; subsequent disconnected streams cannot extend it.
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.recoveryDisconnects == nil {
			r.recoveryDisconnects = make(map[uint64]time.Time)
		}
		if _, exists := r.recoveryDisconnects[serverID]; !exists {
			r.recoveryDisconnects[serverID] = at.UTC()
		}
		return
	}
	r.mu.Lock()
	rt, ok := r.open[serverID]
	delete(r.open, serverID)
	r.mu.Unlock()
	if ok {
		r.closeSession(serverID, rt, at)
	}
}

// closeSession closes the row of a session that has already left open, so that
// a report arriving meanwhile is not counted towards it but waits for dbMu in
// RecordOnline. The caller holds dbMu, not mu.
func (r *OnlineHistoryRecorder) closeSession(serverID uint64, rt *onlineSessionRuntime, at time.Time) {
	end := at
	if end.Before(rt.lastPersist) {
		end = rt.lastPersist
	}
	if err := closeOnlineSessionRow(rt.id, end); err != nil {
		log.Printf("NEZHA>> Failed to close online session %d: %v", rt.id, err)
		// 结束时刻已观察到，不能因写库失败丢失，也不能把行继续当活跃
		// 会话：转待关闭状态，由 RecordOnline 冲洗与清扫周期重试
		r.mu.Lock()
		r.closing[serverID] = append(r.closing[serverID], &onlineSessionClosing{id: rt.id, end: end})
		r.mu.Unlock()
	}
}

// SweepStale 关闭超过在线窗口仍无状态上报的进行中会话。lastActiveFn 返回
// 服务器的运行时最后活跃时间与是否存在。
func (r *OnlineHistoryRecorder) SweepStale(now time.Time, lastActiveFn func(serverID uint64) (time.Time, bool)) {
	// Never call the runtime observer while holding dbMu or mu: reports acquire the
	// runtime lease before these. Snapshot identities, then revalidate them.
	r.dbMu.Lock()
	if !r.ensureRestored() {
		r.dbMu.Unlock()
		return
	}
	r.mu.Lock()
	candidates := make(map[uint64]onlineSessionRuntime, len(r.open))
	for id, rt := range r.open {
		candidates[id] = *rt
	}
	r.mu.Unlock()
	for id := range r.closing {
		r.flushClosing(id)
	}
	r.persistOverdue(candidates)
	// Entries past the window no longer limit anything (deleted servers included).
	for id, opened := range r.lastOpened {
		if now.Sub(opened) >= model.OnlineActiveWindow {
			delete(r.lastOpened, id)
		}
	}
	r.dbMu.Unlock()
	for id, candidate := range candidates {
		lastActive, ok := lastActiveFn(id)
		// Most sessions are alive: find that out without queueing behind the
		// writes of other servers, and take dbMu only to close a session.
		r.mu.Lock()
		rt, _ := r.staleLocked(id, candidate, now, lastActive, ok)
		r.mu.Unlock()
		if rt == nil {
			continue
		}
		r.dbMu.Lock()
		r.mu.Lock()
		rt, end := r.staleLocked(id, candidate, now, lastActive, ok)
		if rt != nil {
			delete(r.open, id)
		}
		r.mu.Unlock()
		if rt != nil {
			r.closeSession(id, rt, end)
		}
		r.dbMu.Unlock()
	}

}

// staleLocked returns the server's session and its end if it is still the
// snapshotted one, unreported since, and out of the online window. mu is held.
func (r *OnlineHistoryRecorder) staleLocked(id uint64, candidate onlineSessionRuntime, now, lastActive time.Time, ok bool) (*onlineSessionRuntime, time.Time) {
	rt := r.open[id]
	if rt == nil || rt.id != candidate.id || rt.revision != candidate.revision {
		return nil, time.Time{}
	}
	if lastActive.Before(rt.lastObserved) {
		lastActive = rt.lastObserved
	}
	if (!ok || lastActive.Before(now.Add(-model.OnlineActiveWindow))) && rt.lastObserved.Before(now.Add(-model.OnlineActiveWindow)) {
		return rt, lastActive
	}
	return nil, time.Time{}
}

// persistOverdue writes last_active_at for the snapshotted sessions whose
// reports found dbMu taken when the write was due (see observe), in one
// transaction. Without it, sustained writes of other servers would leave the
// column behind without bound, and a restart would close those sessions at a
// long outdated time. The caller holds dbMu, not mu.
func (r *OnlineHistoryRecorder) persistOverdue(sessions map[uint64]onlineSessionRuntime) {
	overdue := make(map[uint64]onlineSessionRuntime)
	for id, rt := range sessions {
		if rt.lastObserved.Sub(rt.lastPersist) >= model.OnlineActiveWindow {
			overdue[id] = rt
		}
	}
	if len(overdue) == 0 {
		return
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		for _, rt := range overdue {
			if err := tx.Model(&model.OnlineSession{}).Where("id = ?", rt.id).Update("last_active_at", rt.lastObserved).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("NEZHA>> Failed to persist online session last_active_at: %v", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, persisted := range overdue {
		if rt := r.open[id]; rt != nil && rt.id == persisted.id {
			rt.lastPersist = persisted.lastObserved
		}
	}
}

// SweepOnlineSessions 是定时清扫入口，接上服务器运行时的最后活跃时间。
func SweepOnlineSessions() {
	if OnlineHistoryShared == nil || ServerShared == nil || DB == nil {
		return
	}
	OnlineHistoryShared.SweepStale(time.Now(), onlineServerLastActive)
}

func onlineServerLastActive(serverID uint64) (time.Time, bool) {
	server, ok := ServerShared.Get(serverID)
	if !ok || server == nil {
		return time.Time{}, false
	}
	lastActive := server.RuntimeSnapshot().LastActive
	return lastActive, !lastActive.IsZero()
}

// PruneOnlineSessions 清理超出统计窗口的已结束会话与已删除服务器的残留记录。
// 时间列直接比较（与写入时的绑定格式同构）以走 ended_at 索引，并用分批
// DELETE 替换单条巨型删除，避免大表上长时间占用 sqlite 写锁、阻塞上报路径。
func PruneOnlineSessions() {
	if DB == nil {
		return
	}
	keepDays := Conf.OnlineHistoryRetentionDays()
	if keepDays < model.MinOnlineStatsDays || keepDays > model.MaxOnlineStatsDays {
		keepDays = model.DefaultOnlineStatsDays
	}
	cutoff := model.OnlineHistoryWindowStart(time.Now(), keepDays, Loc)
	pruneOnlineSessionsBatch("ongoing = ? AND ended_at < ?", false, cutoff)
	pruneOnlineSessionsBatch("server_id NOT IN (SELECT `id` FROM servers)")
}

// pruneOnlineSessionsBatch 按主键分批删除匹配的行，每批之间释放写锁。
func pruneOnlineSessionsBatch(where string, args ...any) {
	const batchSize = 10000
	for {
		var ids []uint64
		if err := DB.Model(&model.OnlineSession{}).Unscoped().
			Where(where, args...).Limit(batchSize).Pluck("id", &ids).Error; err != nil {
			log.Printf("NEZHA>> Failed to select online sessions for pruning: %v", err)
			return
		}
		if len(ids) == 0 {
			return
		}
		if err := DB.Unscoped().Delete(&model.OnlineSession{}, ids).Error; err != nil {
			log.Printf("NEZHA>> Failed to prune online sessions: %v", err)
			return
		}
		if len(ids) < batchSize {
			return
		}
	}
}

func closeOnlineSessionRow(id uint64, end time.Time) error {
	return DB.Model(&model.OnlineSession{}).Where("id = ?", id).Updates(map[string]any{
		"ended_at": end.UTC(),
		"ongoing":  false,
	}).Error
}

// Sessions snapshots rows and volatile close boundaries under the same lock.
// Otherwise a failed close can count confirmed offline time as ongoing, or a
// successful retry can remove the boundary after the reader fetched a stale row.
func (r *OnlineHistoryRecorder) Sessions(serverID uint64, start, end time.Time) ([]model.OnlineSession, error) {
	if r != nil {
		r.dbMu.Lock()
		defer r.dbMu.Unlock()
		if !r.ensureRestored() {
			return nil, fmt.Errorf("online history recovery unavailable")
		}
	}
	var sessions []model.OnlineSession
	// Separate branches use (server_id, ongoing, ended_at) and
	// (server_id, ongoing, started_at), avoiding a scan of retained closed rows.
	for _, ongoing := range []bool{false, true} {
		var rows []model.OnlineSession
		q := DB.Where("server_id = ? AND ongoing = ?", serverID, ongoing)
		if ongoing {
			q = q.Where("started_at < ?", end.UTC())
		} else {
			q = q.Where("ended_at > ?", start.UTC())
		}
		if err := q.Find(&rows).Error; err != nil {
			return nil, err
		}
		sessions = append(sessions, rows...)
	}
	if r != nil {
		endings := make(map[uint64]time.Time)
		for _, pending := range r.closing[serverID] {
			endings[pending.id] = pending.end
		}
		for i := range sessions {
			if end, ok := endings[sessions[i].ID]; ok {
				sessions[i].Ongoing = false
				sessions[i].EndedAt = end
			}
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].StartedAt.Before(sessions[j].StartedAt) })
	return sessions, nil
}
