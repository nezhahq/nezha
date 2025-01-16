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

// 通知方式
var (
	NotificationList       map[uint64]map[uint64]*model.Notification // [NotificationGroupID][NotificationID] -> model.Notification
	NotificationIDToGroups map[uint64]map[uint64]struct{}            // [NotificationID] -> NotificationGroupID

	NotificationMap        map[uint64]*model.Notification
	NotificationListSorted []*model.Notification
	NotificationGroup      map[uint64]string // [NotificationGroupID] -> [NotificationGroupName]

	NotificationsLock      sync.RWMutex
	NotificationSortedLock sync.RWMutex
	NotificationGroupLock  sync.RWMutex
)

// InitNotification 初始化 GroupID <-> ID <-> Notification 的映射
func initNotification() {
	NotificationList = make(map[uint64]map[uint64]*model.Notification)
	NotificationIDToGroups = make(map[uint64]map[uint64]struct{})
	NotificationGroup = make(map[uint64]string)
}

// loadNotifications 从 DB 初始化通知方式相关参数
func loadNotifications() {
	initNotification()
	groupNotifications := make(map[uint64][]uint64)
	var ngn []model.NotificationGroupNotification
	if err := DB.Find(&ngn).Error; err != nil {
		panic(err)
	}

	for _, n := range ngn {
		groupNotifications[n.NotificationGroupID] = append(groupNotifications[n.NotificationGroupID], n.NotificationID)
	}

	if err := DB.Find(&NotificationListSorted).Error; err != nil {
		panic(err)
	}

	NotificationMap = make(map[uint64]*model.Notification, len(NotificationListSorted))
	for i := range NotificationListSorted {
		NotificationMap[NotificationListSorted[i].ID] = NotificationListSorted[i]
	}

	for gid, nids := range groupNotifications {
		NotificationList[gid] = make(map[uint64]*model.Notification)
		for _, nid := range nids {
			if n, ok := NotificationMap[nid]; ok {
				NotificationList[gid][n.ID] = n

				if NotificationIDToGroups[n.ID] == nil {
					NotificationIDToGroups[n.ID] = make(map[uint64]struct{})
				}

				NotificationIDToGroups[n.ID][gid] = struct{}{}
			}
		}
	}
}

func UpdateNotificationList() {
	NotificationsLock.RLock()
	defer NotificationsLock.RUnlock()

	NotificationSortedLock.Lock()
	defer NotificationSortedLock.Unlock()

	NotificationListSorted = utils.MapValuesToSlice(NotificationMap)
	slices.SortFunc(NotificationListSorted, func(a, b *model.Notification) int {
		return cmp.Compare(a.ID, b.ID)
	})
}

// OnRefreshOrAddNotificationGroup 刷新通知方式组相关参数
func OnRefreshOrAddNotificationGroup(ng *model.NotificationGroup, ngn []uint64) {
	NotificationsLock.Lock()
	defer NotificationsLock.Unlock()

	NotificationGroupLock.Lock()
	defer NotificationGroupLock.Unlock()
	var isEdit bool
	if _, ok := NotificationGroup[ng.ID]; ok {
		isEdit = true
	}

	if !isEdit {
		AddNotificationGroupToList(ng, ngn)
	} else {
		UpdateNotificationGroupInList(ng, ngn)
	}
}

// AddNotificationGroupToList 添加通知方式组到map中
func AddNotificationGroupToList(ng *model.NotificationGroup, ngn []uint64) {
	NotificationGroup[ng.ID] = ng.Name

	NotificationList[ng.ID] = make(map[uint64]*model.Notification, len(ngn))

	for _, n := range ngn {
		if NotificationIDToGroups[n] == nil {
			NotificationIDToGroups[n] = make(map[uint64]struct{})
		}
		NotificationIDToGroups[n][ng.ID] = struct{}{}
		NotificationList[ng.ID][n] = NotificationMap[n]
	}
}

// UpdateNotificationGroupInList 在 map 中更新通知方式组
func UpdateNotificationGroupInList(ng *model.NotificationGroup, ngn []uint64) {
	NotificationGroup[ng.ID] = ng.Name

	oldList := make(map[uint64]struct{})
	for nid := range NotificationList[ng.ID] {
		oldList[nid] = struct{}{}
	}

	NotificationList[ng.ID] = make(map[uint64]*model.Notification)
	for _, nid := range ngn {
		NotificationList[ng.ID][nid] = NotificationMap[nid]
		if NotificationIDToGroups[nid] == nil {
			NotificationIDToGroups[nid] = make(map[uint64]struct{})
		}
		NotificationIDToGroups[nid][ng.ID] = struct{}{}
	}

	for oldID := range oldList {
		if _, ok := NotificationList[ng.ID][oldID]; !ok {
			delete(NotificationIDToGroups[oldID], ng.ID)
			if len(NotificationIDToGroups[oldID]) == 0 {
				delete(NotificationIDToGroups, oldID)
			}
		}
	}
}

// UpdateNotificationGroupInList 删除通知方式组
func OnDeleteNotificationGroup(gids []uint64) {
	NotificationsLock.Lock()
	defer NotificationsLock.Unlock()

	for _, gid := range gids {
		delete(NotificationGroup, gid)
		delete(NotificationList, gid)
	}
}

// OnRefreshOrAddNotification 刷新通知方式相关参数
func OnRefreshOrAddNotification(n *model.Notification) {
	NotificationsLock.Lock()
	defer NotificationsLock.Unlock()

	var isEdit bool
	_, ok := NotificationMap[n.ID]
	if ok {
		isEdit = true
	}
	if !isEdit {
		AddNotificationToList(n)
	} else {
		UpdateNotificationInList(n)
	}
}

// AddNotificationToList 添加通知方式到map中
func AddNotificationToList(n *model.Notification) {
	NotificationMap[n.ID] = n
}

// UpdateNotificationInList 在 map 中更新通知方式
func UpdateNotificationInList(n *model.Notification) {
	NotificationMap[n.ID] = n
	// 如果已经与通知组有绑定关系，更新
	if gids, ok := NotificationIDToGroups[n.ID]; ok {
		for gid := range gids {
			NotificationList[gid][n.ID] = n
		}
	}
}

// OnDeleteNotification 在map和表中删除通知方式
func OnDeleteNotification(id []uint64) {
	NotificationsLock.Lock()
	defer NotificationsLock.Unlock()

	for _, i := range id {
		delete(NotificationMap, i)
		// 如果绑定了通知组才删除
		if gids, ok := NotificationIDToGroups[i]; ok {
			for gid := range gids {
				delete(NotificationList[gid], i)
				delete(NotificationIDToGroups, i)
			}
		}
	}
}

func UnMuteNotification(notificationGroupID uint64, muteLabel *string) {
	fullMuteLabel := *NotificationMuteLabel.AppendNotificationGroupName(muteLabel, notificationGroupID)
	Cache.Delete(fullMuteLabel)
}

// SendNotification 向指定的通知方式组的所有通知方式发送通知
func SendNotification(notificationGroupID uint64, desc string, muteLabel *string, ext ...*model.Server) {
	if muteLabel != nil {
		// 将通知方式组名称加入静音标志
		muteLabel := *NotificationMuteLabel.AppendNotificationGroupName(muteLabel, notificationGroupID)
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
	NotificationsLock.RLock()
	defer NotificationsLock.RUnlock()
	for _, n := range NotificationList[notificationGroupID] {
		log.Printf("NEZHA>> Try to notify %s", n.Name)
	}
	for _, n := range NotificationList[notificationGroupID] {
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
			log.Printf("NEZHA>> Sending notification to %s succeed", n.Name)
		}
	}
}

type _NotificationMuteLabel struct{}

var NotificationMuteLabel _NotificationMuteLabel

func (_NotificationMuteLabel) IPChanged(serverId uint64) *string {
	label := fmt.Sprintf("bf::ic-%d", serverId)
	return &label
}

func (_NotificationMuteLabel) ServerIncident(alertId uint64, serverId uint64) *string {
	label := fmt.Sprintf("bf::sei-%d-%d", alertId, serverId)
	return &label
}

func (_NotificationMuteLabel) ServerIncidentResolved(alertId uint64, serverId uint64) *string {
	label := fmt.Sprintf("bf::seir-%d-%d", alertId, serverId)
	return &label
}

func (_NotificationMuteLabel) AppendNotificationGroupName(label *string, notificationGroupID uint64) *string {
	NotificationGroupLock.RLock()
	defer NotificationGroupLock.RUnlock()
	newLabel := fmt.Sprintf("%s:%s", *label, NotificationGroup[notificationGroupID])
	return &newLabel
}

func (_NotificationMuteLabel) ServiceLatencyMin(serviceId uint64) *string {
	label := fmt.Sprintf("bf::sln-%d", serviceId)
	return &label
}

func (_NotificationMuteLabel) ServiceLatencyMax(serviceId uint64) *string {
	label := fmt.Sprintf("bf::slm-%d", serviceId)
	return &label
}

func (_NotificationMuteLabel) ServiceStateChanged(serviceId uint64) *string {
	label := fmt.Sprintf("bf::ssc-%d", serviceId)
	return &label
}

func (_NotificationMuteLabel) ServiceTLS(serviceId uint64, extraInfo string) *string {
	label := fmt.Sprintf("bf::stls-%d-%s", serviceId, extraInfo)
	return &label
}
