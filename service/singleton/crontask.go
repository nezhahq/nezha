package singleton

import (
	"cmp"
	"fmt"
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

type CronClass struct {
	class[uint64, *model.Cron]
	*cron.Cron
	pendingAlertTriggerTasksMu sync.Mutex
	pendingAlertTriggerTasks   map[uint64]map[uint64][]time.Time
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

func NewCronClass() *CronClass {
	cronx := cron.New(cron.WithSeconds(), cron.WithLocation(Loc))
	list := make(map[uint64]*model.Cron)

	var sortedList []*model.Cron
	DB.Find(&sortedList)

	var err error
	var notificationGroupList []uint64
	notificationMsgMap := make(map[uint64]*strings.Builder)

	for _, cron := range sortedList {
		// 触发任务类型无需注册
		if cron.TaskType == model.CronTypeTriggerTask {
			list[cron.ID] = cron
			continue
		}
		// 注册计划任务
		cron.CronJobID, err = cronx.AddFunc(cron.Scheduler, CronTrigger(cron))
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
	c.listMu.Lock()
	for _, id := range idList {
		cr := c.list[id]
		if cr != nil && cr.CronJobID != 0 {
			c.Cron.Remove(cr.CronJobID)
		}
		delete(c.list, id)
	}
	c.listMu.Unlock()
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
	if cr == nil || reporter == nil || !cronCanSendToServer(cr, reporter) {
		return false
	}
	if cr.Cover == model.CronCoverAll {
		return !slices.Contains(cr.Servers, reporter.ID)
	}
	if cr.Cover == model.CronCoverIgnoreAll {
		return slices.Contains(cr.Servers, reporter.ID)
	}
	if cr.Cover == model.CronCoverAlertTrigger {
		return CronShared != nil && CronShared.consumeAlertTriggerCronResult(cr.ID, reporter.ID)
	}
	return false
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
	c.pendingAlertTriggerTasks[cronID][serverID] = append(c.pendingAlertTriggerTasks[cronID][serverID], now.Add(alertTriggerCronResultAuthorizationTTL))
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

	c.pruneExpiredAlertTriggerCronResultsLocked(time.Now())
	return c.consumeAlertTriggerCronResultLocked(cronID, serverID)
}

func (c *CronClass) consumeAlertTriggerCronResultLocked(cronID uint64, serverID uint64) bool {
	serverTasks := c.pendingAlertTriggerTasks[cronID]
	expiresAtList := serverTasks[serverID]
	if len(expiresAtList) == 0 {
		return false
	}
	expiresAtList = expiresAtList[1:]
	if len(expiresAtList) == 0 {
		delete(serverTasks, serverID)
	} else {
		serverTasks[serverID] = expiresAtList
	}
	if len(serverTasks) == 0 {
		delete(c.pendingAlertTriggerTasks, cronID)
	}
	return true
}

func (c *CronClass) pruneExpiredAlertTriggerCronResultsLocked(now time.Time) {
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
		if cr.Cover == model.CronCoverAlertTrigger {
			if len(triggerServer) == 0 {
				return
			}
			if s, ok := ServerShared.Get(triggerServer[0]); ok {
				if !cronCanSendToServer(cr, s) {
					return
				}
				if s.GetTaskStream() != nil {
					release, _, err := BeginServerControl(s.ID, cr.UserID, userIsAdmin(cr.UserID))
					if err != nil {
						return
					}
					defer release()
					cronShared := CronShared
					if cronShared != nil {
						cronShared.reserveAlertTriggerCronResult(cr.ID, s.ID)
					}
					if err := s.SendTask(&pb.Task{
						Id:   cr.ID,
						Data: cr.Command,
						Type: model.TaskTypeCommand,
					}); err != nil && cronShared != nil {
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
				_ = s.QueueTaskGuarded(&pb.Task{
					Id:   cr.ID,
					Data: cr.Command,
					Type: model.TaskTypeCommand,
				}, ServerTaskAdmission(s.ID, cr.UserID))
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
