package singleton

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/jinzhu/copier"

	"github.com/robfig/cron/v3"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	pb "github.com/nezhahq/nezha/proto"
)

type CronClass struct {
	class[uint64, *model.Cron]
	*cron.Cron
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
		Cron: cronx,
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

func (c *CronClass) SendTriggerTasks(taskIDs []uint64, triggerServer uint64) {
	c.listMu.RLock()
	var cronLists []*model.Cron
	for _, taskID := range taskIDs {
		if c, ok := c.list[taskID]; ok {
			cronLists = append(cronLists, c)
		}
	}
	c.listMu.RUnlock()

	// 依次调用CronTrigger发送任务
	for _, c := range cronLists {
		go CronTrigger(c, triggerServer)()
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
					go NotificationShared.SendNotification(cr.NotificationGroupID, Localizer.Tf("[Task failed] %s: server %s is offline and cannot execute the task", cr.Name, s.Name), "", &curServer)
				}
			}
			return
		}

		for _, s := range ServerShared.Range {
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
				go NotificationShared.SendNotification(cr.NotificationGroupID, Localizer.Tf("[Task failed] %s: server %s is offline and cannot execute the task", cr.Name, s.Name), "", &curServer)
			}
		}
	}
}
