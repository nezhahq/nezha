package webhook

import (
	"context"
	"encoding/json"
	"maps"
	"net/netip"
	"strings"
	"testing"

	"github.com/libdns/libdns"

	"github.com/nezhahq/nezha/model"
)

var (
	reqTypeForm = "application/x-www-form-urlencoded"
	reqTypeJSON = "application/json"
)

type testSt struct {
	profile           model.DDNSProfile
	expectURL         string
	expectBody        string
	expectContentType string
	expectHeader      map[string]string
}

func execCase(t *testing.T, item testSt) {
	pw := Provider{DDNSProfile: &item.profile}
	pw.ipAddr = "1.1.1.1"
	pw.domain = item.profile.Domains[0]
	pw.ipType = "ipv4"
	pw.recordType = "A"
	pw.DDNSProfile = &item.profile

	reqUrl, err := pw.reqUrl()
	if err != nil {
		t.Fatalf("Error: %s", err)
	}
	if item.expectURL != reqUrl.String() {
		t.Fatalf("Expected %s, but got %s", item.expectURL, reqUrl.String())
	}

	reqBody, err := pw.reqBody()
	if err != nil {
		t.Fatalf("Error: %s", err)
	}
	if item.expectBody != reqBody {
		t.Fatalf("Expected %s, but got %s", item.expectBody, reqBody)
	}

	req, _, err := pw.prepareRequest(context.Background())
	if err != nil {
		t.Fatalf("Error: %s", err)
	}

	if item.expectContentType != req.Header.Get("Content-Type") {
		t.Fatalf("Expected %s, but got %s", item.expectContentType, req.Header.Get("Content-Type"))
	}

	for k, v := range item.expectHeader {
		if v != req.Header.Get(k) {
			t.Fatalf("Expected %s, but got %s", v, req.Header.Get(k))
		}
	}
}

func TestWebhookRequest(t *testing.T) {
	ipv4 := true

	cases := []testSt{
		{
			profile: model.DDNSProfile{
				Domains:        []string{"www.example.com"},
				MaxRetries:     1,
				EnableIPv4:     &ipv4,
				WebhookURL:     "http://1.1.1.1/?ip=#ip#",
				WebhookMethod:  methodGET,
				WebhookHeaders: `{"ip":"#ip#","record":"#record#"}`,
			},
			expectURL:         "http://1.1.1.1/?ip=1.1.1.1",
			expectContentType: "",
			expectHeader: map[string]string{
				"ip":     "1.1.1.1",
				"record": "A",
			},
		},
		{
			profile: model.DDNSProfile{
				Domains:            []string{"www.example.com"},
				MaxRetries:         1,
				EnableIPv4:         &ipv4,
				WebhookURL:         "http://1.1.1.1/api",
				WebhookMethod:      methodPOST,
				WebhookRequestType: requestTypeJSON,
				WebhookRequestBody: `{"ip":"#ip#","record":"#record#"}`,
			},
			expectURL:         "http://1.1.1.1/api",
			expectContentType: reqTypeJSON,
			expectBody:        `{"ip":"1.1.1.1","record":"A"}`,
		},
		{
			profile: model.DDNSProfile{
				Domains:            []string{"www.example.com"},
				MaxRetries:         1,
				EnableIPv4:         &ipv4,
				WebhookURL:         "http://1.1.1.1/api",
				WebhookMethod:      methodPOST,
				WebhookRequestType: requestTypeForm,
				WebhookRequestBody: `{"ip":"#ip#","record":"#record#"}`,
			},
			expectURL:         "http://1.1.1.1/api",
			expectContentType: reqTypeForm,
			expectBody:        "ip=1.1.1.1&record=A",
		},
	}

	for _, c := range cases {
		execCase(t, c)
	}
}

func TestWebhookTargetRejectsBlockedRanges(t *testing.T) {
	cases := []string{
		"http://0.0.0.0/",
		"http://10.1.2.3/",
		"http://100.64.0.1/",
		"http://127.0.0.1/",
		"http://127.255.255.254/",
		"http://169.254.169.254/",
		"http://172.16.0.1/",
		"http://192.0.0.1/",
		"http://192.0.2.1/",
		"http://192.168.1.1/",
		"http://198.18.0.1/",
		"http://198.51.100.1/",
		"http://203.0.113.1/",
		"http://224.0.0.1/",
		"http://240.0.0.1/",
		"http://[::]/",
		"http://[::1]/",
		"http://[::ffff:127.0.0.1]/",
		"http://[64:ff9b::1]/",
		"http://[100::1]/",
		"http://[2001:db8::1]/",
		"http://[fc00::1]/",
		"http://[fe80::1]/",
		"http://[ff00::1]/",
		"ftp://example.com/",
		"file:///etc/passwd",
		"http:///path",
	}

	for _, rawURL := range cases {
		t.Run(rawURL, func(t *testing.T) {
			provider := Provider{DDNSProfile: &model.DDNSProfile{
				Domains:        []string{"www.example.com"},
				WebhookURL:     rawURL,
				WebhookMethod:  methodGET,
				WebhookHeaders: `{}`,
			}}
			provider.ipAddr = "1.1.1.1"
			provider.domain = provider.DDNSProfile.Domains[0]
			provider.ipType = "ipv4"
			provider.recordType = "A"

			_, _, err := provider.prepareRequest(context.Background())
			if err == nil {
				t.Fatalf("expected %s to be rejected", rawURL)
			}
			if !strings.Contains(err.Error(), "not allowed") {
				t.Fatalf("expected not allowed error, got %q", err.Error())
			}
		})
	}
}

// The DDNS worker logs the error SetRecords returns, so it must not carry the
// webhook URL: the URL holds tokens and the expanded #access_secret#.
func TestSetRecordsErrorOmitsWebhookURL(t *testing.T) {
	recs := []libdns.Record{libdns.Address{Name: "home", IP: netip.MustParseAddr("6.6.6.6")}}
	secrets := []string{"stored-id", "stored-secret", "embedded-token", "pa55"}

	// A cancelled context makes client.Do fail before anything is dialled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name   string
		url    string
		expect string
	}{
		{"request fails",
			"http://1.1.1.1/embedded-token/update?token=embedded-token&id=#access_id#&password=#access_secret#",
			"Get: " + context.Canceled.Error()},
		{"invalid escape",
			"http://1.1.1.1/embedded-token%zz/update", errInvalidURL.Error()},
		{"unescaped password",
			"http://user:pa55/word@1.1.1.1/update", errInvalidURL.Error()},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			provider := &Provider{DDNSProfile: &model.DDNSProfile{
				AccessID:      "stored-id",
				AccessSecret:  "stored-secret",
				WebhookURL:    c.url,
				WebhookMethod: methodGET,
			}}
			_, err := provider.SetRecords(ctx, "example.com.", recs)
			if err == nil {
				t.Fatal("expected SetRecords to fail")
			}
			for _, secret := range secrets {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error discloses %q: %v", secret, err)
				}
			}
			if !strings.HasSuffix(err.Error(), "Cause by: "+c.expect) {
				t.Fatalf("expected cause %q, got: %v", c.expect, err)
			}
		})
	}
}

// Placeholder values are not written by the author of the template (#domain#
// may be a server's DDNS domain override), so they must stay inside the JSON
// string or the header value the template puts them in.
func TestWebhookPlaceholderValuesStayInTheirField(t *testing.T) {
	domain := `www.example.com","ip":"6.6.6.6","Authorization":"Bearer other","x":"y.example.com`
	provider := &Provider{
		ipAddr:     "1.1.1.1",
		ipType:     "ipv4",
		recordType: "A",
		domain:     domain,
		DDNSProfile: &model.DDNSProfile{
			AccessSecret:       `stored"secret\`,
			WebhookURL:         "http://1.1.1.1/api?domain=#domain#",
			WebhookMethod:      methodPOST,
			WebhookRequestType: requestTypeJSON,
			WebhookRequestBody: "{\r\n\"ip\":\"#ip#\",\r\n\"domain\":\"#domain#\",\r\n\"key\":\"#access_secret#\"\r\n}",
			WebhookHeaders:     " {\r\n\"Authorization\":\"Bearer #access_secret#\",\r\n\"X-Domain\":\"#domain#\"\r\n} ",
		},
	}

	body, err := provider.reqBody()
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]string
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, body)
	}
	expect := map[string]string{"ip": "1.1.1.1", "domain": domain, "key": `stored"secret\`}
	if !maps.Equal(record, expect) {
		t.Fatalf("placeholder value changed the JSON document: %s", body)
	}

	req, _, err := provider.prepareRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != `Bearer stored"secret\` {
		t.Fatalf("placeholder value replaced a header: Authorization: %s", got)
	}
	if got := req.Header.Get("X-Domain"); got != domain {
		t.Fatalf("unexpected X-Domain: %s", got)
	}
	if len(req.Header) != 3 { // Content-Type and the two of the template
		t.Fatalf("placeholder value added a header: %v", req.Header)
	}
	if got := req.URL.Query(); len(got) != 1 || got.Get("domain") != domain {
		t.Fatalf("placeholder value changed the query: %s", req.URL.RawQuery)
	}
}
