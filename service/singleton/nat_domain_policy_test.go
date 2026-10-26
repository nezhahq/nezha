package singleton

import (
	"errors"
	"sync"
	"testing"

	"github.com/nezhahq/nezha/model"
	"github.com/stretchr/testify/require"
)

func TestNATDomainPolicyRollbackAndConcurrentSnapshots(t *testing.T) {
	conf := &ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{DashboardHost: "panel.example.com"}, NATMemberDomains: []string{"tunnel.example.com"}}}
	before := conf.NATDomainPolicySnapshot()
	next := model.SettingForm{DashboardHost: "console.example.com", NATMemberDomains: []string{"files.example.com"}}
	failure := errors.New("configuration storage unavailable")
	require.ErrorIs(t, conf.CommitNATDomainPolicy(next, func() error { return failure }), failure)
	require.Equal(t, before, conf.NATDomainPolicySnapshot())
	snapshot := conf.NATDomainPolicySnapshot()
	snapshot.MemberDomains[0] = "changed.example.com"
	require.Equal(t, before, conf.NATDomainPolicySnapshot(), "snapshots must not alias live grants")
	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		workers.Go(func() {
			for iteration := 0; iteration < 100; iteration++ {
				if err := conf.CommitNATDomainPolicy(next, func() error { return nil }); err != nil {
					t.Error(err)
				}
				policy := conf.NATDomainPolicySnapshot()
				if policy.DashboardHost != "console.example.com" || len(policy.MemberDomains) != 1 || policy.MemberDomains[0] != "files.example.com" {
					t.Errorf("inconsistent policy: %+v", policy)
				}
				settings := conf.SettingsSnapshot()
				if settings.DashboardHost != policy.DashboardHost || len(settings.NATMemberDomains) != 1 || settings.NATMemberDomains[0] != "files.example.com" {
					t.Errorf("inconsistent settings: %+v", settings)
				}
			}
		})
	}
	workers.Wait()
}

func TestValidateNATMemberDomains(t *testing.T) {
	for _, domain := range []string{"*.example.com", "https://example.com", "member.example.com/path", "user@example.com", "member.example.com?query", "member.example.com#fragment", "-invalid.example.com", "invalid..example.com", "fe80::1%eth0"} {
		_, err := ValidateNATMemberDomains([]string{domain}, "panel.example.com", "", "", "")
		require.Error(t, err, domain)
	}
	_, err := ValidateNATMemberDomains([]string{"member.example.com"}, "", "", "", "")
	require.Error(t, err, "an allowlist requires a declared dashboard host")
	for _, host := range []string{"panel.example.com", "agent.example.com", "admin.example.com", "127.0.0.1"} {
		_, err := ValidateNATMemberDomains([]string{host}, "panel.example.com", "agent.example.com", "admin.example.com", "127.0.0.1")
		require.Error(t, err, "every configured panel entrance is reserved")
	}
	domains, err := ValidateNATMemberDomains([]string{"Tunnel.Example.COM.:443", "tunnel.example.com", "[2001:db8::1]:8080"}, "panel.example.com", "", "", "")
	require.NoError(t, err)
	require.Equal(t, []string{"tunnel.example.com", "2001:db8::1"}, domains)
}
