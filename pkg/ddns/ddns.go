package ddns

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"strings"
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
	DDNSProfile *model.DDNSProfile
	IPAddrs     *model.IP
	Setter      libdns.RecordSetter

	// Revoked, if set, reports whether the authority this update was started
	// with has been withdrawn. An update outlives the call that started it by
	// domains x retries x request timeout, so it is asked before every attempt.
	Revoked func() bool
}

func (provider *Provider) GetProfileID() uint64 {
	return provider.DDNSProfile.ID
}

func (provider *Provider) abandoned() bool {
	if provider.Revoked == nil || !provider.Revoked() {
		return false
	}
	log.Printf("NEZHA>> DDNS profile %d is no longer in force, abandoning the running update", provider.GetProfileID())
	return true
}

func (provider *Provider) UpdateDomain(ctx context.Context, overrideDomains ...string) {
	domains := utils.IfOr(len(overrideDomains) > 0, overrideDomains, provider.DDNSProfile.Domains)
	maxRetries := int(provider.DDNSProfile.MaxRetries)
	if maxRetries <= 0 {
		maxRetries = 1
	}

	for _, domain := range domains {
		var prefix, zone string
		var soaErr error

		for retries := 0; retries < maxRetries; retries++ {
			if provider.abandoned() {
				return
			}
			prefix, zone, soaErr = provider.splitDomainSOA(ctx, domain)
			if soaErr == nil {
				break
			}
			log.Printf("NEZHA>> Failed to split domain SOA for %s (attempt %d/%d): %v", domain, retries+1, maxRetries, soaErr)
		}

		if soaErr != nil {
			log.Printf("NEZHA>> Failed to split domain SOA for %s after %d retries, skipping domain", domain, maxRetries)
			continue
		}

		// 独立处理 IPv4 更新或删除
		if provider.DDNSProfile.EnableIPv4 != nil && *provider.DDNSProfile.EnableIPv4 {
			for retries := 0; retries < maxRetries; retries++ {
				if provider.abandoned() {
					return
				}
				log.Printf("NEZHA>> Updating IPv4 record of domain %s: %d/%d", domain, retries+1, maxRetries)
				var ipv4Err error
				if provider.IPAddrs.IPv4Addr == "" {
					ipv4Err = provider.deleteDomainRecord(ctx, prefix, zone, "A")
				} else {
					ipv4Err = provider.addDomainRecord(ctx, prefix, zone, "A", provider.IPAddrs.IPv4Addr)
				}

				if ipv4Err != nil {
					log.Printf("NEZHA>> Failed to update IPv4 record of domain %s: %v", domain, ipv4Err)
				} else {
					log.Printf("NEZHA>> Update IPv4 record of domain %s succeeded", domain)
					break
				}
			}
		}

		// 独立处理 IPv6 更新或删除
		if provider.DDNSProfile.EnableIPv6 != nil && *provider.DDNSProfile.EnableIPv6 {
			for retries := 0; retries < maxRetries; retries++ {
				if provider.abandoned() {
					return
				}
				log.Printf("NEZHA>> Updating IPv6 record of domain %s: %d/%d", domain, retries+1, maxRetries)
				var ipv6Err error
				if provider.IPAddrs.IPv6Addr == "" {
					ipv6Err = provider.deleteDomainRecord(ctx, prefix, zone, "AAAA")
				} else {
					ipv6Err = provider.addDomainRecord(ctx, prefix, zone, "AAAA", provider.IPAddrs.IPv6Addr)
				}

				if ipv6Err != nil {
					log.Printf("NEZHA>> Failed to update IPv6 record of domain %s: %v", domain, ipv6Err)
				} else {
					log.Printf("NEZHA>> Update IPv6 record of domain %s succeeded", domain)
					break
				}
			}
		}
	}
}

func (provider *Provider) addDomainRecord(ctx context.Context, prefix, zone, recType, addr string) error {
	netipAddr, err := netip.ParseAddr(addr)
	if err != nil {
		return fmt.Errorf("parse error: %v", err)
	}

	_, err = provider.Setter.SetRecords(ctx, zone,
		[]libdns.Record{
			libdns.Address{
				Name: prefix,
				IP:   netipAddr,
				TTL:  time.Minute,
			},
		})
	return err
}

func (provider *Provider) deleteDomainRecord(ctx context.Context, prefix, zone, recType string) error {
	deleter, okDeleter := provider.Setter.(libdns.RecordDeleter)
	if !okDeleter {
		log.Printf("NEZHA>> DNS provider does not support RecordDeleter, safely skipping deletion for %s", recType)
		return nil
	}

	targetRecType := strings.ToUpper(recType)
	_, err := deleter.DeleteRecords(ctx, zone, []libdns.Record{
		libdns.RR{
			Name: prefix,
			Type: targetRecType,
		},
	})
	if err != nil {
		return fmt.Errorf("deleter.DeleteRecords failed: %w", err)
	}

	log.Printf("NEZHA>> Successfully deleted %s record for %s.%s", recType, prefix, zone)
	return nil
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

			if r != nil && len(r.Answer) > 0 {
				if soa, ok := r.Answer[0].(*dns.SOA); ok {
					zoneName := soa.Hdr.Name
					pfx := libdns.RelativeName(domain, zoneName)
					if pfx == "@" {
						pfx = ""
					}
					return pfx, zoneName, nil
				}
			}
		}
	}

	return "", "", fmt.Errorf("SOA record not found for domain: %s", domain)
}
