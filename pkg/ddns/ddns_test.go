package ddns

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/libdns/libdns"
	"github.com/miekg/dns"

	"github.com/nezhahq/nezha/model"
)

type testSt struct {
	domain string
	zone   string
	prefix string
}

func TestSplitDomainSOA(t *testing.T) {
	handler := dns.NewServeMux()
	handler.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Authoritative = true
		if len(r.Question) > 0 {
			qname := r.Question[0].Name
			var zoneName string
			if strings.HasSuffix(qname, "example.co.uk.") {
				zoneName = "example.co.uk."
			} else if strings.HasSuffix(qname, "example.com.") {
				zoneName = "example.com."
			}

			if zoneName != "" {
				soa := &dns.SOA{
					Hdr:    dns.RR_Header{Name: zoneName, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 300},
					Ns:     "ns.example.com.",
					Mbox:   "admin.example.com.",
					Serial: 1,
				}
				m.Answer = append(m.Answer, soa)
				m.Ns = append(m.Ns, soa)
			}
		}
		_ = w.WriteMsg(m)
	})

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer pc.Close()

	server := &dns.Server{PacketConn: pc, Net: "udp", Handler: handler}
	go func() {
		_ = server.ActivateAndServe()
	}()
	defer server.Shutdown()

	addr := pc.LocalAddr().String()

	cases := []testSt{
		{
			domain: "www.example.co.uk",
			zone:   "example.co.uk.",
			prefix: "www",
		},
		{
			domain: "abc.example.com",
			zone:   "example.com.",
			prefix: "abc",
		},
		{
			domain: "example.com",
			zone:   "example.com.",
			prefix: "",
		},
	}

	ctx := context.WithValue(context.Background(), DNSServerKey{}, []string{addr})
	provider := &Provider{}
	for _, c := range cases {
		prefix, zone, err := provider.splitDomainSOA(ctx, c.domain)
		if err != nil {
			t.Fatalf("Error for domain %s: %s", c.domain, err)
		}
		if prefix != c.prefix {
			t.Fatalf("Expected prefix %s, but got %s", c.prefix, prefix)
		}
		if zone != c.zone {
			t.Fatalf("Expected zone %s, but got %s", c.zone, zone)
		}
	}
}

type MockSetter struct {
	Records         []libdns.Record
	ExistingRecords []libdns.Record
	GetCalled       bool
	SetCalled       bool
	DelCalled       bool
	DeletedRecords  []libdns.Record
}

func (m *MockSetter) GetRecords(ctx context.Context, zone string) ([]libdns.Record, error) {
	m.GetCalled = true
	return m.ExistingRecords, nil
}

func (m *MockSetter) SetRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	m.SetCalled = true
	m.Records = append(m.Records, records...)
	return records, nil
}

func (m *MockSetter) DeleteRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	m.DelCalled = true
	m.DeletedRecords = append(m.DeletedRecords, records...)
	return records, nil
}

type MockUnsupportedSetter struct{}

func (m *MockUnsupportedSetter) SetRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	return records, nil
}

func TestUnsupportedGetterDeleterSkip(t *testing.T) {
	provider := &Provider{
		DDNSProfile: &model.DDNSProfile{MaxRetries: 1},
		IPAddrs:     &model.IP{IPv4Addr: ""},
		Setter:      &MockUnsupportedSetter{},
	}

	err := provider.deleteDomainRecord(context.Background(), "sub", "example.com", "A")
	if err != nil {
		t.Fatalf("expected no error for unsupported deleter skip, got %v", err)
	}
}

func TestIndependentDualStackProcessing(t *testing.T) {
	enableIPv4 := true
	enableIPv6 := true
	mockSetter := &MockSetter{}

	provider := &Provider{
		DDNSProfile: &model.DDNSProfile{
			MaxRetries: 1,
			Domains:    []string{"example.com"},
			EnableIPv4: &enableIPv4,
			EnableIPv6: &enableIPv6,
		},
		IPAddrs: &model.IP{
			IPv4Addr: "1.1.1.1",
			IPv6Addr: "2001:db8::1",
		},
		Setter: mockSetter,
	}

	handler := dns.NewServeMux()
	handler.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Authoritative = true
		if len(r.Question) > 0 {
			soa := &dns.SOA{
				Hdr:    dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 300},
				Ns:     "ns.example.com.",
				Mbox:   "admin.example.com.",
				Serial: 1,
			}
			m.Answer = append(m.Answer, soa)
			m.Ns = append(m.Ns, soa)
		}
		_ = w.WriteMsg(m)
	})

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer pc.Close()

	server := &dns.Server{PacketConn: pc, Net: "udp", Handler: handler}
	go func() {
		_ = server.ActivateAndServe()
	}()
	defer server.Shutdown()

	ctx := context.WithValue(context.Background(), DNSServerKey{}, []string{pc.LocalAddr().String()})
	provider.UpdateDomain(ctx)

	if !mockSetter.SetCalled {
		t.Fatalf("expected SetRecords to have been called for dual stack updates")
	}

	foundIPv4 := false
	foundIPv6 := false
	for _, rec := range mockSetter.Records {
		if addrRec, ok := rec.(libdns.Address); ok {
			if addrRec.IP.String() == "1.1.1.1" {
				foundIPv4 = true
			}
			if addrRec.IP.String() == "2001:db8::1" {
				foundIPv6 = true
			}
		}
	}

	if !foundIPv4 || !foundIPv6 {
		t.Fatalf("expected both IPv4 and IPv6 records to be processed independently, got records: %v", mockSetter.Records)
	}
}

func TestTransientSOAFailureAndRetry(t *testing.T) {
	var attempts int64
	handler := dns.NewServeMux()
	handler.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		atomic.AddInt64(&attempts, 1)
		m := new(dns.Msg)
		m.SetReply(r)
		_ = w.WriteMsg(m)
	})

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer pc.Close()

	server := &dns.Server{PacketConn: pc, Net: "udp", Handler: handler}
	go func() {
		_ = server.ActivateAndServe()
	}()
	defer server.Shutdown()

	var maxRetries uint64 = 3
	provider := &Provider{
		DDNSProfile: &model.DDNSProfile{
			MaxRetries: maxRetries,
			Domains:    []string{"example.com"},
			EnableIPv4: &[]bool{true}[0],
			EnableIPv6: &[]bool{false}[0],
		},
		IPAddrs: &model.IP{IPv4Addr: "1.1.1.1"},
		Setter:  &MockSetter{},
	}

	ctx := context.WithValue(context.Background(), DNSServerKey{}, []string{pc.LocalAddr().String()})
	provider.UpdateDomain(ctx)

	actualAttempts := atomic.LoadInt64(&attempts)
	expectedAttempts := int64(maxRetries) * 2
	if actualAttempts != expectedAttempts {
		t.Fatalf("expected exact attempt count of %d, got %d", expectedAttempts, actualAttempts)
	}
}

func TestGenericRecordDeleterContract(t *testing.T) {
	mockSetter := &MockSetter{}
	provider := &Provider{
		DDNSProfile: &model.DDNSProfile{MaxRetries: 1},
		IPAddrs:     &model.IP{IPv4Addr: ""},
		Setter:      mockSetter,
	}

	ctx := context.Background()
	err := provider.deleteDomainRecord(ctx, "sub", "example.com", "A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !mockSetter.DelCalled {
		t.Fatalf("expected DeleteRecords to be called")
	}

	if len(mockSetter.DeletedRecords) != 1 {
		t.Fatalf("expected 1 record sent for deletion, got %d", len(mockSetter.DeletedRecords))
	}

	rr, ok := mockSetter.DeletedRecords[0].(libdns.RR)
	if !ok {
		t.Fatalf("expected deletion payload to be libdns.RR")
	}

	if rr.Name != "sub" || rr.Type != "A" {
		t.Fatalf("unexpected RR fields: Name=%s, Type=%s", rr.Name, rr.Type)
	}
}

func TestCloudflareSpecialDeletion(t *testing.T) {
	mockSetter := &MockSetter{
		ExistingRecords: []libdns.Record{
			libdns.Address{
				Name: "sub",
				IP:   netip.MustParseAddr("1.2.3.4"),
			},
		},
	}

	provider := &Provider{
		DDNSProfile: &model.DDNSProfile{
			MaxRetries: 1,
			Provider:   model.ProviderCloudflare,
		},
		IPAddrs: &model.IP{IPv4Addr: ""},
		Setter:  mockSetter,
	}

	ctx := context.Background()
	err := provider.deleteDomainRecord(ctx, "sub", "example.com", "A")
	if err != nil {
		t.Fatalf("unexpected error for Cloudflare special deletion: %v", err)
	}

	if !mockSetter.GetCalled {
		t.Fatalf("expected GetRecords to have been called for Cloudflare special flow")
	}

	if !mockSetter.DelCalled {
		t.Fatalf("expected DeleteRecords to have been called for Cloudflare special flow")
	}

	if len(mockSetter.DeletedRecords) != 1 {
		t.Fatalf("expected 1 record matched for deletion, got %d", len(mockSetter.DeletedRecords))
	}
}
