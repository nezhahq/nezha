package singleton

import (
	"fmt"
	"log"
	"sync"
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
	alertsStore                   map[uint64]map[uint64][][]bool       // [alert_id][server_id] -> [timeTick][ruleId] 时间点对应的rule的检查结果
	alertsPrevState               map[uint64]map[uint64]uint8          // [alert_id][server_id] -> 对应报警规则的上一次报警状态
	AlertsCycleTransferStatsStore map[uint64]*model.CycleTransferStats // [alert_id] -> 对应报警规则的周期流量统计
)

// addCycleTransferStatsInfo 向AlertsCycleTransferStatsStore中添加周期流量报警统计信息
func addCycleTransferStatsInfo(alert *model.AlertRule) {
	if !alert.Enabled() {
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
	alertsStore = make(map[uint64]map[uint64][][]bool)
	alertsPrevState = make(map[uint64]map[uint64]uint8)
	AlertsCycleTransferStatsStore = make(map[uint64]*model.CycleTransferStats)
	AlertsLock.Lock()
	if err := DB.Find(&Alerts).Error; err != nil {
		panic(err)
	}
	for _, alert := range Alerts {
		alertsStore[alert.ID] = make(map[uint64][][]bool)
		alertsPrevState[alert.ID] = make(map[uint64]uint8)
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
	delete(alertsStore, alert.ID)
	delete(alertsPrevState, alert.ID)
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
	alertsStore[alert.ID] = make(map[uint64][][]bool)
	alertsPrevState[alert.ID] = make(map[uint64]uint8)
	delete(AlertsCycleTransferStatsStore, alert.ID)
	addCycleTransferStatsInfo(alert)
}

func OnDeleteAlert(id []uint64) {
	AlertsLock.Lock()
	defer AlertsLock.Unlock()
	for _, i := range id {
		delete(alertsStore, i)
		delete(alertsPrevState, i)
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
		if !alert.Enabled() {
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
			if alert.UserID != server.UserID && !role.IsAdmin() {
				continue
			}
			alertsStore[alert.ID][server.ID] = append(alertsStore[alert.
				ID][server.ID], alert.Snapshot(AlertsCycleTransferStatsStore[alert.ID], server, DB))
			// 发送通知，分为触发报警和恢复通知
			max, passed := alert.Check(alertsStore[alert.ID][server.ID])
			// 保存当前服务器状态信息
			curServer := model.Server{}
			copier.Copy(&curServer, server)

			// 本次未通过检查
			if !passed {
				// 始终触发模式或上次检查不为失败时触发报警（跳过单次触发+上次失败的情况）
				if alert.TriggerMode == model.ModeAlwaysTrigger || alertsPrevState[alert.ID][server.ID] != _RuleCheckFail {
					alertsPrevState[alert.ID][server.ID] = _RuleCheckFail
					message := fmt.Sprintf("[%s] %s(%s) %s", Localizer.T("Incident"),
						server.Name, IPDesensitize(server.GeoIP.IP.Join()), alert.Name)
					go CronShared.SendTriggerTasks(alert.FailTriggerTasks, curServer.ID)
					go NotificationShared.SendNotification(alert.NotificationGroupID, message, NotificationMuteLabel.ServerIncident(server.ID, alert.ID), &curServer)
					// 清除恢复通知的静音缓存
					NotificationShared.UnMuteNotification(alert.NotificationGroupID, NotificationMuteLabel.ServerIncidentResolved(server.ID, alert.ID))
				}
			} else {
				// 本次通过检查但上一次的状态为失败，则发送恢复通知
				if alertsPrevState[alert.ID][server.ID] == _RuleCheckFail {
					message := fmt.Sprintf("[%s] %s(%s) %s", Localizer.T("Resolved"),
						server.Name, IPDesensitize(server.GeoIP.IP.Join()), alert.Name)
					go CronShared.SendTriggerTasks(alert.RecoverTriggerTasks, curServer.ID)
					go NotificationShared.SendNotification(alert.NotificationGroupID, message, NotificationMuteLabel.ServerIncidentResolved(server.ID, alert.ID), &curServer)
					// 清除失败通知的静音缓存
					NotificationShared.UnMuteNotification(alert.NotificationGroupID, NotificationMuteLabel.ServerIncident(server.ID, alert.ID))
				}
				alertsPrevState[alert.ID][server.ID] = _RuleCheckPass
			}
			// 清理旧数据
			if max > 0 && max < len(alertsStore[alert.ID][server.ID]) {
				index := len(alertsStore[alert.ID][server.ID]) - max
				alertsStore[alert.ID][server.ID] = alertsStore[alert.ID][server.ID][index:]
			}
		}
	}
}
