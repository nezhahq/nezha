package singleton

import (
	"sync"

	"github.com/nezhahq/nezha/model"
	"gorm.io/gorm"
)

var (
	UserIdToAgentSecret map[uint64]string
	AgentSecretToUserId map[string]uint64

	UserRoleMap map[uint64]uint8

	UserLock sync.RWMutex
)

func initUser() {
	UserIdToAgentSecret = make(map[uint64]string)
	AgentSecretToUserId = make(map[string]uint64)
	UserRoleMap = make(map[uint64]uint8)

	var users []model.User
	DB.Find(&users)

	for _, u := range users {
		UserIdToAgentSecret[u.ID] = u.AgentSecret
		AgentSecretToUserId[u.AgentSecret] = u.ID
		UserRoleMap[u.ID] = u.Role
	}
}

func OnUserUpdate(u *model.User) {
	UserLock.Lock()
	defer UserLock.Unlock()

	if u == nil {
		return
	}

	UserIdToAgentSecret[u.ID] = u.AgentSecret
	AgentSecretToUserId[u.AgentSecret] = u.ID
	UserRoleMap[u.ID] = u.Role
}

func OnUserDelete(id []uint64, errorFunc func(string, ...interface{}) error) error {
	UserLock.Lock()
	defer UserLock.Unlock()

	if len(id) < 1 {
		return Localizer.ErrorT("user id not specified")
	}

	var (
		cron, server   bool
		crons, servers []uint64
	)

	for _, uid := range id {
		err := DB.Transaction(func(tx *gorm.DB) error {
			CronLock.RLock()
			crons = model.FindByUserID(CronList, uid)
			CronLock.RUnlock()

			cron = len(crons) > 0
			if cron {
				if err := tx.Unscoped().Delete(&model.Cron{}, "id in (?)", crons).Error; err != nil {
					return err
				}
			}

			SortedServerLock.RLock()
			servers = model.FindByUserID(SortedServerList, uid)
			SortedServerLock.RUnlock()

			server = len(servers) > 0
			if server {
				if err := tx.Unscoped().Delete(&model.Server{}, "id in (?)", servers).Error; err != nil {
					return err
				}
				if err := tx.Unscoped().Delete(&model.ServerGroupServer{}, "server_id in (?)", servers).Error; err != nil {
					return err
				}
			}

			if err := tx.Unscoped().Delete(&model.Transfer{}, "server_id in (?)", servers).Error; err != nil {
				return err
			}

			if err := tx.Where("id IN (?)", id).Delete(&model.User{}).Error; err != nil {
				return err
			}
			return nil
		})

		if err != nil {
			return errorFunc("%v", err)
		}

		if cron {
			OnDeleteCron(crons)
		}

		if server {
			AlertsLock.Lock()
			for _, sid := range servers {
				for _, alert := range Alerts {
					if AlertsCycleTransferStatsStore[alert.ID] != nil {
						delete(AlertsCycleTransferStatsStore[alert.ID].ServerName, sid)
						delete(AlertsCycleTransferStatsStore[alert.ID].Transfer, sid)
						delete(AlertsCycleTransferStatsStore[alert.ID].NextUpdate, sid)
					}
				}
			}
			AlertsLock.Unlock()
			OnServerDelete(servers)
		}

		secret := UserIdToAgentSecret[uid]
		delete(AgentSecretToUserId, secret)
		delete(UserIdToAgentSecret, uid)
	}

	if cron {
		UpdateCronList()
	}

	if server {
		ReSortServer()
	}

	return nil
}
