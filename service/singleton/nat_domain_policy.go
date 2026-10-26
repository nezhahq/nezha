package singleton

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/nezhahq/nezha/model"
)

// NATDomainPolicy is one consistent view of member grants and panel reservations.
type NATDomainPolicy struct {
	DashboardHost string
	InstallHost   string
	ListenHost    string
	ReservedHosts string
	MemberDomains []string
}

func (p NATDomainPolicy) isReserved(domain string) bool {
	target := splitDashboardHostname(domain)
	if target == "" {
		return false
	}
	hosts := []string{p.InstallHost, p.DashboardHost, p.ListenHost}
	hosts = append(hosts, strings.Split(p.ReservedHosts, ",")...)
	for _, host := range hosts {
		if reserved := splitDashboardHostname(host); reserved != "" && reserved == target {
			return true
		}
	}
	return false
}

func (c *ConfigClass) NATDomainPolicySnapshot() NATDomainPolicy {
	c.natDomainPolicyMu.RLock()
	defer c.natDomainPolicyMu.RUnlock()
	return NATDomainPolicy{
		DashboardHost: c.DashboardHost, InstallHost: c.InstallHost,
		ListenHost: c.ListenHost, ReservedHosts: c.ReservedHosts,
		MemberDomains: slices.Clone(c.NATMemberDomains),
	}
}

func (c *ConfigClass) CommitNATDomainPolicy(form model.SettingForm, persist func() error) error {
	// Runtime routing must not observe a grant before Save succeeds, or retain it
	// after a failed write. Keep host reservations and grants in the same commit.
	c.natDomainPolicyMu.Lock()
	defer c.natDomainPolicyMu.Unlock()
	dashboard, install, reserved, domains := c.DashboardHost, c.InstallHost, c.ReservedHosts, c.NATMemberDomains
	c.DashboardHost, c.InstallHost, c.ReservedHosts = form.DashboardHost, form.InstallHost, form.ReservedHosts
	// Older clients omit the field. Only an explicit [] revokes every member grant.
	if form.NATMemberDomains != nil {
		c.NATMemberDomains = slices.Clone(form.NATMemberDomains)
	}
	if err := persist(); err != nil {
		c.DashboardHost, c.InstallHost, c.ReservedHosts, c.NATMemberDomains = dashboard, install, reserved, domains
		return err
	}
	return nil
}

func ValidateNATMemberDomains(domains []string, dashboard, install, reserved, listen string) ([]string, error) {
	if domains == nil {
		return nil, nil
	}
	policy := NATDomainPolicy{DashboardHost: dashboard, InstallHost: install, ReservedHosts: reserved, ListenHost: listen}
	normalized := make([]string, 0, len(domains))
	for _, domain := range domains {
		// Validate the input before SplitHostPort, which also accepts URL-like ports.
		if strings.ContainsAny(strings.TrimSpace(domain), "/\\@?#*% \t\r\n") {
			return nil, fmt.Errorf("nat_member_domains must contain exact hostnames without wildcards or URLs")
		}
		host := splitDashboardHostname(domain)
		if !validNATMemberHostname(host) {
			return nil, fmt.Errorf("nat_member_domains must contain exact hostnames without wildcards or URLs")
		}
		if policy.isReserved(host) {
			return nil, fmt.Errorf("nat_member_domains must not include a reserved dashboard host")
		}
		if !slices.Contains(normalized, host) {
			normalized = append(normalized, host)
		}
	}
	if len(normalized) > 0 && !validNATMemberHostname(splitDashboardHostname(dashboard)) {
		return nil, fmt.Errorf("dashboard_host must be configured before enabling member NAT")
	}
	return normalized, nil
}

func validNATMemberHostname(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Zone() == ""
	}
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}
