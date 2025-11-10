package singleton

import (
	"cmp"
	"slices"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

type NATClass struct {
	class[string, *model.NAT]

	idToDomain map[uint64]string
}

func NewNATClass() *NATClass {
	var sortedList []*model.NAT

	DB.Find(&sortedList)
	list := make(map[string]*model.NAT, len(sortedList))
	idToDomain := make(map[uint64]string, len(sortedList))
	for _, profile := range sortedList {
		list[profile.Domain] = profile
		idToDomain[profile.ID] = profile.Domain
	}

	return &NATClass{
		class: class[string, *model.NAT]{
			list:       list,
			sortedList: sortedList,
		},
		idToDomain: idToDomain,
	}
}

func (c *NATClass) Update(n *model.NAT) {
	c.listMu.Lock()

	if oldDomain, ok := c.idToDomain[n.ID]; ok && oldDomain != n.Domain {
		delete(c.list, oldDomain)
	}

	c.list[n.Domain] = n
	c.idToDomain[n.ID] = n.Domain

	c.listMu.Unlock()
	c.sortList()
}

func (c *NATClass) Delete(idList []uint64) {
	c.listMu.Lock()

	for _, id := range idList {
		if domain, ok := c.idToDomain[id]; ok {
			delete(c.list, domain)
			delete(c.idToDomain, id)
		}
	}

	c.listMu.Unlock()
	c.sortList()
}

func (c *NATClass) GetNATConfigByDomain(domain string) *model.NAT {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	return c.list[domain]
}

func (c *NATClass) GetDomain(id uint64) string {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	return c.idToDomain[id]
}

func (c *NATClass) sortList() {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	sortedList := utils.MapValuesToSlice(c.list)
	slices.SortFunc(sortedList, func(a, b *model.NAT) int {
		return cmp.Compare(a.ID, b.ID)
	})

	c.sortedListMu.Lock()
	defer c.sortedListMu.Unlock()
	c.sortedList = sortedList
}
