package singleton

import "github.com/nezhahq/nezha/model"

// NewEmptyServerClassForTest 构造一个不依赖 DB 的空 ServerClass，仅用于单测。
// 生产路径请用 NewServerClass。
func NewEmptyServerClassForTest() *ServerClass {
	sc := &ServerClass{
		class: class[uint64, *model.Server]{
			list: make(map[uint64]*model.Server),
		},
		uuidToID: make(map[string]uint64),
	}
	model.OwnerServerIDsLookup = sc.ownerServerIDs
	model.AllServerIDsLookup = sc.allServerIDs
	model.OwnerIsAdminLookup = ownerIsAdmin
	return sc
}

// InsertForTest 把一个 server 直接塞进内存表与排序快照，跳过 DB & InitServer 逻辑。
// 调用方需保证 server.ID 已经设置。
func (c *ServerClass) InsertForTest(s *model.Server) {
	c.lockLifecycleWrite()
	defer c.unlockLifecycleWrite()

	c.listMu.Lock()
	c.list[s.ID] = s
	if s.UUID != "" {
		c.uuidToID[s.UUID] = s.ID
	}
	c.listMu.Unlock()
	c.sortList()
}

// NewEmptyDDNSClassForTest 构造一个不依赖 DB 的空 DDNSClass，仅用于单测。
func NewEmptyDDNSClassForTest() *DDNSClass {
	return &DDNSClass{
		class: class[uint64, *model.DDNSProfile]{
			list: make(map[uint64]*model.DDNSProfile),
		},
	}
}

// InsertForTest 把一个 DDNS profile 直接塞进内存表，跳过 DB。
func (c *DDNSClass) InsertForTest(p *model.DDNSProfile) {
	c.listMu.Lock()
	c.list[p.ID] = p
	c.listMu.Unlock()
	c.sortList()
}

// NewEmptyNotificationClassForTest 构造空 NotificationClass。
func NewEmptyNotificationClassForTest() *NotificationClass {
	return &NotificationClass{
		class: class[uint64, *model.Notification]{
			list: make(map[uint64]*model.Notification),
		},
		groupToIDList: make(map[uint64]map[uint64]*model.Notification),
		idToGroupList: make(map[uint64]map[uint64]struct{}),
		groupList:     make(map[uint64]string),
	}
}

// InsertForTest 把一个 Notification 直接塞进内存表。
func (c *NotificationClass) InsertForTest(n *model.Notification) {
	c.listMu.Lock()
	c.list[n.ID] = n
	c.listMu.Unlock()
	c.sortList()
}
