package singleton

import (
	"sync"

	"github.com/nezhahq/nezha/model"
	"gorm.io/gorm"
)

var (
	UserInfoMap         map[uint64]model.UserInfo
	AgentSecretToUserId map[string]uint64

	UserLock sync.RWMutex
)

func initUser() {
	UserInfoMap = make(map[uint64]model.UserInfo)
	AgentSecretToUserId = make(map[string]uint64)

	var users []model.User
	DB.Find(&users)

	// for backward compatibility
	UserInfoMap[0] = model.UserInfo{
		Role:        model.RoleAdmin,
		AgentSecret: Conf.AgentSecretKey,
	}
	AgentSecretToUserId[Conf.AgentSecretKey] = 0

	for _, u := range users {
		UserInfoMap[u.ID] = model.UserInfo{
			Role:        u.Role,
			AgentSecret: u.AgentSecret,
		}
		AgentSecretToUserId[u.AgentSecret] = u.ID
	}
}

func OnUserUpdate(u *model.User) {
	UserLock.Lock()
	defer UserLock.Unlock()

	if u == nil {
		return
	}

	UserInfoMap[u.ID] = model.UserInfo{
		Role:        u.Role,
		AgentSecret: u.AgentSecret,
	}
	AgentSecretToUserId[u.AgentSecret] = u.ID
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

		secret := UserInfoMap[uid].AgentSecret
		delete(AgentSecretToUserId, secret)
		delete(UserInfoMap, uid)
	}

	if cron {
		UpdateCronList()
	}

	if server {
		ReSortServer()
	}

	return nil
}
