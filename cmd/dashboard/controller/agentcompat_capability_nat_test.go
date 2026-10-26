//go:build agentcompat

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

func TestAgentcompatNATCapabilityUsesExactCurrentProfilePermission(t *testing.T) {
	cleanup, userID := setupMCPTest(t)
	t.Cleanup(cleanup)
	oldUsers := singleton.UserInfoMap
	singleton.UserInfoMap = map[uint64]model.UserInfo{userID: {Role: model.RoleMember}, 999: {Role: model.RoleMember}}
	t.Cleanup(func() { singleton.UserInfoMap = oldUsers })
	singleton.Conf.DashboardHost = "panel.example"
	singleton.Conf.NATMemberDomains = []string{"private-profile.example", "foreign-profile.example"}
	handler := rpc.NewNezhaHandler()
	originalHandler := rpc.NezhaHandlerSingleton
	originalNAT := singleton.NATShared
	rpc.NezhaHandlerSingleton = handler
	t.Cleanup(func() {
		rpc.NezhaHandlerSingleton = originalHandler
		singleton.NATShared = originalNAT
	})
	require.NoError(t, singleton.DB.AutoMigrate(&model.NAT{}))
	secondServer := &model.Server{Common: model.Common{ID: 8}}
	secondServer.SetUserID(userID)
	singleton.ServerShared.InsertForTest(secondServer)
	profile := &model.NAT{Common: model.Common{ID: 41, UserID: userID}, Name: "private-profile", ServerID: 7, Domain: "private-profile.example"}
	foreignProfile := &model.NAT{Common: model.Common{ID: 42, UserID: 999}, Name: "foreign-profile", ServerID: 7, Domain: "foreign-profile.example"}
	require.NoError(t, singleton.DB.Create(profile).Error)
	require.NoError(t, singleton.DB.Create(foreignProfile).Error)
	singleton.NATShared = singleton.NewNATClass()
	token, plaintext := mkToken(t, userID, []string{model.ScopeNATRead}, []uint64{7, 8})
	server := newAgentcompatCapabilityServer(t)

	capability := registerAgentcompatCapability(t, server.URL, plaintext, agentcompatCapabilityRegisterRequest{Purpose: "nat", ServerID: 7, ResourceID: 41})
	status, mismatchBody := postAgentcompatEmpty(t, server.URL+agentcompatCapabilityRegisterPath, plaintext, agentcompatCapabilityRegisterRequest{Purpose: "nat", ServerID: 8, ResourceID: 41})
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, mismatchBody, errAgentcompatCapabilityUnavailable.Error())
	foreignStatus, foreignBody := postAgentcompatEmpty(t, server.URL+agentcompatCapabilityRegisterPath, plaintext, agentcompatCapabilityRegisterRequest{Purpose: "nat", ServerID: 7, ResourceID: 42})
	require.Equal(t, http.StatusOK, foreignStatus)
	require.Contains(t, foreignBody, errAgentcompatCapabilityUnavailable.Error())

	parsed, err := rpc.ParseAgentCompatIOStreamCapability(capability)
	require.NoError(t, err)
	access := rpc.AgentCompatCapabilityAccess{
		Capability: parsed, Owner: rpc.AgentCompatCapabilityOwner{PATID: token.ID, UserID: userID},
		Purpose: rpc.AgentCompatCapabilityNAT, TargetServerID: 7, ResourceID: 41, ServerAccessAllowed: true,
	}
	handle, err := handler.ConsumeAgentCompatNATCapability(access)
	require.NoError(t, err)
	lease, err := handler.CreateAgentCompatNATStream(handle, "private-nat-stream")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handler.CloseAgentCompatNATStreamLease(lease)) })
	require.NoError(t, handler.PublishAgentCompatNATStream(handle, rpc.AgentCompatNATPublication{Purpose: rpc.AgentCompatCapabilityNAT, TargetServerID: 7, ResourceID: 41, StreamID: "private-nat-stream"}))

	profile.ServerID = 8
	singleton.NATShared.Update(profile)
	_, unavailableErr := postAgentcompatCapability[agentcompatCapabilityWaitRequest, agentcompatCapabilityWaitResponse](context.Background(), server.URL+agentcompatCapabilityWaitPath, plaintext, agentcompatCapabilityWaitRequest{Capability: capability, Purpose: "nat", ServerID: 7, ResourceID: 41})
	require.Error(t, unavailableErr)
	require.NotContains(t, unavailableErr.Error(), capability)
	require.NotContains(t, unavailableErr.Error(), "private-nat-stream")
	profile.ServerID = 7
	singleton.NATShared.Update(profile)
	waitContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waited, err := postAgentcompatCapability[agentcompatCapabilityWaitRequest, agentcompatCapabilityWaitResponse](waitContext, server.URL+agentcompatCapabilityWaitPath, plaintext, agentcompatCapabilityWaitRequest{Capability: capability, Purpose: "nat", ServerID: 7, ResourceID: 41})
	require.NoError(t, err)
	require.Equal(t, "private-nat-stream", waited.StreamID)
}

func TestAgentcompatCapabilityOwnerIncludesCurrentAdminRole(t *testing.T) {
	cleanup, _ := setupMCPTest(t)
	t.Cleanup(cleanup)
	handler := rpc.NewNezhaHandler()
	originalHandler := rpc.NezhaHandlerSingleton
	rpc.NezhaHandlerSingleton = handler
	t.Cleanup(func() { rpc.NezhaHandlerSingleton = originalHandler })
	admin := &model.User{Common: model.Common{ID: 300}, Username: "cap-admin", Role: model.RoleAdmin}
	require.NoError(t, singleton.DB.Create(admin).Error)
	target := &model.Server{Common: model.Common{ID: 8}}
	target.SetUserID(999)
	singleton.ServerShared.InsertForTest(target)
	token, plaintext := mkDistinctCapabilityToken(t, admin.ID, "admin")
	token.SetServerIDs([]uint64{8})
	require.NoError(t, singleton.DB.Model(token).Update("servers_csv", token.ServersCSV).Error)
	server := newAgentcompatCapabilityServer(t)
	capability := registerAgentcompatCapability(t, server.URL, plaintext, agentcompatCapabilityRegisterRequest{Purpose: "terminal", ServerID: 8})
	parsed, err := rpc.ParseAgentCompatIOStreamCapability(capability)
	require.NoError(t, err)
	require.NoError(t, handler.CreateStreamWithPurpose("admin-private-stream", admin.ID, 8, rpc.PurposeTerminal))
	require.NoError(t, handler.BindAgentCompatIOStreamCapability(rpc.AgentCompatCapabilityBinding{
		AgentCompatCapabilityAccess: rpc.AgentCompatCapabilityAccess{
			Capability: parsed, Owner: rpc.AgentCompatCapabilityOwner{PATID: token.ID, UserID: admin.ID, IsAdmin: true},
			Purpose: rpc.AgentCompatCapabilityTerminal, TargetServerID: 8, ServerAccessAllowed: true,
		},
		StreamID: "admin-private-stream",
	}))
	waitContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waited, err := postAgentcompatCapability[agentcompatCapabilityWaitRequest, agentcompatCapabilityWaitResponse](waitContext, server.URL+agentcompatCapabilityWaitPath, plaintext, agentcompatCapabilityWaitRequest{Capability: capability, Purpose: "terminal", ServerID: 8})
	require.NoError(t, err)
	require.Equal(t, "admin-private-stream", waited.StreamID)
}
