package singleton

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jinzhu/copier"

	"github.com/robfig/cron/v3"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	pb "github.com/nezhahq/nezha/proto"
)

var (
	Cron     *cron.Cron
	Crons    map[uint64]*model.Cron // [CronID] -> *model.Cron
	CronLock sync.RWMutex

	CronList []*model.Cron
)

func InitCronTask() {
	Cron = cron.New(cron.WithSeconds(), cron.WithLocation(Loc))
	Crons = make(map[uint64]*model.Cron)
}

// loadCronTasks 加载计划任务
func loadCronTasks() {
	InitCronTask()
	DB.Find(&CronList)
	var err error
	var notificationGroupList []uint64
	notificationMsgMap := make(map[uint64]*strings.Builder)
	for _, cron := range CronList {
		// 触发任务类型无需注册
		if cron.TaskType == model.CronTypeTriggerTask {
			Crons[cron.ID] = cron
			continue
		}
		// 注册计划任务
		cron.CronJobID, err = Cron.AddFunc(cron.Scheduler, CronTrigger(cron))
		if err == nil {
			Crons[cron.ID] = cron
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
		NotificationShared.SendNotification(gid, notificationMsgMap[gid].String(), nil)
	}
	Cron.Start()
}

func OnRefreshOrAddCron(c *model.Cron) {
	CronLock.Lock()
	defer CronLock.Unlock()
	crOld := Crons[c.ID]
	if crOld != nil && crOld.CronJobID != 0 {
		Cron.Remove(crOld.CronJobID)
	}

	delete(Crons, c.ID)
	Crons[c.ID] = c
}

func UpdateCronList() {
	CronLock.RLock()
	defer CronLock.RUnlock()

	CronList = utils.MapValuesToSlice(Crons)
	slices.SortFunc(CronList, func(a, b *model.Cron) int {
		return cmp.Compare(a.ID, b.ID)
	})
}

func OnDeleteCron(id []uint64) {
	CronLock.Lock()
	defer CronLock.Unlock()
	for _, i := range id {
		cr := Crons[i]
		if cr != nil && cr.CronJobID != 0 {
			Cron.Remove(cr.CronJobID)
		}
		delete(Crons, i)
	}
}

func ManualTrigger(c *model.Cron) {
	CronTrigger(c)()
}

func SendTriggerTasks(taskIDs []uint64, triggerServer uint64) {
	CronLock.RLock()
	var cronLists []*model.Cron
	for _, taskID := range taskIDs {
		if c, ok := Crons[taskID]; ok {
			cronLists = append(cronLists, c)
		}
	}
	CronLock.RUnlock()

	// 依次调用CronTrigger发送任务
	for _, c := range cronLists {
		go CronTrigger(c, triggerServer)()
	}
}

func CronTrigger(cr *model.Cron, triggerServer ...uint64) func() {
	crIgnoreMap := make(map[uint64]bool)
	for j := 0; j < len(cr.Servers); j++ {
		crIgnoreMap[cr.Servers[j]] = true
	}
	return func() {
		if cr.Cover == model.CronCoverAlertTrigger {
			if len(triggerServer) == 0 {
				return
			}
			m := ServerShared.GetList()
			if s, ok := m[triggerServer[0]]; ok {
				if s.TaskStream != nil {
					s.TaskStream.Send(&pb.Task{
						Id:   cr.ID,
						Data: cr.Command,
						Type: model.TaskTypeCommand,
					})
				} else {
					// 保存当前服务器状态信息
					curServer := model.Server{}
					copier.Copy(&curServer, s)
					NotificationShared.SendNotification(cr.NotificationGroupID, Localizer.Tf("[Task failed] %s: server %s is offline and cannot execute the task", cr.Name, s.Name), nil, &curServer)
				}
			}
			return
		}

		m := ServerShared.GetList()
		for _, s := range m {
			if cr.Cover == model.CronCoverAll && crIgnoreMap[s.ID] {
				continue
			}
			if cr.Cover == model.CronCoverIgnoreAll && !crIgnoreMap[s.ID] {
				continue
			}
			if s.TaskStream != nil {
				s.TaskStream.Send(&pb.Task{
					Id:   cr.ID,
					Data: cr.Command,
					Type: model.TaskTypeCommand,
				})
			} else {
				// 保存当前服务器状态信息
				curServer := model.Server{}
				copier.Copy(&curServer, s)
				NotificationShared.SendNotification(cr.NotificationGroupID, Localizer.Tf("[Task failed] %s: server %s is offline and cannot execute the task", cr.Name, s.Name), nil, &curServer)
			}
		}
	}
}
