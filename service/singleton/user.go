package singleton

import (
	"fmt"
	"sync"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
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

	// Backward compatibility for pre-user-scoped Agents. AgentSecretKey is a
	// deployment-wide migration/master credential, so user 0 is intentionally
	// not tenant-scoped. Do not remove this mapping until every legacy Agent has
	// rotated to a per-user/per-Agent credential; doing so would disconnect them.
	UserInfoMap[0] = model.UserInfo{
		Role:        model.RoleAdmin,
		AgentSecret: Conf.AgentSecretKey,
	}
	AgentSecretToUserId[Conf.AgentSecretKey] = 0

	for _, u := range users {
		if u.AgentSecret == "" {
			u.AgentSecret = utils.MustGenerateRandomString(model.DefaultAgentSecretLength)
			if err := DB.Save(&u).Error; err != nil {
				panic(fmt.Errorf("update of user %d failed: %v", u.ID, err))
			}
		}

		UserInfoMap[u.ID] = model.UserInfo{
			Role:        u.Role,
			Username:    u.Username,
			AgentSecret: u.AgentSecret,
		}
		AgentSecretToUserId[u.AgentSecret] = u.ID
	}

	model.ServerOwnerLookup = lookupServerOwner
}

// lookupServerOwner resolves Server.UserID into a display-ready owner
// record for model.Server.MarshalJSON. uid=0 is the legacy global agent
// secret (a pseudo-owner with no User row) and intentionally returns
// ok=false with no username; the frontend renders it as "Global". Other
// uids return ok=false when the user has been deleted, so the JSON still
// carries the bare id and the frontend can render an "Unknown (#<uid>)"
// placeholder. RLock is required because OnUserUpdate / OnUserDelete may
// mutate UserInfoMap concurrently with serialization.
func lookupServerOwner(uid uint64) (model.ServerOwnerInfo, bool) {
	if uid == 0 {
		return model.ServerOwnerInfo{}, false
	}
	UserLock.RLock()
	info, ok := UserInfoMap[uid]
	UserLock.RUnlock()
	if !ok {
		return model.ServerOwnerInfo{}, false
	}
	return model.ServerOwnerInfo{ID: uid, Username: info.Username}, true
}

func OnUserUpdate(u *model.User) {
	UserLock.Lock()
	defer UserLock.Unlock()

	if u == nil {
		return
	}

	UserInfoMap[u.ID] = model.UserInfo{
		Role:        u.Role,
		Username:    u.Username,
		AgentSecret: u.AgentSecret,
	}
	AgentSecretToUserId[u.AgentSecret] = u.ID
}

func OnUserDelete(id []uint64, errorFunc func(string, ...any) error) error {
	// Runs after UserLock is released: unregistering scheduled tasks takes time
	// per task, and every Agent request and alert check waits for UserLock.
	// Until then a task that fires covers nothing, because its owner is gone.
	var deletedCrons []uint64
	defer func() {
		if len(deletedCrons) > 0 {
			CronShared.Delete(deletedCrons)
		}
	}()

	if len(id) < 1 {
		return Localizer.ErrorT("user id not specified")
	}

	// A server on its way to a deleted user (Pending transfer) is already
	// attributed to them but still belongs to its source user. Hand it back
	// before the loop below picks the servers to delete by UserID. This runs
	// before UserLock is taken: the rollback waits for the server control
	// lock and pushes the revert to an online agent.
	if ServerTransferShared != nil {
		if err := ServerTransferShared.ReturnPendingToSource(id); err != nil {
			return errorFunc("%v", err)
		}
	}

	UserLock.Lock()
	defer UserLock.Unlock()

	if ServerTransferShared != nil {
		ServerTransferShared.OnUsersDeleted(id)
	}

	var (
		cron, server   bool
		crons, servers []uint64
	)

	slist := ServerShared.GetSortedList()
	clist := CronShared.GetSortedList()
	for _, uid := range id {
		err := DB.Transaction(func(tx *gorm.DB) error {
			crons = model.FindByUserID(clist, uid)
			cron = len(crons) > 0
			if cron {
				if err := tx.Unscoped().Delete(&model.Cron{}, "id in (?)", crons).Error; err != nil {
					return err
				}
			}

			servers = model.FindByUserID(slist, uid)
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

			if err := tx.Where("id = ?", uid).Delete(&model.User{}).Error; err != nil {
				return err
			}
			return nil
		})

		if err != nil {
			return errorFunc("%v", err)
		}

		if cron {
			deletedCrons = append(deletedCrons, crons...)
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
			// Cancel pending transfers before ServerShared drops the
			// in-memory entry: same ordering rationale as batchDeleteServer.
			if ServerTransferShared != nil {
				ServerTransferShared.OnServersDeleted(servers)
			}
			ServerShared.Delete(servers)
		}

		secret := UserInfoMap[uid].AgentSecret
		delete(AgentSecretToUserId, secret)
		delete(UserInfoMap, uid)
	}
	return nil
}
