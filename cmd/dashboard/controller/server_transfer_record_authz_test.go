package controller

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func setupServerRecordTransferFixture(t *testing.T) func() {
	t.Helper()
	cleanup := setupRetryServerTransferFixture(t)
	originalDDNS := singleton.DDNSShared
	singleton.DDNSShared = singleton.NewEmptyDDNSClassForTest()
	require.NoError(t, singleton.DB.AutoMigrate(&model.ServerGroupServer{}, &model.Transfer{}))
	return func() {
		singleton.DDNSShared = originalDDNS
		cleanup()
	}
}

func editServerAs(uid uint64, role model.Role, serverID string) error {
	c := ctxAs(uid, role)
	bindJSON(c, model.ServerForm{Name: "renamed"})
	c.Params = gin.Params{{Key: "id", Value: serverID}}
	_, err := updateServer(c)
	return err
}

func deleteServerAs(uid uint64, role model.Role, serverID uint64) error {
	c := ctxAs(uid, role)
	bindJSON(c, []uint64{serverID})
	_, err := batchDeleteServer(c)
	return err
}

// Register flips Server.UserID before the agent has rotated, and a cancel,
// failure or timeout still returns the server to its previous owner. Until the
// transfer settles the destination may neither edit nor delete the record:
// the delete would drop the pending entry and free the UUID for re-enrolment.
func TestPendingTransferDestinationCannotEditOrDeleteServer(t *testing.T) {
	defer setupServerRecordTransferFixture(t)()
	seedServer(t, 1, 100)
	transfer, err := singleton.ServerTransferShared.InitiateExclusive(1, 100, 200, 1)
	require.NoError(t, err)

	require.Error(t, editServerAs(200, model.RoleMember, "1"))
	server, _ := singleton.ServerShared.Get(1)
	require.Equal(t, "seeded", server.Name)

	require.Error(t, deleteServerAs(200, model.RoleMember, 1))
	_, exists := singleton.ServerShared.Get(1)
	require.True(t, exists, "the pending server must survive the destination's delete")
	require.True(t, singleton.ServerTransferShared.HasPending(1), "and so must the pending entry that guards it")
	var rows int64
	require.NoError(t, singleton.DB.Model(&model.Server{}).Where("id = ?", 1).Count(&rows).Error)
	require.EqualValues(t, 1, rows)

	verified, _, err := singleton.ServerTransferShared.MarkVerified(1, transfer.ID)
	require.NoError(t, err)
	require.True(t, verified)
	require.NoError(t, editServerAs(200, model.RoleMember, "1"), "a settled transfer gives the new owner the record")
	require.NoError(t, deleteServerAs(200, model.RoleMember, 1))
}

// The gate is about the unconfirmed destination only: once a transfer is rolled
// back the record is the previous owner's again, even while the agent has not
// acknowledged the rollback, and admins are never held up.
func TestServerRecordGateLeavesPreviousOwnerAndAdminAlone(t *testing.T) {
	defer setupServerRecordTransferFixture(t)()
	seedServer(t, 1, 100)
	seedServer(t, 2, 100)
	cancelled, err := singleton.ServerTransferShared.InitiateExclusive(1, 100, 200, 1)
	require.NoError(t, err)
	_, err = singleton.ServerTransferShared.InitiateExclusive(2, 100, 200, 1)
	require.NoError(t, err)

	_, err = singleton.ServerTransferShared.Cancel(cancelled.ID)
	require.NoError(t, err)
	require.True(t, singleton.ServerControlTransitioning(1), "rollback not yet acknowledged by the agent")
	require.Error(t, deleteServerAs(200, model.RoleMember, 1))
	require.NoError(t, editServerAs(100, model.RoleMember, "1"))
	require.NoError(t, deleteServerAs(100, model.RoleMember, 1))

	require.NoError(t, editServerAs(1, model.RoleAdmin, "2"))
	require.NoError(t, deleteServerAs(1, model.RoleAdmin, 2))
	require.False(t, singleton.ServerTransferShared.HasPending(2))
}
