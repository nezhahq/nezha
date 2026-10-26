package singleton

import (
	"github.com/nezhahq/nezha/model"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNATRouteRejectsUnauthorizedDisabledProfiles(t *testing.T) {
	withReservedHostConf(t, &model.Config{ConfigDashboard: model.ConfigDashboard{DashboardHost: "panel.example"}, NATMemberDomains: []string{"member.example"}})
	oldServers, oldUsers := ServerShared, UserInfoMap
	ServerShared = NewEmptyServerClassForTest()
	UserInfoMap = map[uint64]model.UserInfo{100: {Role: model.RoleMember}}
	t.Cleanup(func() { ServerShared, UserInfoMap = oldServers, oldUsers })
	server := &model.Server{Common: model.Common{ID: 7, UserID: 100}}
	ServerShared.InsertForTest(server)
	profile := &model.NAT{Common: model.Common{ID: 1, UserID: 100}, ServerID: 7, Domain: "member.example", Enabled: false}
	routes := &NATClass{class: class[string, *model.NAT]{list: map[string]*model.NAT{profile.Domain: profile}}}
	require.Same(t, profile, routes.GetNATConfigByDomain(profile.Domain), "authorized disabled profile retains its intentional disabled response")
	server.SetUserID(200)
	require.Nil(t, routes.GetNATConfigByDomain(profile.Domain), "a former owner's disabled profile must not shadow the panel")
	server.SetUserID(100)
	Conf.NATMemberDomains = nil
	require.Nil(t, routes.GetNATConfigByDomain(profile.Domain))
	Conf.NATMemberDomains = []string{profile.Domain}
	Conf.ReservedHosts = profile.Domain
	require.Nil(t, routes.GetNATConfigByDomain(profile.Domain))
	Conf.ReservedHosts = ""
	delete(UserInfoMap, 100)
	require.Nil(t, routes.GetNATConfigByDomain(profile.Domain))
}

func TestMemberNATRequiresExplicitDomainAndDashboardHost(t *testing.T) {
	withReservedHostConf(t, &model.Config{NATMemberDomains: []string{"member.example"}})
	require.False(t, NATDomainAllowed("member.example", false))
	Conf.DashboardHost = "panel.example"
	require.True(t, NATDomainAllowed("MEMBER.EXAMPLE.:443", false))
	require.False(t, NATDomainAllowed("sub.member.example", false))
	require.False(t, NATDomainAllowed("panel.example", true))
	require.True(t, NATDomainAllowed("admin-tunnel.example", true))
}
