package singleton

import (
	"cmp"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jinzhu/copier"

	"github.com/robfig/cron/v3"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	pb "github.com/nezhahq/nezha/proto"
)

const alertTriggerCronResultAuthorizationTTL = 24 * time.Hour

// Result authorizations are reserved for every cover, once per server on every
// run of a scheduled cron. An agent that never answers must not grow its list
// without bound, and the sweep over all lists must not run for each dispatched
// task or reported result.
const (
	cronResultAuthorizationLimit         = 64
	cronResultAuthorizationSweepInterval = time.Minute
)

type CronClass struct {
	class[uint64, *model.Cron]
	*cron.Cron
	pendingAlertTriggerTasksMu sync.Mutex
	pendingAlertTriggerTasks   map[uint64]map[uint64][]time.Time
	pendingAlertTriggerSweptAt time.Time
	closeOnce                  sync.Once
}

// Close stops the scheduler and joins every job before a test restores globals.
// The embedded cron.Stop only exposes the completion context; callers must await it.
func (c *CronClass) Close() {
	if c == nil || c.Cron == nil {
		return
	}
	c.closeOnce.Do(func() {
		<-c.Cron.Stop().Done()
	})
}

// cronSpecParser is the cron.WithSeconds() grammar without the specs that
// robfig/cron mishandles. A field made only of commas parses to an empty bit
// mask without an error, and SpecSchedule.Next then steps through five years
// of candidate times on the scheduler goroutine shared by every job. A TZ=
// prefix that is not followed by any field panics inside Parser.Parse.
type cronSpecParser struct {
	cron.Parser
}

func newCronSpecParser() cronSpecParser {
	return cronSpecParser{cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)}
}

func (p cronSpecParser) Parse(spec string) (cron.Schedule, error) {
	if (strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=")) && !strings.Contains(spec, " ") {
		return nil, fmt.Errorf("missing fields in spec %q", spec)
	}
	schedule, err := p.Parser.Parse(spec)
	if err != nil {
		return nil, err
	}
	if s, ok := schedule.(*cron.SpecSchedule); ok {
		for _, bits := range []uint64{s.Second, s.Minute, s.Hour, s.Dom, s.Month, s.Dow} {
			if bits == 0 {
				return nil, fmt.Errorf("empty field in spec %q", spec)
			}
		}
	}
	return schedule, nil
}

func NewCronClass() *CronClass {
	cronx := cron.New(cron.WithParser(newCronSpecParser()), cron.WithLocation(Loc))
	list := make(map[uint64]*model.Cron)

	var sortedList []*model.Cron
	DB.Find(&sortedList)

	var err error
	var notificationGroupList []uint64
	notificationMsgMap := make(map[uint64]*strings.Builder)

	for _, cron := range sortedList {
		// Rows stored before the command limit existed are reported like a
		// failed registration instead of being scheduled or left triggerable.
		err = model.ValidateCronCommand(cron.Command)
		// 触发任务类型无需注册
		if err == nil && cron.TaskType == model.CronTypeTriggerTask {
			list[cron.ID] = cron
			continue
		}
		// 注册计划任务
		if err == nil {
			cron.CronJobID, err = cronx.AddFunc(cron.Scheduler, CronTrigger(cron))
		}
		if err == nil {
			list[cron.ID] = cron
		} else {
			// 当前通知组首次出现 将其加入通知组列表并初始化通知组消息缓存
			if _, ok := notificationMsgMap[cron.NotificationGroupID]; !ok {
				notificationGroupList = append(notificationGroupList, cron.NotificationGroupID)
				notificationMsgMap[cron.NotificationGroupID] = new(strings.Builder)
				notificationMsgMap[cron.NotificationGroupID].WriteString(Localizer.T("Tasks failed to register: ["))
			}
			notificationMsgMap[cron.NotificationGroupID].WriteString(fmt.Sprintf("%d,", cron.ID))
		}
	}

	// 向注册错误的计划任务所在通知组发送通知
	for _, gid := range notificationGroupList {
		notificationMsgMap[gid].WriteString(Localizer.T("] These tasks will not execute properly. Fix them in the admin dashboard."))
		NotificationShared.SendNotification(gid, notificationMsgMap[gid].String(), "")
	}
	cronx.Start()

	return &CronClass{
		class: class[uint64, *model.Cron]{
			list:       list,
			sortedList: sortedList,
		},
		Cron:                     cronx,
		pendingAlertTriggerTasks: make(map[uint64]map[uint64][]time.Time),
	}
}

func (c *CronClass) Update(cr *model.Cron) {
	c.listMu.Lock()
	crOld := c.list[cr.ID]
	if crOld != nil && crOld.CronJobID != 0 {
		c.Cron.Remove(crOld.CronJobID)
	}

	delete(c.list, cr.ID)
	c.list[cr.ID] = cr
	c.listMu.Unlock()
	c.deleteAlertTriggerCronResultAuthorizations([]uint64{cr.ID})

	c.sortList()
}

func (c *CronClass) Delete(idList []uint64) {
	var jobs []cron.EntryID
	c.listMu.Lock()
	for _, id := range idList {
		cr := c.list[id]
		if cr != nil && cr.CronJobID != 0 {
			jobs = append(jobs, cr.CronJobID)
		}
		delete(c.list, id)
	}
	c.listMu.Unlock()
	// Every Remove makes the scheduler copy and sort all of its entries, so
	// the jobs are unregistered after the lock every task lookup waits for is
	// released.
	for _, job := range jobs {
		c.Cron.Remove(job)
	}
	c.deleteAlertTriggerCronResultAuthorizations(idList)

	c.sortList()
}

func (c *CronClass) sortList() {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	sortedList := utils.MapValuesToSlice(c.list)
	slices.SortFunc(sortedList, func(a, b *model.Cron) int {
		return cmp.Compare(a.ID, b.ID)
	})

	c.sortedListMu.Lock()
	defer c.sortedListMu.Unlock()
	c.sortedList = sortedList
}

func (c *CronClass) SendTriggerTasks(taskIDs []uint64, triggerServer uint64, triggerOwner uint64) {
	c.listMu.RLock()
	var cronLists []*model.Cron
	for _, taskID := range taskIDs {
		if c, ok := c.list[taskID]; ok && cronCanBeTriggeredByOwner(c, triggerOwner) {
			cronLists = append(cronLists, c)
		}
	}
	c.listMu.RUnlock()

	// 依次调用CronTrigger发送任务
	for _, c := range cronLists {
		go CronTrigger(c, triggerServer)()
	}
}

func cronCanBeTriggeredByOwner(cr *model.Cron, triggerOwner uint64) bool {
	return cr.UserID == triggerOwner || userIsAdmin(triggerOwner)
}

func CanReportCronResult(cr *model.Cron, reporter *model.Server) bool {
	if cr == nil || reporter == nil || !serverTaskResultAdmitted(reporter, cr.UserID) {
		return false
	}
	if cr.Cover == model.CronCoverAll && slices.Contains(cr.Servers, reporter.ID) {
		return false
	}
	if cr.Cover == model.CronCoverIgnoreAll && !slices.Contains(cr.Servers, reporter.ID) {
		return false
	}
	// Cover only says the cron may be sent to this reporter. Whatever the cover,
	// one result is accepted for each task CronTrigger dispatched to it.
	return CronShared != nil && CronShared.consumeAlertTriggerCronResult(cr.ID, reporter.ID)
}

func (c *CronClass) reserveAlertTriggerCronResult(cronID uint64, serverID uint64) {
	c.pendingAlertTriggerTasksMu.Lock()
	defer c.pendingAlertTriggerTasksMu.Unlock()

	now := time.Now()
	c.pruneExpiredAlertTriggerCronResultsLocked(now)
	if c.pendingAlertTriggerTasks == nil {
		c.pendingAlertTriggerTasks = make(map[uint64]map[uint64][]time.Time)
	}
	if c.pendingAlertTriggerTasks[cronID] == nil {
		c.pendingAlertTriggerTasks[cronID] = make(map[uint64][]time.Time)
	}
	expiresAtList := append(c.pendingAlertTriggerTasks[cronID][serverID], now.Add(alertTriggerCronResultAuthorizationTTL))
	if len(expiresAtList) > cronResultAuthorizationLimit {
		expiresAtList = expiresAtList[len(expiresAtList)-cronResultAuthorizationLimit:]
	}
	c.pendingAlertTriggerTasks[cronID][serverID] = expiresAtList
}

func (c *CronClass) revokeAlertTriggerCronResult(cronID uint64, serverID uint64) {
	c.pendingAlertTriggerTasksMu.Lock()
	defer c.pendingAlertTriggerTasksMu.Unlock()

	serverTasks := c.pendingAlertTriggerTasks[cronID]
	expiresAtList := serverTasks[serverID]
	if len(expiresAtList) == 0 {
		return
	}
	expiresAtList = expiresAtList[:len(expiresAtList)-1]
	if len(expiresAtList) == 0 {
		delete(serverTasks, serverID)
	} else {
		serverTasks[serverID] = expiresAtList
	}
	if len(serverTasks) == 0 {
		delete(c.pendingAlertTriggerTasks, cronID)
	}
}

func (c *CronClass) consumeAlertTriggerCronResult(cronID uint64, serverID uint64) bool {
	c.pendingAlertTriggerTasksMu.Lock()
	defer c.pendingAlertTriggerTasksMu.Unlock()

	now := time.Now()
	c.pruneExpiredAlertTriggerCronResultsLocked(now)
	return c.consumeAlertTriggerCronResultLocked(cronID, serverID, now)
}

func (c *CronClass) consumeAlertTriggerCronResultLocked(cronID uint64, serverID uint64, now time.Time) bool {
	serverTasks := c.pendingAlertTriggerTasks[cronID]
	expiresAtList := serverTasks[serverID]
	// The sweep may not have run since these expired; the oldest come first.
	for len(expiresAtList) > 0 && !expiresAtList[0].After(now) {
		expiresAtList = expiresAtList[1:]
	}
	consumed := len(expiresAtList) > 0
	if consumed {
		expiresAtList = expiresAtList[1:]
	}
	if len(expiresAtList) == 0 {
		delete(serverTasks, serverID)
	} else {
		serverTasks[serverID] = expiresAtList
	}
	if len(serverTasks) == 0 {
		delete(c.pendingAlertTriggerTasks, cronID)
	}
	return consumed
}

func (c *CronClass) pruneExpiredAlertTriggerCronResultsLocked(now time.Time) {
	if now.Sub(c.pendingAlertTriggerSweptAt) < cronResultAuthorizationSweepInterval {
		return
	}
	c.pendingAlertTriggerSweptAt = now
	for cronID, serverTasks := range c.pendingAlertTriggerTasks {
		for serverID, expiresAtList := range serverTasks {
			validExpiresAtList := expiresAtList[:0]
			for _, expiresAt := range expiresAtList {
				if expiresAt.After(now) {
					validExpiresAtList = append(validExpiresAtList, expiresAt)
				}
			}
			if len(validExpiresAtList) == 0 {
				delete(serverTasks, serverID)
			} else {
				serverTasks[serverID] = validExpiresAtList
			}
		}
		if len(serverTasks) == 0 {
			delete(c.pendingAlertTriggerTasks, cronID)
		}
	}
}

func (c *CronClass) deleteAlertTriggerCronResultAuthorizations(cronIDs []uint64) {
	c.pendingAlertTriggerTasksMu.Lock()
	defer c.pendingAlertTriggerTasksMu.Unlock()

	for _, cronID := range cronIDs {
		delete(c.pendingAlertTriggerTasks, cronID)
	}
}

func ManualTrigger(cr *model.Cron) {
	CronTrigger(cr)()
}

func CronTrigger(cr *model.Cron, triggerServer ...uint64) func() {
	crIgnoreMap := make(map[uint64]bool)
	for _, server := range cr.Servers {
		crIgnoreMap[server] = true
	}
	return func() {
		if err := model.ValidateCronCommand(cr.Command); err != nil {
			// Defense in depth, as in DispatchTask: every covered Agent gets
			// its own encoded copy of the command.
			log.Printf("NEZHA>> CronTrigger rejected task %d: %v", cr.ID, err)
			return
		}
		if cr.Cover == model.CronCoverAlertTrigger {
			if len(triggerServer) == 0 {
				return
			}
			if s, ok := ServerShared.Get(triggerServer[0]); ok {
				if !cronCanSendToServer(cr, s) {
					return
				}
				if s.GetTaskStream() != nil {
					cronShared := CronShared
					if cronShared != nil {
						cronShared.reserveAlertTriggerCronResult(cr.ID, s.ID)
					}
					// An alert command can still be sending after timeout. Keep
					// its control lease until the actual transport send exits.
					if err := s.SendTaskGuarded(&pb.Task{
						Id:   cr.ID,
						Data: cr.Command,
						Type: model.TaskTypeCommand,
					}, ServerTaskAdmission(s.ID, cr.UserID)); err != nil && cronShared != nil {
						cronShared.revokeAlertTriggerCronResult(cr.ID, s.ID)
					}
				} else {
					// 保存当前服务器状态信息
					curServer := model.Server{}
					copier.Copy(&curServer, s)
					go NotificationShared.SendNotification(cr.NotificationGroupID, Localizer.Tf("[Task failed] %s: server %s is offline and cannot execute the task", cr.Name, s.Name), "", &curServer)
				}
			}
			return
		}

		// 先在锁内快照 server 列表再逐个 SendTask：ServerShared.Range 会在整个
		// 回调期间持 listMu.RLock，而 SendTask 走阻塞 gRPC，一个卡死的 agent
		// 会让需要写锁的 server 编辑/删除被拖死。GetList 克隆后即释放锁。
		for _, s := range ServerShared.GetList() {
			if s == nil {
				continue
			}
			if !cronCanSendToServer(cr, s) {
				continue
			}
			if cr.Cover == model.CronCoverAll && crIgnoreMap[s.ID] {
				continue
			}
			if cr.Cover == model.CronCoverIgnoreAll && !crIgnoreMap[s.ID] {
				continue
			}
			if s.GetTaskStream() != nil {
				cronShared := CronShared
				if cronShared != nil {
					cronShared.reserveAlertTriggerCronResult(cr.ID, s.ID)
				}
				// The agent can answer as soon as the queued task is sent, so
				// reserve first and roll back when the task was never queued.
				if err := s.QueueTaskGuarded(&pb.Task{
					Id:   cr.ID,
					Data: cr.Command,
					Type: model.TaskTypeCommand,
				}, ServerTaskAdmission(s.ID, cr.UserID)); err != nil && cronShared != nil {
					cronShared.revokeAlertTriggerCronResult(cr.ID, s.ID)
				}
			} else {
				// 保存当前服务器状态信息
				curServer := model.Server{}
				copier.Copy(&curServer, s)
				go NotificationShared.SendNotification(cr.NotificationGroupID, Localizer.Tf("[Task failed] %s: server %s is offline and cannot execute the task", cr.Name, s.Name), "", &curServer)
			}
		}
	}
}

func cronCanSendToServer(cr *model.Cron, server *model.Server) bool {
	return cr.UserID == server.GetUserID() || userIsAdmin(cr.UserID)
}

func userIsAdmin(userID uint64) bool {
	if userID == 0 {
		return true
	}

	UserLock.RLock()
	defer UserLock.RUnlock()

	userInfo, ok := UserInfoMap[userID]
	return ok && userInfo.Role.IsAdmin()
}
