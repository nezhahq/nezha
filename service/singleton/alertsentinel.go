package singleton

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jinzhu/copier"

	"github.com/nezhahq/nezha/model"
)

const (
	_RuleCheckNoData = iota
	_RuleCheckFail
	_RuleCheckPass
)

type NotificationHistory struct {
	Duration time.Duration
	Until    time.Time
}

// 报警规则
var (
	AlertsLock                    sync.RWMutex
	Alerts                        []*model.AlertRule
	alertsStore                   map[uint64]map[uint64][]model.TimedAlertPoint // distinct reports per alert/server
	alertsPrevState               map[uint64]map[uint64]uint8                   // [alert_id][server_id] -> 对应报警规则的上一次报警状态
	alertsLastMetricSeq           map[uint64]map[uint64]uint64                  // last distinct Agent metric report per alert/server
	AlertsCycleTransferStatsStore map[uint64]*model.CycleTransferStats          // [alert_id] -> 对应报警规则的周期流量统计
	alertDeliveryMu               sync.Mutex
	alertDeliveries               map[uint64]map[uint64]*alertDeliveryEntry
	alertEventSequence            atomic.Uint64
)

type alertDeliveryEntry struct {
	cancel chan struct{}
	done   chan struct{}
	sendMu *sync.Mutex
}

type alertDeliveryPhase uint8

const (
	alertDeliveryIncident alertDeliveryPhase = iota
	alertDeliveryRecovery
)

func cancelAlertDeliveries(alertID uint64) {
	alertDeliveryMu.Lock()
	defer alertDeliveryMu.Unlock()
	for _, entry := range alertDeliveries[alertID] {
		close(entry.cancel)
	}
	delete(alertDeliveries, alertID)
}

func cancelAlertDeliveriesForServers(serverIDs []uint64) {
	alertDeliveryMu.Lock()
	defer alertDeliveryMu.Unlock()
	for alertID, byServer := range alertDeliveries {
		for _, serverID := range serverIDs {
			if entry := byServer[serverID]; entry != nil {
				close(entry.cancel)
				delete(byServer, serverID)
			}
		}
		if len(byServer) == 0 {
			delete(alertDeliveries, alertID)
		}
	}
}

func runAlertDelivery(cancel <-chan struct{}, send func() bool, always bool, retryDelay time.Duration) {
	for {
		select {
		case <-cancel:
			return
		default:
		}
		delivered := send()
		if delivered && !always {
			return
		}
		delay := retryDelay
		if delivered {
			delay = firstNotificationDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-cancel:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func startAlertDelivery(alert *model.AlertRule, server *model.Server, message, muteLabel string, phase alertDeliveryPhase) {
	// A mute entry belongs to one transition, not all later incidents of the
	// same phase. Concurrent in-flight sends may finish after a recovery.
	muteLabel = fmt.Sprintf("%s:event-%d", muteLabel, alertEventSequence.Add(1))
	alertDeliveryMu.Lock()
	if alertDeliveries == nil {
		alertDeliveries = make(map[uint64]map[uint64]*alertDeliveryEntry)
	}
	if alertDeliveries[alert.ID] == nil {
		alertDeliveries[alert.ID] = make(map[uint64]*alertDeliveryEntry)
	}
	sendMu := new(sync.Mutex)
	if old := alertDeliveries[alert.ID][server.ID]; old != nil {
		close(old.cancel)
		sendMu = old.sendMu
	}
	entry := &alertDeliveryEntry{cancel: make(chan struct{}), done: make(chan struct{}), sendMu: sendMu}
	alertDeliveries[alert.ID][server.ID] = entry
	alertDeliveryMu.Unlock()

	groupID := alert.NotificationGroupID
	always := phase == alertDeliveryIncident && alert.TriggerMode == model.ModeAlwaysTrigger
	go func() {
		defer close(entry.done)
		runAlertDelivery(entry.cancel, func() bool {
			// Serialize incident/recovery delivery for this alert/server pair.
			// A recovery can cancel a pending incident, but never overtake an
			// already-running network request.
			entry.sendMu.Lock()
			defer entry.sendMu.Unlock()
			select {
			case <-entry.cancel:
				return true
			default:
			}
			return NotificationShared.SendNotification(groupID, message, muteLabel, server)
		}, always, 30*time.Second)
	}()
}

func shouldRunAlertFailTasks(triggerMode uint8, newIncident bool) bool {
	return newIncident || triggerMode == model.ModeAlwaysTrigger
}

// addCycleTransferStatsInfo 向AlertsCycleTransferStatsStore中添加周期流量报警统计信息
func addCycleTransferStatsInfo(alert *model.AlertRule) {
	if alert == nil || !alert.Enabled() || !alert.IsSafeToEvaluate() {
		return
	}
	for _, rule := range alert.Rules {
		if !rule.IsTransferDurationRule() {
			continue
		}
		if AlertsCycleTransferStatsStore[alert.ID] == nil {
			from := rule.GetTransferDurationStart()
			to := rule.GetTransferDurationEnd()
			AlertsCycleTransferStatsStore[alert.ID] = &model.CycleTransferStats{
				Name:       alert.Name,
				From:       from,
				To:         to,
				Max:        uint64(rule.Max),
				Min:        uint64(rule.Min),
				ServerName: make(map[uint64]string),
				Transfer:   make(map[uint64]uint64),
				NextUpdate: make(map[uint64]time.Time),
			}
		}
	}
}

// AlertSentinelStart 报警器启动
func AlertSentinelStart() {
	alertDeliveryMu.Lock()
	for _, byServer := range alertDeliveries {
		for _, entry := range byServer {
			close(entry.cancel)
		}
	}
	alertDeliveries = make(map[uint64]map[uint64]*alertDeliveryEntry)
	alertDeliveryMu.Unlock()
	alertsStore = make(map[uint64]map[uint64][]model.TimedAlertPoint)
	alertsPrevState = make(map[uint64]map[uint64]uint8)
	alertsLastMetricSeq = make(map[uint64]map[uint64]uint64)
	AlertsCycleTransferStatsStore = make(map[uint64]*model.CycleTransferStats)
	AlertsLock.Lock()
	if err := DB.Find(&Alerts).Error; err != nil {
		panic(err)
	}
	for _, alert := range Alerts {
		if alert == nil {
			log.Printf("NEZHA>> Skipping invalid nil alert rule loaded from database")
			continue
		}
		alertsStore[alert.ID] = make(map[uint64][]model.TimedAlertPoint)
		alertsPrevState[alert.ID] = make(map[uint64]uint8)
		alertsLastMetricSeq[alert.ID] = make(map[uint64]uint64)
		if !alert.IsSafeToEvaluate() {
			log.Printf("NEZHA>> Skipping invalid alert rule %d loaded from database", alert.ID)
			continue
		}
		addCycleTransferStatsInfo(alert)
	}
	AlertsLock.Unlock()

	time.Sleep(time.Second * 10)
	lastPrint := time.Now()
	var checkCount uint64
	ticker := time.Tick(3 * time.Second) // 3秒钟检查一次
	for startedAt := range ticker {
		checkStatus()
		checkCount++
		if lastPrint.Before(startedAt.Add(-1 * time.Hour)) {
			if Conf.Debug {
				log.Printf("NEZHA>> Checking alert rules %d times each hour %v %v", checkCount, startedAt, time.Now())
			}
			checkCount = 0
			lastPrint = startedAt
		}
	}
}

func OnRefreshOrAddAlert(alert *model.AlertRule) {
	AlertsLock.Lock()
	defer AlertsLock.Unlock()
	cancelAlertDeliveries(alert.ID)
	delete(alertsStore, alert.ID)
	delete(alertsPrevState, alert.ID)
	delete(alertsLastMetricSeq, alert.ID)
	var isEdit bool
	for i := range Alerts {
		if Alerts[i].ID == alert.ID {
			Alerts[i] = alert
			isEdit = true
		}
	}
	if !isEdit {
		Alerts = append(Alerts, alert)
	}
	alertsStore[alert.ID] = make(map[uint64][]model.TimedAlertPoint)
	alertsPrevState[alert.ID] = make(map[uint64]uint8)
	alertsLastMetricSeq[alert.ID] = make(map[uint64]uint64)
	delete(AlertsCycleTransferStatsStore, alert.ID)
	addCycleTransferStatsInfo(alert)
}

func OnDeleteAlert(id []uint64) {
	AlertsLock.Lock()
	defer AlertsLock.Unlock()
	for _, i := range id {
		cancelAlertDeliveries(i)
		delete(alertsStore, i)
		delete(alertsPrevState, i)
		delete(alertsLastMetricSeq, i)
		currentAlerts := Alerts[:0]
		for _, alert := range Alerts {
			if alert.ID != i {
				currentAlerts = append(currentAlerts, alert)
			}
		}
		Alerts = currentAlerts
		delete(AlertsCycleTransferStatsStore, i)
	}
}

// checkStatus 检查报警规则并发送报警
func checkStatus() {
	AlertsLock.RLock()
	defer AlertsLock.RUnlock()
	m := ServerShared.GetList()

	for _, alert := range Alerts {
		// 跳过未启用
		if alert == nil || !alert.Enabled() || !alert.IsSafeToEvaluate() {
			continue
		}
		for _, server := range m {
			// 监测点
			UserLock.RLock()
			var role model.Role
			if u, ok := UserInfoMap[alert.UserID]; !ok {
				role = model.RoleMember
			} else {
				role = u.Role
			}
			UserLock.RUnlock()
			if alert.UserID != server.GetUserID() && !role.IsAdmin() {
				continue
			}
			checkStatusForServer(alert, server)
		}
	}
}

// checkStatusForServer isolates each alert/server evaluation. Input validation
// and model-level guards handle known malformed states; this recovery boundary
// ensures an unexpected evaluator panic cannot terminate the dashboard process
// or prevent unrelated servers from being checked on the same tick.
func checkStatusForServer(alert *model.AlertRule, server *model.Server) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("NEZHA>> Recovered panic evaluating alert rule %d for server %d: %v", alert.ID, server.ID, recovered)
		}
	}()

	// Offline-only rules deliberately observe elapsed time on every tick. Metric
	// rules, including compound rules, must never count the same Agent report
	// repeatedly or treat a disconnected Agent's retained State as fresh data.
	metricRule := false
	runtime := server.RuntimeSnapshot()
	for _, rule := range alert.Rules {
		if !rule.IsOfflineRule() {
			metricRule = true
			break
		}
	}
	if metricRule {
		if runtime.LastActive.IsZero() || runtime.State == nil || time.Since(runtime.LastActive) > model.AlertSampleMaxAge || time.Since(runtime.LastActive) < 0 {
			alertsStore[alert.ID][server.ID] = nil
			return
		}
		if runtime.ReportSequence == 0 || runtime.ReportSequence == alertsLastMetricSeq[alert.ID][server.ID] {
			return
		}
		alertsLastMetricSeq[alert.ID][server.ID] = runtime.ReportSequence
	}
	point, known := alert.SnapshotStatusWithRuntime(AlertsCycleTransferStatsStore[alert.ID], server, runtime, DB)
	if !known {
		alertsStore[alert.ID][server.ID] = nil
		return
	}
	sampledAt := time.Now()
	if metricRule {
		sampledAt = runtime.LastActive
	}
	alertsStore[alert.ID][server.ID] = append(alertsStore[alert.ID][server.ID], model.TimedAlertPoint{At: sampledAt, Values: point})
	// Bound memory even while coverage is insufficient to decide a state.
	cutoff := time.Now().Add(-alert.TimedRetention())
	samples := alertsStore[alert.ID][server.ID]
	first := 0
	for first < len(samples) && samples[first].At.Before(cutoff) {
		first++
	}
	if first > 0 {
		alertsStore[alert.ID][server.ID] = append([]model.TimedAlertPoint(nil), samples[first:]...)
	}
	// 发送通知，分为触发报警和恢复通知
	known, passed := alert.CheckTimed(alertsStore[alert.ID][server.ID], time.Now())
	if !known {
		return
	}
	// 保存当前服务器状态信息
	curServer := model.Server{}
	copier.Copy(&curServer, server)

	// 本次未通过检查
	if !passed {
		newIncident := alertsPrevState[alert.ID][server.ID] != _RuleCheckFail
		if newIncident {
			alertsPrevState[alert.ID][server.ID] = _RuleCheckFail
			message := fmt.Sprintf("[%s] %s(%s) %s", Localizer.T("Incident"),
				server.Name, IPDesensitize(server.GeoIP.IP.Join()), alert.Name)
			startAlertDelivery(alert, &curServer, message, NotificationMuteLabel.ServerIncident(server.ID, alert.ID), alertDeliveryIncident)
		}
		if shouldRunAlertFailTasks(alert.TriggerMode, newIncident) {
			go CronShared.SendTriggerTasks(alert.FailTriggerTasks, curServer.ID, alert.UserID)
		}
	} else {
		// 本次通过检查但上一次的状态为失败，则发送恢复通知
		if alertsPrevState[alert.ID][server.ID] == _RuleCheckFail {
			message := fmt.Sprintf("[%s] %s(%s) %s", Localizer.T("Resolved"),
				server.Name, IPDesensitize(server.GeoIP.IP.Join()), alert.Name)
			go CronShared.SendTriggerTasks(alert.RecoverTriggerTasks, curServer.ID, alert.UserID)
			startAlertDelivery(alert, &curServer, message, NotificationMuteLabel.ServerIncidentResolved(server.ID, alert.ID), alertDeliveryRecovery)
		}
		alertsPrevState[alert.ID][server.ID] = _RuleCheckPass
	}
}
