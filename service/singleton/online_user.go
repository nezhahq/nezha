package singleton

import (
	"slices"
	"sync"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

var (
	OnlineUserMap     = make(map[string]*model.OnlineUser)
	OnlineUserMapLock sync.Mutex
)

func AddOnlineUser(connId string, user *model.OnlineUser) {
	OnlineUserMapLock.Lock()
	defer OnlineUserMapLock.Unlock()
	OnlineUserMap[connId] = user
}

func RemoveOnlineUser(connId string) {
	OnlineUserMapLock.Lock()
	defer OnlineUserMapLock.Unlock()
	delete(OnlineUserMap, connId)
}

func BlockByIPs(ipList []string) error {
	OnlineUserMapLock.Lock()
	defer OnlineUserMapLock.Unlock()

	for _, ip := range ipList {
		if err := model.BlockIP(DB, ip, model.WAFBlockReasonTypeManual, model.BlockIDManual); err != nil {
			return err
		}
		for _, user := range OnlineUserMap {
			if user.IP == ip && user.Conn != nil {
				user.Conn.Close()
			}
		}
	}

	return nil
}

func GetOnlineUsers(limit, offset int) []*model.OnlineUser {
	OnlineUserMapLock.Lock()
	defer OnlineUserMapLock.Unlock()
	users := utils.MapValuesToSlice(OnlineUserMap)
	slices.SortFunc(users, func(i, j *model.OnlineUser) int {
		return i.ConnectedAt.Compare(j.ConnectedAt)
	})
	if offset > len(users) {
		return nil
	}
	if offset+limit > len(users) {
		return users[offset:]
	}
	return users[offset : offset+limit]
}

func GetOnlineUserCount() int {
	OnlineUserMapLock.Lock()
	defer OnlineUserMapLock.Unlock()
	return len(OnlineUserMap)
}
