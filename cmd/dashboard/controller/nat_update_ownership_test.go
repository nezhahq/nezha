package controller

import (
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// updateNAT checks two owners: the target server and the NAT profile being
// edited. Only the first one was covered; with the profile check removed a
// member could re-point another member's tunnel at a server of their own and
// every test stayed green.
func TestUpdateNATRejectsProfileOwnedByAnotherMember(t *testing.T) {
	defer setupTenancyTest(t)()
	oldConf, oldNAT, oldUsers := singleton.Conf, singleton.NATShared, singleton.UserInfoMap
	defer func() { singleton.Conf, singleton.NATShared, singleton.UserInfoMap = oldConf, oldNAT, oldUsers }()
	singleton.UserInfoMap = map[uint64]model.UserInfo{10: {Role: model.RoleMember}, 20: {Role: model.RoleMember}}
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{DashboardHost: "panel.example.invalid"}, NATMemberDomains: []string{"victim.example.invalid"}}}
	require.NoError(t, singleton.DB.AutoMigrate(&model.NAT{}))
	singleton.NATShared = singleton.NewNATClass()
	singleton.ServerShared.InsertForTest(&model.Server{Common: model.Common{ID: 1, UserID: 10}, Name: "victim-server"})
	singleton.ServerShared.InsertForTest(&model.Server{Common: model.Common{ID: 2, UserID: 20}, Name: "attacker-server"})

	victimProfileID, err := createNAT(ctxAsMemberWithBody(10, map[string]any{"name": "victim-tunnel", "enabled": true, "server_id": 1, "host": "203.0.113.7:80", "domain": "victim.example.invalid"}))
	require.NoError(t, err)

	hijack := ctxAsMemberWithBody(20, map[string]any{"name": "victim-tunnel", "enabled": true, "server_id": 2, "host": "203.0.113.9:80", "domain": "victim.example.invalid"})
	hijack.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(victimProfileID, 10)}}
	_, err = updateNAT(hijack)

	require.Error(t, err, "a member must not edit another member's NAT profile")
	var stored model.NAT
	require.NoError(t, singleton.DB.First(&stored, victimProfileID).Error)
	require.Equal(t, uint64(1), stored.ServerID, "the profile must still point at its owner's server")
	require.Equal(t, "203.0.113.7:80", stored.Host)
	routed := singleton.NATShared.GetNATConfigByDomain("victim.example.invalid")
	require.NotNil(t, routed)
	require.Equal(t, uint64(1), routed.ServerID, "live routing must still use the owner's server")

	// Control: the owner can still edit it.
	ownerEdit := ctxAsMemberWithBody(10, map[string]any{"name": "victim-tunnel", "enabled": true, "server_id": 1, "host": "203.0.113.8:80", "domain": "victim.example.invalid"})
	ownerEdit.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(victimProfileID, 10)}}
	_, err = updateNAT(ownerEdit)
	require.NoError(t, err)
}
