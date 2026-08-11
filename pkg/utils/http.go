package utils

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// HttpClient / HttpClientSkipTlsVerify must not be used to dispatch
// requests to user-controlled URLs (SSRF risk, GHSA-6x26-5727-rrm9).
// For any attacker-controlled URL use NewRestrictedHTTPClient instead.
var (
	HttpClientSkipTlsVerify *http.Client
	HttpClient              *http.Client
)

var ErrHTTPURLTargetNotAllowed = errors.New("HTTP URL target is not allowed")

var blockedHTTPClientCIDRs = mustParseHTTPClientCIDRs([]string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::/128",
	"::1/128",
	"::ffff:0:0/96",
	"64:ff9b::/96",
	"64:ff9b:1::/48",
	"100::/64",
	"2001::/23",
	"2001:db8::/32",
	"2002::/16",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
})

func init() {
	HttpClientSkipTlsVerify = httpClient(_httpClient{
		Transport: httpTransport(_httpTransport{
			SkipVerifyTLS: true,
		}),
	})
	HttpClient = httpClient(_httpClient{
		Transport: httpTransport(_httpTransport{
			SkipVerifyTLS: false,
		}),
	})

	http.DefaultClient.Timeout = time.Minute * 10
}

type _httpTransport struct {
	SkipVerifyTLS bool
}

func httpTransport(conf _httpTransport) *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: conf.SkipVerifyTLS},
		Proxy:           http.ProxyFromEnvironment,
	}
}

type _httpClient struct {
	Transport *http.Transport
}

func httpClient(conf _httpClient) *http.Client {
	return &http.Client{
		Transport: conf.Transport,
		Timeout:   time.Minute * 10,
	}
}

func NewRestrictedHTTPClient(rawURL string, skipVerifyTLS bool) (*http.Client, error) {
	parsedURL, ip, err := ResolveAllowedHTTPURL(rawURL)
	if err != nil {
		return nil, err
	}
	return buildRestrictedHTTPClient(parsedURL, ip, skipVerifyTLS), nil
}

// buildRestrictedHTTPClient assembles a client whose DialContext is pinned to
// the already-vetted IP. Separated from NewRestrictedHTTPClient so tests can
// exercise the SNI / redirect behavior without relying on live DNS.
func buildRestrictedHTTPClient(parsedURL *url.URL, ip net.IP, skipVerifyTLS bool) *http.Client {
	port := parsedURL.Port()
	if port == "" {
		if parsedURL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	// Pin outbound webhooks to the vetted IP so DNS changes cannot retarget private hosts.
	targetAddress := net.JoinHostPort(ip.String(), port)
	dialer := &net.Dialer{}

	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, targetAddress)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: skipVerifyTLS, ServerName: parsedURL.Hostname()},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: time.Minute * 10,
	}
}

func ResolveAllowedHTTPURL(rawURL string) (*url.URL, net.IP, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, nil, ErrHTTPURLTargetNotAllowed
	}

	host := parsedURL.Hostname()
	if host == "" {
		return nil, nil, ErrHTTPURLTargetNotAllowed
	}
	if ip := net.ParseIP(host); ip != nil {
		if !HTTPURLTargetIPAllowed(ip) {
			return nil, nil, ErrHTTPURLTargetNotAllowed
		}
		return parsedURL, ip, nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, nil, err
	}
	if len(ips) == 0 {
		return nil, nil, ErrHTTPURLTargetNotAllowed
	}
	for _, ip := range ips {
		if !HTTPURLTargetIPAllowed(ip) {
			return nil, nil, ErrHTTPURLTargetNotAllowed
		}
	}

	return parsedURL, ips[0], nil
}

func HTTPURLTargetIPAllowed(ip net.IP) bool {
	parsedIP, ok := netipFromIP(ip)
	if !ok {
		return false
	}
	for _, cidr := range blockedHTTPClientCIDRs {
		if cidr.Contains(parsedIP) {
			return false
		}
	}
	return parsedIP.IsGlobalUnicast()
}

func netipFromIP(ip net.IP) (netip.Addr, bool) {
	parsedIP, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	return parsedIP.Unmap(), true
}

func mustParseHTTPClientCIDRs(cidrs []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		prefixes = append(prefixes, netip.MustParsePrefix(cidr))
	}
	return prefixes
}
