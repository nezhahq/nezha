package singleton

import (
	"cmp"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

const (
	firstNotificationDelay = time.Minute * 15
)

type NotificationClass struct {
	class[uint64, *model.Notification]

	groupToIDList map[uint64]map[uint64]*model.Notification
	idToGroupList map[uint64]map[uint64]struct{}

	groupList map[uint64]string
	groupMu   sync.RWMutex
}

func NewNotificationClass() *NotificationClass {
	var sortedList []*model.Notification

	groupToIDList := make(map[uint64]map[uint64]*model.Notification)
	idToGroupList := make(map[uint64]map[uint64]struct{})

	groupNotifications := make(map[uint64][]uint64)
	var ngn []model.NotificationGroupNotification
	DB.Find(&ngn)

	for _, n := range ngn {
		groupNotifications[n.NotificationGroupID] = append(groupNotifications[n.NotificationGroupID], n.NotificationID)
	}

	DB.Find(&sortedList)
	list := make(map[uint64]*model.Notification, len(sortedList))
	for _, n := range sortedList {
		list[n.ID] = n
	}

	var groups []model.NotificationGroup
	DB.Find(&groups)
	groupList := make(map[uint64]string)
	for _, grp := range groups {
		groupList[grp.ID] = grp.Name
	}

	for gid, nids := range groupNotifications {
		groupToIDList[gid] = make(map[uint64]*model.Notification)
		for _, nid := range nids {
			if n, ok := list[nid]; ok {
				groupToIDList[gid][n.ID] = n

				if idToGroupList[n.ID] == nil {
					idToGroupList[n.ID] = make(map[uint64]struct{})
				}

				idToGroupList[n.ID][gid] = struct{}{}
			}
		}
	}

	nc := &NotificationClass{
		class: class[uint64, *model.Notification]{
			list:       list,
			sortedList: sortedList,
		},
		groupToIDList: groupToIDList,
		idToGroupList: idToGroupList,
		groupList:     groupList,
	}
	return nc
}

func (c *NotificationClass) Update(n *model.Notification) {
	c.listMu.Lock()

	_, ok := c.list[n.ID]
	c.list[n.ID] = n

	if ok {
		if gids, ok := c.idToGroupList[n.ID]; ok {
			for gid := range gids {
				c.groupToIDList[gid][n.ID] = n
			}
		}
	}

	c.listMu.Unlock()
	c.sortList()
}

func (c *NotificationClass) UpdateGroup(ng *model.NotificationGroup, ngn []uint64) {
	c.groupMu.Lock()
	defer c.groupMu.Unlock()

	_, ok := c.groupList[ng.ID]
	c.groupList[ng.ID] = ng.Name

	c.listMu.Lock()
	defer c.listMu.Unlock()
	if !ok {
		c.groupToIDList[ng.ID] = make(map[uint64]*model.Notification, len(ngn))
		for _, n := range ngn {
			if c.idToGroupList[n] == nil {
				c.idToGroupList[n] = make(map[uint64]struct{})
			}
			c.idToGroupList[n][ng.ID] = struct{}{}
			c.groupToIDList[ng.ID][n] = c.list[n]
		}
	} else {
		oldList := make(map[uint64]struct{})
		for nid := range c.groupToIDList[ng.ID] {
			oldList[nid] = struct{}{}
		}

		c.groupToIDList[ng.ID] = make(map[uint64]*model.Notification)
		for _, nid := range ngn {
			c.groupToIDList[ng.ID][nid] = c.list[nid]
			if c.idToGroupList[nid] == nil {
				c.idToGroupList[nid] = make(map[uint64]struct{})
			}
			c.idToGroupList[nid][ng.ID] = struct{}{}
		}

		for oldID := range oldList {
			if _, ok := c.groupToIDList[ng.ID][oldID]; !ok {
				delete(c.groupToIDList[oldID], ng.ID)
				if len(c.idToGroupList[oldID]) == 0 {
					delete(c.idToGroupList, oldID)
				}
			}
		}
	}
}

func (c *NotificationClass) Delete(idList []uint64) {
	c.listMu.Lock()

	for _, id := range idList {
		delete(c.list, id)
		// 如果绑定了通知组才删除
		if gids, ok := c.idToGroupList[id]; ok {
			for gid := range gids {
				delete(c.groupToIDList[gid], id)
				delete(c.idToGroupList, id)
			}
		}
	}

	c.listMu.Unlock()
	c.sortList()
}

func (c *NotificationClass) DeleteGroup(gids []uint64) {
	c.listMu.Lock()
	defer c.listMu.Unlock()
	c.groupMu.Lock()
	defer c.groupMu.Unlock()

	for _, gid := range gids {
		delete(c.groupList, gid)
		delete(c.groupToIDList, gid)
	}
}

func (c *NotificationClass) GetGroupName(gid uint64) string {
	c.groupMu.RLock()
	defer c.groupMu.RUnlock()

	return c.groupList[gid]
}

func (c *NotificationClass) sortList() {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	sortedList := utils.MapValuesToSlice(c.list)
	slices.SortFunc(sortedList, func(a, b *model.Notification) int {
		return cmp.Compare(a.ID, b.ID)
	})

	c.sortedListMu.Lock()
	defer c.sortedListMu.Unlock()
	c.sortedList = sortedList
}

func (c *NotificationClass) UnMuteNotification(notificationGroupID uint64, muteLabel string) {
	fullMuteLabel := NotificationMuteLabel.AppendNotificationGroupName(muteLabel, c.GetGroupName(notificationGroupID))
	Cache.Delete(fullMuteLabel)
}

// SendNotification 向指定的通知方式组的所有通知方式发送通知
func (c *NotificationClass) SendNotification(notificationGroupID uint64, desc string, muteLabel string, ext ...*model.Server) {
	if muteLabel != "" {
		// 将通知方式组名称加入静音标志
		muteLabel := NotificationMuteLabel.AppendNotificationGroupName(muteLabel, c.GetGroupName(notificationGroupID))
		// 通知防骚扰策略
		var flag bool
		if cacheN, has := Cache.Get(muteLabel); has {
			nHistory := cacheN.(NotificationHistory)
			// 每次提醒都增加一倍等待时间，最后每天最多提醒一次
			if time.Now().After(nHistory.Until) {
				flag = true
				nHistory.Duration *= 2
				if nHistory.Duration > time.Hour*24 {
					nHistory.Duration = time.Hour * 24
				}
				nHistory.Until = time.Now().Add(nHistory.Duration)
				// 缓存有效期加 10 分钟
				Cache.Set(muteLabel, nHistory, nHistory.Duration+time.Minute*10)
			}
		} else {
			// 新提醒直接通知
			flag = true
			Cache.Set(muteLabel, NotificationHistory{
				Duration: firstNotificationDelay,
				Until:    time.Now().Add(firstNotificationDelay),
			}, firstNotificationDelay+time.Minute*10)
		}

		if !flag {
			if Conf.Debug {
				log.Println("NEZHA>> Muted repeated notification", desc, muteLabel)
			}
			return
		}
	}
	// 向该通知方式组的所有通知方式发出通知
	c.listMu.RLock()
	defer c.listMu.RUnlock()
	for _, n := range c.groupToIDList[notificationGroupID] {
		log.Printf("NEZHA>> Try to notify %s", n.Name)
	}
	for _, n := range c.groupToIDList[notificationGroupID] {
		ns := model.NotificationServerBundle{
			Notification: n,
			Server:       nil,
			Loc:          Loc,
		}
		if len(ext) > 0 {
			ns.Server = ext[0]
		}
		if err := ns.Send(desc); err != nil {
			log.Printf("NEZHA>> Sending notification to %s failed: %v", n.Name, err)
		} else {
			log.Printf("NEZHA>> Sending notification to %s succeeded", n.Name)
		}
	}
}

type _NotificationMuteLabel struct{}

var NotificationMuteLabel _NotificationMuteLabel

func (_NotificationMuteLabel) IPChanged(serverId uint64) string {
	return fmt.Sprintf("bf::ic-%d", serverId)
}

func (_NotificationMuteLabel) ServerIncident(alertId uint64, serverId uint64) string {
	return fmt.Sprintf("bf::sei-%d-%d", alertId, serverId)
}

func (_NotificationMuteLabel) ServerIncidentResolved(alertId uint64, serverId uint64) string {
	return fmt.Sprintf("bf::seir-%d-%d", alertId, serverId)
}

func (_NotificationMuteLabel) AppendNotificationGroupName(label string, notificationGroupName string) string {
	return fmt.Sprintf("%s:%s", label, notificationGroupName)
}

func (_NotificationMuteLabel) ServiceLatencyMin(serviceId uint64) string {
	return fmt.Sprintf("bf::sln-%d", serviceId)
}

func (_NotificationMuteLabel) ServiceLatencyMax(serviceId uint64) string {
	return fmt.Sprintf("bf::slm-%d", serviceId)
}

func (_NotificationMuteLabel) ServiceStateChanged(serviceId uint64) string {
	return fmt.Sprintf("bf::ssc-%d", serviceId)
}

func (_NotificationMuteLabel) ServiceTLS(serviceId uint64, extraInfo string) string {
	return fmt.Sprintf("bf::stls-%d-%s", serviceId, extraInfo)
}
