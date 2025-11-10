package ddns

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"time"

	"github.com/libdns/libdns"
	"github.com/miekg/dns"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

type DNSServerKey struct{}

const (
	dnsTimeOut = 10 * time.Second
)

type Provider struct {
	prefix string
	zone   string

	DDNSProfile *model.DDNSProfile
	IPAddrs     *model.IP
	Setter      libdns.RecordSetter
}

func (provider *Provider) GetProfileID() uint64 {
	return provider.DDNSProfile.ID
}

func (provider *Provider) UpdateDomain(ctx context.Context, overrideDomains ...string) {
	for _, domain := range utils.IfOr(len(overrideDomains) > 0, overrideDomains, provider.DDNSProfile.Domains) {
		for retries := range int(provider.DDNSProfile.MaxRetries) {
			log.Printf("NEZHA>> Updating DNS Record of domain %s: %d/%d", domain, retries+1, provider.DDNSProfile.MaxRetries)
			if err := provider.updateDomain(ctx, domain); err != nil {
				log.Printf("NEZHA>> Failed to update DNS record of domain %s: %v", domain, err)
			} else {
				log.Printf("NEZHA>> Update DNS record of domain %s succeeded", domain)
				break
			}
		}
	}
}

func (provider *Provider) updateDomain(ctx context.Context, domain string) error {
	var err error
	provider.prefix, provider.zone, err = provider.splitDomainSOA(ctx, domain)
	if err != nil {
		return err
	}

	// 当IPv4和IPv6同时成功才算作成功
	if *provider.DDNSProfile.EnableIPv4 {
		if err = provider.addDomainRecord(ctx, "A", provider.IPAddrs.IPv4Addr); err != nil {
			return err
		}
	}

	if *provider.DDNSProfile.EnableIPv6 {
		if err = provider.addDomainRecord(ctx, "AAAA", provider.IPAddrs.IPv6Addr); err != nil {
			return err
		}
	}

	return nil
}

func (provider *Provider) addDomainRecord(ctx context.Context, recType, addr string) error {
	netipAddr, err := netip.ParseAddr(addr)
	if err != nil {
		return fmt.Errorf("parse error: %v", err)
	}

	_, err = provider.Setter.SetRecords(ctx, provider.zone,
		[]libdns.Record{
			libdns.Address{
				Name: provider.prefix,
				IP:   netipAddr,
				TTL:  time.Minute,
			},
		})
	return err
}

func (provider *Provider) splitDomainSOA(ctx context.Context, domain string) (prefix string, zone string, err error) {
	c := &dns.Client{Timeout: dnsTimeOut}

	domain += "."
	indexes := dns.Split(domain)

	servers := utils.DNSServers

	customDNSServers, _ := ctx.Value(DNSServerKey{}).([]string)
	if len(customDNSServers) > 0 {
		servers = customDNSServers
	}

	for _, server := range servers {
		for _, idx := range indexes {
			var m dns.Msg
			m.SetQuestion(domain[idx:], dns.TypeSOA)

			r, _, err := c.Exchange(&m, server)
			if err != nil {
				continue
			}

			if len(r.Answer) > 0 {
				if soa, ok := r.Answer[0].(*dns.SOA); ok {
					zone := soa.Hdr.Name
					prefix := libdns.RelativeName(domain, zone)
					// Convert "@" to empty string for zone apex
					if prefix == "@" {
						prefix = ""
					}
					return prefix, zone, nil
				}
			}
		}
	}

	return "", "", fmt.Errorf("SOA record not found for domain: %s", domain)
}
