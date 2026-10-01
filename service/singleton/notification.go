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

	groupList       map[uint64]string
	groupMu         sync.RWMutex
	deliveryMu      sync.Mutex
	deliveryKeys    map[string]*notificationKeyLock
	asyncInFlight   map[string]struct{}
	asyncRetryAfter map[string]time.Time
	asyncRetryDelay map[string]time.Duration
	muteGeneration  map[string]uint64
	serviceDelivery map[uint64]*orderedServiceDelivery
	sendForTest     func(*model.Notification, string, *model.Server) error
}

type serviceDeliveryRequest struct {
	groupID uint64
	desc    string
	label   string
}

type orderedServiceDelivery struct {
	current serviceDeliveryRequest
	pending *serviceDeliveryRequest
	update  chan struct{}
	cancel  chan struct{}
	done    chan struct{}
}

// SendServiceState preserves incident→recovery order without allowing a
// slow webhook to accumulate one goroutine per probe result. Only the latest
// pending phase is retained; a failed current phase retries with backoff.
func (c *NotificationClass) SendServiceState(serviceID, groupID uint64, desc, label string) {
	req := serviceDeliveryRequest{groupID: groupID, desc: desc, label: label}
	c.deliveryMu.Lock()
	if c.serviceDelivery == nil {
		c.serviceDelivery = make(map[uint64]*orderedServiceDelivery)
	}
	state := c.serviceDelivery[serviceID]
	if state == nil {
		state = &orderedServiceDelivery{current: req, update: make(chan struct{}, 1), cancel: make(chan struct{}), done: make(chan struct{})}
		c.serviceDelivery[serviceID] = state
		c.deliveryMu.Unlock()
		go c.runServiceDelivery(serviceID, state)
		return
	}
	if state.pending == nil && state.current.label == label {
		c.deliveryMu.Unlock()
		return
	}
	if state.pending == nil || state.pending.label != label {
		state.pending = &req
		select {
		case state.update <- struct{}{}:
		default:
		}
	}
	c.deliveryMu.Unlock()
}

func (c *NotificationClass) runServiceDelivery(serviceID uint64, state *orderedServiceDelivery) {
	defer close(state.done)
	delay := 30 * time.Second
	for {
		select {
		case <-state.cancel:
			return
		default:
		}
		delivered := c.SendNotification(state.current.groupID, state.current.desc, state.current.label)
		c.deliveryMu.Lock()
		// Do not advance to a later phase until every recipient has received the
		// current one. Otherwise a failed incident can be discarded as soon as a
		// recovery is queued, leaving that recipient with a recovery but no incident.
		if delivered && state.pending != nil {
			next := state.pending
			state.current = *next
			state.pending = nil
			delay = 30 * time.Second
			select {
			case <-state.update:
			default:
			}
			c.deliveryMu.Unlock()
			continue
		}
		if delivered {
			if c.serviceDelivery[serviceID] == state {
				delete(c.serviceDelivery, serviceID)
			}
			c.deliveryMu.Unlock()
			return
		}
		c.deliveryMu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-state.cancel:
			timer.Stop()
			return
		case <-state.update:
			timer.Stop()
		case <-timer.C:
			delay = min(delay*2, 15*time.Minute)
		}
	}
}

func (c *NotificationClass) CancelServiceState(serviceID uint64) {
	if c == nil {
		return
	}
	c.deliveryMu.Lock()
	if state := c.serviceDelivery[serviceID]; state != nil {
		close(state.cancel)
		delete(c.serviceDelivery, serviceID)
	}
	c.deliveryMu.Unlock()
}

// SendNotificationAsync coalesces concurrent service-probe reports for the
// same event before allocating a webhook goroutine. Alert transitions use the
// synchronous SendNotification path so they retain their retry semantics.
func (c *NotificationClass) SendNotificationAsync(groupID uint64, desc, muteLabel string, ext ...*model.Server) {
	c.sendNotificationAsync(groupID, desc, muteLabel, nil, ext...)
}

func (c *NotificationClass) sendNotificationAsync(groupID uint64, desc, muteLabel string, onSuccess func(), ext ...*model.Server) {
	key := fmt.Sprintf("%d:%s", groupID, muteLabel)
	c.deliveryMu.Lock()
	if c.asyncInFlight == nil {
		c.asyncInFlight = make(map[string]struct{})
	}
	if _, busy := c.asyncInFlight[key]; busy {
		c.deliveryMu.Unlock()
		return
	}
	if time.Now().Before(c.asyncRetryAfter[key]) {
		c.deliveryMu.Unlock()
		return
	}
	c.asyncInFlight[key] = struct{}{}
	c.deliveryMu.Unlock()
	go func() {
		delivered := c.SendNotification(groupID, desc, muteLabel, ext...)
		if delivered && onSuccess != nil {
			onSuccess()
		}
		c.deliveryMu.Lock()
		if delivered {
			delete(c.asyncRetryAfter, key)
			delete(c.asyncRetryDelay, key)
		} else {
			if c.asyncRetryAfter == nil {
				c.asyncRetryAfter = make(map[string]time.Time)
				c.asyncRetryDelay = make(map[string]time.Duration)
			}
			delay := c.asyncRetryDelay[key]
			if delay == 0 {
				delay = 30 * time.Second
			} else {
				delay = min(delay*2, 15*time.Minute)
			}
			c.asyncRetryDelay[key] = delay
			c.asyncRetryAfter[key] = time.Now().Add(delay)
			c.pruneAsyncBackoffLocked()
		}
		delete(c.asyncInFlight, key)
		c.deliveryMu.Unlock()
	}()
}

const maxAsyncBackoffKeys = 4096

// Bound memory when a failing webhook sees many short-lived event labels.
// Evicted old event keys are no longer active after their phase changes.
func (c *NotificationClass) pruneAsyncBackoffLocked() {
	if len(c.asyncRetryAfter) <= maxAsyncBackoffKeys {
		return
	}
	now := time.Now()
	for key, until := range c.asyncRetryAfter {
		if !now.Before(until) {
			delete(c.asyncRetryAfter, key)
			delete(c.asyncRetryDelay, key)
		}
	}
	for key := range c.asyncRetryAfter {
		if len(c.asyncRetryAfter) <= maxAsyncBackoffKeys {
			break
		}
		delete(c.asyncRetryAfter, key)
		delete(c.asyncRetryDelay, key)
	}
}

type notificationKeyLock struct {
	mu   sync.Mutex
	refs int
}

func (c *NotificationClass) lockDeliveryKey(key string) func() {
	if key == "" {
		return func() {}
	}
	c.deliveryMu.Lock()
	if c.deliveryKeys == nil {
		c.deliveryKeys = make(map[string]*notificationKeyLock)
	}
	entry := c.deliveryKeys[key]
	if entry == nil {
		entry = new(notificationKeyLock)
		c.deliveryKeys[key] = entry
	}
	entry.refs++
	c.deliveryMu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		c.deliveryMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(c.deliveryKeys, key)
		}
		c.deliveryMu.Unlock()
	}
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
	func() {
		c.listMu.Lock()
		defer c.listMu.Unlock()

		_, ok := c.list[n.ID]
		c.list[n.ID] = n
		if !ok {
			return
		}

		gids := c.idToGroupList[n.ID]
		for gid := range gids {
			group, exists := c.groupToIDList[gid]
			if !exists {
				delete(gids, gid)
				continue
			}
			group[n.ID] = n
		}
		if len(gids) == 0 {
			delete(c.idToGroupList, n.ID)
		}
	}()
	c.sortList()
}

func (c *NotificationClass) UpdateGroup(ng *model.NotificationGroup, ngn []uint64) {
	c.groupMu.Lock()
	defer c.groupMu.Unlock()

	c.groupList[ng.ID] = ng.Name

	c.listMu.Lock()
	defer c.listMu.Unlock()

	oldList := c.groupToIDList[ng.ID]
	newList := make(map[uint64]*model.Notification, len(ngn))
	for _, nid := range ngn {
		n, ok := c.list[nid]
		if !ok {
			continue
		}
		newList[nid] = n
		if c.idToGroupList[nid] == nil {
			c.idToGroupList[nid] = make(map[uint64]struct{})
		}
		c.idToGroupList[nid][ng.ID] = struct{}{}
	}

	for oldID := range oldList {
		if _, ok := newList[oldID]; ok {
			continue
		}
		delete(c.idToGroupList[oldID], ng.ID)
		if len(c.idToGroupList[oldID]) == 0 {
			delete(c.idToGroupList, oldID)
		}
	}
	c.groupToIDList[ng.ID] = newList
}

func (c *NotificationClass) Delete(idList []uint64) {
	func() {
		c.listMu.Lock()
		defer c.listMu.Unlock()

		for _, id := range idList {
			delete(c.list, id)
			// 如果绑定了通知组才删除
			if gids, ok := c.idToGroupList[id]; ok {
				for gid := range gids {
					delete(c.groupToIDList[gid], id)
				}
				delete(c.idToGroupList, id)
			}
		}
	}()
	c.sortList()
}

func (c *NotificationClass) DeleteGroup(gids []uint64) {
	c.groupMu.Lock()
	defer c.groupMu.Unlock()
	c.listMu.Lock()
	defer c.listMu.Unlock()

	for _, gid := range gids {
		for nid := range c.groupToIDList[gid] {
			delete(c.idToGroupList[nid], gid)
			if len(c.idToGroupList[nid]) == 0 {
				delete(c.idToGroupList, nid)
			}
		}
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
	c.listMu.RLock()
	for id := range c.groupToIDList[notificationGroupID] {
		key := notificationMethodMuteLabel(fullMuteLabel, id)
		c.deliveryMu.Lock()
		if c.muteGeneration == nil {
			c.muteGeneration = make(map[string]uint64)
		}
		c.muteGeneration[key]++
		Cache.Delete(key)
		c.deliveryMu.Unlock()
	}
	c.listMu.RUnlock()
}

func notificationMethodMuteLabel(label string, id uint64) string {
	return fmt.Sprintf("%s:method-%d", label, id)
}

// SendNotification 向指定的通知方式组的所有通知方式发送通知
func (c *NotificationClass) SendNotification(notificationGroupID uint64, desc string, muteLabel string, ext ...*model.Server) bool {
	fullMuteLabel := ""
	if muteLabel != "" {
		fullMuteLabel = NotificationMuteLabel.AppendNotificationGroupName(muteLabel, c.GetGroupName(notificationGroupID))
	}
	// Copy the group under the lock. Webhook delivery can take minutes and must
	// not block notification or notification-group updates while it is in flight.
	c.listMu.RLock()
	notifications := make([]*model.Notification, 0, len(c.groupToIDList[notificationGroupID]))
	for _, n := range c.groupToIDList[notificationGroupID] {
		notifications = append(notifications, n)
	}
	c.listMu.RUnlock()

	if len(notifications) == 0 {
		log.Printf("NEZHA>> Notification group %d has no recipients", notificationGroupID)
		return false
	}
	allDelivered := true
	for _, n := range notifications {
		var server *model.Server
		if len(ext) > 0 {
			server = ext[0]
		}
		if !c.sendRecipient(n, desc, fullMuteLabel, server) {
			allDelivered = false
		}
	}
	return allDelivered
}

func (c *NotificationClass) sendRecipient(n *model.Notification, desc, fullMuteLabel string, server *model.Server) bool {
	if n == nil {
		return false
	}
	cacheKey := ""
	if fullMuteLabel != "" {
		cacheKey = notificationMethodMuteLabel(fullMuteLabel, n.ID)
	}
	unlock := c.lockDeliveryKey(cacheKey)
	defer unlock()
	var generation uint64
	if cacheKey != "" {
		c.deliveryMu.Lock()
		generation = c.muteGeneration[cacheKey]
		c.deliveryMu.Unlock()
	}
	var history NotificationHistory
	if cacheKey != "" {
		if value, exists := Cache.Get(cacheKey); exists {
			history = value.(NotificationHistory)
			if time.Now().Before(history.Until) {
				return true // this recipient already received this phase
			}
		}
	}
	log.Printf("NEZHA>> Try to notify %s", n.Name)
	var err error
	if c.sendForTest != nil {
		err = c.sendForTest(n, desc, server)
	} else {
		err = (&model.NotificationServerBundle{Notification: n, Server: server, Loc: Loc}).Send(desc)
	}
	if err != nil {
		// Webhook errors can contain a credential-bearing request URL. Keep
		// failure type without ever writing the URL/token to dashboard logs.
		log.Printf("NEZHA>> Sending notification to %s failed (%s)", n.Name, notificationFailureSummary(err))
		return false
	}
	log.Printf("NEZHA>> Sending notification to %s succeeded", n.Name)
	if cacheKey != "" {
		if history.Duration == 0 {
			history.Duration = firstNotificationDelay
		} else {
			history.Duration = min(history.Duration*2, 24*time.Hour)
		}
		history.Until = time.Now().Add(history.Duration)
		c.deliveryMu.Lock()
		if c.muteGeneration[cacheKey] == generation {
			Cache.Set(cacheKey, history, history.Duration+10*time.Minute)
		}
		c.deliveryMu.Unlock()
	}
	return true
}

func notificationFailureSummary(err error) string {
	// Even wrapped URL errors can include credentials in path/query. Log only
	// the error type; the HTTP status path already returns a dedicated type.
	return fmt.Sprintf("%T", err)
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
