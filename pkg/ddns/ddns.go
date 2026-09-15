package ddns

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"sync"
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

type cfRecordCacheEntry struct {
	records   []libdns.Record
	timestamp time.Time
}

var (
	cfCacheMux    sync.Mutex
	cfRecordCache = make(map[string]cfRecordCacheEntry)
)

const (
	cfCacheTTL   = 2 * time.Second
	maxCacheSize = 100
)

func getCachedCloudflareRecords(ctx context.Context, getter libdns.RecordGetter, profileID uint64, zone string) ([]libdns.Record, error) {

	cacheKey := fmt.Sprintf("%d:%s", profileID, zone)

	cfCacheMux.Lock()
	if entry, ok := cfRecordCache[cacheKey]; ok {
		if time.Since(entry.timestamp) < cfCacheTTL {
			cfCacheMux.Unlock()
			log.Printf("NEZHA>> [Cloudflare Special] Hit local short-term cache for profile %d zone %s, skipping duplicate GetRecords API call", profileID, zone)
			return entry.records, nil
		}
	}
	cfCacheMux.Unlock()

	records, err := getter.GetRecords(ctx, zone)
	if err != nil {
		return nil, err
	}

	cfCacheMux.Lock()
	if len(cfRecordCache) > maxCacheSize {
		now := time.Now()
		for k, entry := range cfRecordCache {
			if now.Sub(entry.timestamp) > cfCacheTTL {
				delete(cfRecordCache, k)
			}
		}
	}
	cfRecordCache[cacheKey] = cfRecordCacheEntry{
		records:   records,
		timestamp: time.Now(),
	}
	cfCacheMux.Unlock()

	return records, nil
}

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
			for retries := 0; retries < maxRetries; retries++ {
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

	if provider.DDNSProfile.Provider == model.ProviderCloudflare {
		log.Printf("NEZHA>> [Cloudflare Special] Applying Cloudflare specific parameters for record %s.%s (Type: %s)", prefix, zone, recType)
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
	targetRecType := strings.ToUpper(recType)

	// 临时兼容 Cloudflare
	if provider.DDNSProfile.Provider == model.ProviderCloudflare {
		log.Printf("NEZHA>> [Cloudflare Special] Executing specialized deletion logic for %s record on %s.%s", targetRecType, prefix, zone)

		getter, okGetter := provider.Setter.(libdns.RecordGetter)
		deleter, okDeleter := provider.Setter.(libdns.RecordDeleter)
		if !okGetter || !okDeleter {
			log.Printf("NEZHA>> DNS provider does not support record getting or deletion, safely skipping deletion for %s", recType)
			return nil
		}

		cleanName := func(name string) string {
			return strings.ToLower(strings.TrimSuffix(name, "."))
		}
		cleanPrefix := cleanName(prefix)

		allRecords, err := getCachedCloudflareRecords(ctx, getter, provider.GetProfileID(), zone)
		if err != nil {
			return fmt.Errorf("cloudflare failed to get DNS records: %w", err)
		}

		cleanZone := cleanName(zone)
		var targetRecords []libdns.Record
		for _, rec := range allRecords {
			rr := rec.RR()
			if strings.ToUpper(rr.Type) != targetRecType {
				continue
			}

			relName := libdns.RelativeName(rr.Name, zone)
			cleanRel := cleanName(relName)
			cleanRRName := cleanName(rr.Name)

			isApex := cleanRel == "" || cleanRel == "@" || cleanRRName == cleanZone

			var matchedName bool
			if cleanPrefix == "" {
				matchedName = isApex
			} else {
				matchedName = cleanRel == cleanPrefix || cleanRRName == cleanPrefix
			}

			if matchedName {
				targetRecords = append(targetRecords, rec)
			}
		}

		if len(targetRecords) == 0 {
			log.Printf("NEZHA>> [Cloudflare Special] No matching %s record found for deletion under zone %s, already clean", recType, zone)
			return nil
		}

		_, err = deleter.DeleteRecords(ctx, zone, targetRecords)
		if err != nil {
			return fmt.Errorf("cloudflare deleter.DeleteRecords failed: %w", err)
		}

		log.Printf("NEZHA>> [Cloudflare Special] Successfully deleted %d matching %s record(s) for %s.%s", len(targetRecords), recType, prefix, zone)
		return nil
	}

	// 原生
	deleter, okDeleter := provider.Setter.(libdns.RecordDeleter)
	if !okDeleter {
		log.Printf("NEZHA>> DNS provider does not support RecordDeleter, safely skipping deletion for %s", recType)
		return nil
	}

	_, err := deleter.DeleteRecords(ctx, zone, []libdns.Record{
		libdns.RR{
			Name: prefix,
			Type: targetRecType,
		},
	})
	if err != nil {
		return fmt.Errorf("deleter.DeleteRecords failed: %w", err)
	}

	log.Printf("NEZHA>> Successfully deleted %s record for %s.%s via standard libdns contract", recType, prefix, zone)
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
