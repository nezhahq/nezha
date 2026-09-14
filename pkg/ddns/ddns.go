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
}

func (provider *Provider) GetProfileID() uint64 {
	return provider.DDNSProfile.ID
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
			isDelete := provider.IPAddrs.IPv4Addr == ""
			actionName := utils.IfOr(isDelete, "Deleting", "Updating")

			for retries := 0; retries < maxRetries; retries++ {
				log.Printf("NEZHA>> %s IPv4 record of domain %s: %d/%d", actionName, domain, retries+1, maxRetries)
				var ipv4Err error
				if isDelete {
					ipv4Err = provider.deleteDomainRecord(ctx, prefix, zone, "A")
				} else {
					ipv4Err = provider.addDomainRecord(ctx, prefix, zone, "A", provider.IPAddrs.IPv4Addr)
				}

				if ipv4Err != nil {
					log.Printf("NEZHA>> Failed to %s IPv4 record of domain %s: %v", strings.ToLower(actionName), domain, ipv4Err)
				} else {
					log.Printf("NEZHA>> %s IPv4 record of domain %s succeeded", actionName, domain)
					break
				}
			}
		}

		// 独立处理 IPv6 更新或删除
		if provider.DDNSProfile.EnableIPv6 != nil && *provider.DDNSProfile.EnableIPv6 {
			isDelete := provider.IPAddrs.IPv6Addr == ""
			actionName := utils.IfOr(isDelete, "Deleting", "Updating")

			for retries := 0; retries < maxRetries; retries++ {
				log.Printf("NEZHA>> %s IPv6 record of domain %s: %d/%d", actionName, domain, retries+1, maxRetries)
				var ipv6Err error
				if isDelete {
					ipv6Err = provider.deleteDomainRecord(ctx, prefix, zone, "AAAA")
				} else {
					ipv6Err = provider.addDomainRecord(ctx, prefix, zone, "AAAA", provider.IPAddrs.IPv6Addr)
				}

				if ipv6Err != nil {
					log.Printf("NEZHA>> Failed to %s IPv6 record of domain %s: %v", strings.ToLower(actionName), domain, ipv6Err)
				} else {
					log.Printf("NEZHA>> %s IPv6 record of domain %s succeeded", actionName, domain)
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
	var recordsToDelete []libdns.Record

	if getter, okGetter := provider.Setter.(libdns.RecordGetter); okGetter {
		existingRecords, err := getter.GetRecords(ctx, zone)
		if err == nil {
			for _, rec := range existingRecords {
				rr := rec.RR()
				matchName := rr.Name == prefix || strings.TrimSuffix(rr.Name, ".") == strings.TrimSuffix(prefix, ".")
				if matchName && strings.ToUpper(rr.Type) == targetRecType {
					recordsToDelete = append(recordsToDelete, rec)
				}
			}
		}
	}

	if len(recordsToDelete) == 0 {
		recordsToDelete = []libdns.Record{
			libdns.RR{
				Name: prefix,
				Type: targetRecType,
			},
		}
	}

	_, err := deleter.DeleteRecords(ctx, zone, recordsToDelete)
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
