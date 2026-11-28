package controller

import (
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// Every create handler commits the row before it registers it in memory,
// and the SELECT-based checks cannot see a row committed after they ran.
// Either way the ownership check passes for another user's new row, so the
// DELETE itself must be limited to rows the caller may remove: a member
// looping over the next IDs must never remove another user's fresh row.
func TestBatchDeleteNeverRemovesOtherUsersFreshRows(t *testing.T) {
	setupCoverPATFixture(t)
	require.NoError(t, singleton.DB.AutoMigrate(
		&model.Notification{}, &model.NotificationGroupNotification{}, &model.NAT{},
		&model.AlertRule{}, &model.ServerGroup{}, &model.NotificationGroup{}, &model.ServerGroupServer{}, &model.Transfer{},
	))
	originalNAT := singleton.NATShared
	singleton.NATShared = singleton.NewNATClass()
	originalTransfer := singleton.ServerTransferShared
	singleton.ServerTransferShared = singleton.NewServerTransferClass()
	t.Cleanup(func() { singleton.NATShared, singleton.ServerTransferShared = originalNAT, originalTransfer })

	const victim, member = uint64(1), uint64(100)
	cases := []struct {
		name   string
		row    func(id, owner uint64) any
		delete func(*gin.Context) (any, error)
		// checkedBySelect: the handler checks ownership with a SELECT, so the
		// victim's row is committed right after that query (the race window).
		checkedBySelect bool
		// allOrNothing: the handler refuses the whole request when it names a
		// row the caller cannot delete (servers, so unknown and foreign IDs
		// look the same); the caller's own row is then deleted on its own.
		allOrNothing bool
	}{
		{"cron", func(id, owner uint64) any {
			return &model.Cron{Common: model.Common{ID: id, UserID: owner}, Name: "c", Scheduler: "@every 1h"}
		}, batchDeleteCron, false, false},
		{"service", func(id, owner uint64) any {
			return &model.Service{Common: model.Common{ID: id, UserID: owner}, Name: "s", Type: model.TaskTypeHTTPGet}
		}, batchDeleteService, false, false},
		{"notification", func(id, owner uint64) any {
			return &model.Notification{Common: model.Common{ID: id, UserID: owner}, Name: "n"}
		}, batchDeleteNotification, false, false},
		{"nat", func(id, owner uint64) any {
			return &model.NAT{Common: model.Common{ID: id, UserID: owner}, Name: "nat", Domain: "nat-" + strconv.FormatUint(id, 10) + ".example"}
		}, batchDeleteNAT, false, false},
		{"alert-rule", func(id, owner uint64) any {
			return &model.AlertRule{Common: model.Common{ID: id, UserID: owner}, Name: "a"}
		}, batchDeleteAlertRule, true, false},
		{"server-group", func(id, owner uint64) any {
			return &model.ServerGroup{Common: model.Common{ID: id, UserID: owner}, Name: "g"}
		}, batchDeleteServerGroup, true, false},
		{"notification-group", func(id, owner uint64) any {
			return &model.NotificationGroup{Common: model.Common{ID: id, UserID: owner}, Name: "ng"}
		}, batchDeleteNotificationGroup, true, false},
		{"server", func(id, owner uint64) any {
			return &model.Server{Common: model.Common{ID: id, UserID: owner}, Name: "srv", UUID: "uuid-" + strconv.FormatUint(id, 10)}
		}, batchDeleteServer, false, true},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			foreignID, ownID := uint64(500+index*2), uint64(501+index*2)
			foreign := tc.row(foreignID, victim)
			if tc.checkedBySelect {
				var inserted atomic.Bool
				callback := "insert_victim_row_after_check_" + tc.name
				require.NoError(t, singleton.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
					if inserted.CompareAndSwap(false, true) {
						require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Create(foreign).Error)
					}
				}))
				t.Cleanup(func() { _ = singleton.DB.Callback().Query().Remove(callback) })
			} else {
				// Committed but not registered in memory yet.
				require.NoError(t, singleton.DB.Create(foreign).Error)
			}
			require.NoError(t, singleton.DB.Create(tc.row(ownID, member)).Error)

			_, err := tc.delete(ctxAsMemberWithBody(member, []uint64{foreignID, ownID}))
			if tc.allOrNothing {
				require.EqualError(t, err, "permission denied")
				var count int64
				require.NoError(t, singleton.DB.Model(tc.row(0, 0)).Where("id = ?", ownID).Count(&count).Error)
				require.Equal(t, int64(1), count, "a refused request must not delete anything")
				_, err = tc.delete(ctxAsMemberWithBody(member, []uint64{ownID}))
			}
			require.NoError(t, err)

			var count int64
			require.NoError(t, singleton.DB.Model(tc.row(0, 0)).Where("id = ?", foreignID).Count(&count).Error)
			require.Equal(t, int64(1), count, "another user's row was deleted")
			require.NoError(t, singleton.DB.Model(tc.row(0, 0)).Where("id = ?", ownID).Count(&count).Error)
			require.Equal(t, int64(0), count, "the member's own row must still be deleted")
		})
	}
}
