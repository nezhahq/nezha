package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nezhahq/nezha/pkg/utils"
)

var (
	msg         = "msg"
	reqTypeForm = "application/x-www-form-urlencoded"
	reqTypeJSON = "application/json"
)

type testSt struct {
	url               string
	body              string
	header            string
	reqType           uint8
	reqMethod         uint8
	expectURL         string
	expectBody        string
	expectMethod      string
	expectContentType string
	expectHeader      map[string]string
}

func execCase(t *testing.T, item testSt) {
	trueBool := true
	n := Notification{
		URL:               item.url,
		RequestMethod:     item.reqMethod,
		RequestType:       item.reqType,
		RequestBody:       item.body,
		RequestHeader:     item.header,
		FormatMetricUnits: &trueBool,
	}
	server := Server{
		Common:       Common{},
		Name:         "ServerName",
		Note:         "",
		DisplayIndex: 0,
		Host: &Host{
			Platform:        "",
			PlatformVersion: "",
			CPU:             nil,
			MemTotal:        0,
			DiskTotal:       0,
			SwapTotal:       8888,
			Arch:            "",
			Virtualization:  "",
			BootTime:        0,
			Version:         "",
		},
		State: &HostState{
			CPU:            0,
			MemUsed:        0,
			SwapUsed:       8888,
			DiskUsed:       0,
			NetInTransfer:  0,
			NetOutTransfer: 0,
			NetInSpeed:     0,
			NetOutSpeed:    0,
			Uptime:         0,
			Load1:          0,
			Load5:          0,
			Load15:         0,
			TcpConnCount:   0,
			UdpConnCount:   0,
			ProcessCount:   0,
		},
		GeoIP: &GeoIP{
			IP: IP{
				IPv4Addr: "1.1.1.1",
			},
			CountryCode: "",
		},
		LastActive:              time.Time{},
		PrevTransferInSnapshot:  0,
		PrevTransferOutSnapshot: 0,
	}
	ns := NotificationServerBundle{
		Notification: &n,
		Server:       &server,
		Loc:          time.Local,
	}
	if item.expectURL != ns.reqURL(msg) {
		t.Fatalf("Expected %s, but got %s", item.expectURL, ns.reqURL(msg))
	}
	reqBody, err := ns.reqBody(msg)
	if err != nil {
		t.Fatalf("Error: %s", err)
	}
	if item.expectBody != reqBody {
		t.Fatalf("Expected %s, but got %s", item.expectBody, reqBody)
	}
	reqMethod, err := n.reqMethod()
	if err != nil {
		t.Fatalf("Error: %s", err)
	}
	if item.expectMethod != reqMethod {
		t.Fatalf("Expected %s, but got %s", item.expectMethod, reqMethod)
	}

	req, err := http.NewRequest("", "", strings.NewReader(""))
	if err != nil {
		t.Fatalf("Error: %s", err)
	}
	n.setContentType(req)
	if item.expectContentType != req.Header.Get("Content-Type") {
		t.Fatalf("Expected %s, but got %s", item.expectContentType, req.Header.Get("Content-Type"))
	}
	n.setRequestHeader(req)
	for k, v := range item.expectHeader {
		if v != req.Header.Get(k) {
			t.Fatalf("Expected %s, but got %s", v, req.Header.Get(k))
		}
	}
}

func TestNotification(t *testing.T) {
	cases := []testSt{
		{
			url:               "https://example.com",
			body:              `{"asd":"dsa"}`,
			header:            `{"asd":"dsa"}`,
			reqMethod:         NotificationRequestMethodGET,
			expectURL:         "https://example.com",
			expectMethod:      http.MethodGet,
			expectContentType: "",
			expectHeader:      map[string]string{"asd": "dsa"},
			expectBody:        "",
		},
		{
			url:               "https://example.com/?m=#NEZHA#",
			body:              `{"asd":"dsa"}`,
			reqMethod:         NotificationRequestMethodGET,
			expectURL:         "https://example.com/?m=" + msg,
			expectMethod:      http.MethodGet,
			expectContentType: "",
			expectBody:        "",
		},
		{
			url:               "https://example.com/?m=#NEZHA#",
			body:              `{"asd":"#NEZHA#"}`,
			reqMethod:         NotificationRequestMethodPOST,
			reqType:           NotificationRequestTypeForm,
			expectURL:         "https://example.com/?m=" + msg,
			expectMethod:      http.MethodPost,
			expectContentType: reqTypeForm,
			expectBody:        "asd=" + msg,
		},
		{
			url:               "https://example.com/?m=#NEZHA#",
			body:              `{"#NEZHA#":"#NEZHA#"}`,
			reqMethod:         NotificationRequestMethodPOST,
			reqType:           NotificationRequestTypeForm,
			expectURL:         "https://example.com/?m=" + msg,
			expectMethod:      http.MethodPost,
			expectContentType: reqTypeForm,
			expectBody:        "%23NEZHA%23=" + msg,
		},
		{
			url:               "https://example.com/?m=#NEZHA#",
			body:              `{"asd":"#NEZHA#"}`,
			reqMethod:         NotificationRequestMethodPOST,
			reqType:           NotificationRequestTypeJSON,
			expectURL:         "https://example.com/?m=" + msg,
			expectMethod:      http.MethodPost,
			expectContentType: reqTypeJSON,
			expectBody:        `{"asd":"msg"}`,
		},
		{
			url:               "https://example.com/?m=#NEZHA#",
			body:              `{"#NEZHA#":"#NEZHA#"}`,
			reqMethod:         NotificationRequestMethodPOST,
			header:            `{"asd":"dsa11"}`,
			reqType:           NotificationRequestTypeJSON,
			expectURL:         "https://example.com/?m=" + msg,
			expectMethod:      http.MethodPost,
			expectContentType: reqTypeJSON,
			expectBody:        `{"msg":"msg"}`,
			expectHeader:      map[string]string{"asd": "dsa11"},
		},
		{
			url:               "https://example.com/?m=#NEZHA#",
			body:              `{"Server":"#SERVER.NAME#","ServerIP":"#SERVER.IP#","ServerSWAP":"#SERVER.SWAP#"}`,
			reqMethod:         NotificationRequestMethodPOST,
			header:            `{"asd":"dsa11"}`,
			reqType:           NotificationRequestTypeJSON,
			expectURL:         "https://example.com/?m=" + msg,
			expectMethod:      http.MethodPost,
			expectContentType: reqTypeJSON,
			expectBody:        `{"Server":"ServerName","ServerIP":"1.1.1.1","ServerSWAP":"100.00 %"}`,
			expectHeader:      map[string]string{"asd": "dsa11"},
		},
		{
			url:               "https://example.com/?m=#NEZHA#",
			body:              `{"#NEZHA#":"#NEZHA#","Server":"#SERVER.NAME#","ServerIP":"#SERVER.IP#","ServerSWAP":"#SERVER.SWAP#"}`,
			reqMethod:         NotificationRequestMethodPOST,
			reqType:           NotificationRequestTypeForm,
			expectURL:         "https://example.com/?m=" + msg,
			expectMethod:      http.MethodPost,
			expectContentType: reqTypeForm,
			expectBody:        "%23NEZHA%23=" + msg + "&Server=ServerName&ServerIP=1.1.1.1&ServerSWAP=100.00+%25",
		},
		{
			url:               "https://example.com/?m=#NEZHA#",
			body:              `{"Server":"#SERVER.NAME#","ServerIP":"#SERVER.IP#","ServerSWAP":#SERVER.SWAPUSED#}`,
			reqMethod:         NotificationRequestMethodPOST,
			header:            `{"asd":"dsa11"}`,
			reqType:           NotificationRequestTypeJSON,
			expectURL:         "https://example.com/?m=" + msg,
			expectMethod:      http.MethodPost,
			expectContentType: reqTypeJSON,
			expectBody:        `{"Server":"ServerName","ServerIP":"1.1.1.1","ServerSWAP":8888}`,
			expectHeader:      map[string]string{"asd": "dsa11"},
		},
		{
			url:               "https://example.com/?m=#NEZHA#",
			body:              `{"#NEZHA#":"#NEZHA#","Server":"#SERVER.NAME#","ServerIP":"#SERVER.IP#","ServerSWAP":"#SERVER.SWAPUSED#"}`,
			reqMethod:         NotificationRequestMethodPOST,
			reqType:           NotificationRequestTypeForm,
			expectURL:         "https://example.com/?m=" + msg,
			expectMethod:      http.MethodPost,
			expectContentType: reqTypeForm,
			expectBody:        "%23NEZHA%23=" + msg + "&Server=ServerName&ServerIP=1.1.1.1&ServerSWAP=8888",
		},
	}

	for _, c := range cases {
		execCase(t, c)
	}
}

func TestNotificationResponseErrorDoesNotReflectNonSuccessResponseBody(t *testing.T) {
	const internalResponseBody = "internal service says token=secret"

	resp := &http.Response{
		StatusCode: http.StatusTeapot,
		Status:     "418 I'm a teapot",
		Body:       io.NopCloser(strings.NewReader(internalResponseBody)),
	}

	err := notificationResponseError(resp)
	if strings.Contains(err.Error(), internalResponseBody) {
		t.Fatalf("expected upstream response body to be hidden from error, got %q", err.Error())
	}
}

func TestNotificationSendRejectsLoopbackTarget(t *testing.T) {
	verifyTLS := true
	notification := &Notification{
		URL:           "http://127.0.0.1/internal",
		RequestMethod: NotificationRequestMethodGET,
		VerifyTLS:     &verifyTLS,
	}

	bundle := NotificationServerBundle{
		Notification: notification,
		Loc:          time.Local,
	}

	err := bundle.Send("probe")
	if err == nil {
		t.Fatal("expected loopback notification URL to be rejected")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected not allowed error, got %q", err.Error())
	}
}

func TestNotificationSendContextStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bundle := NotificationServerBundle{
		Notification: &Notification{URL: "http://1.1.1.1/", RequestMethod: NotificationRequestMethodGET},
		Loc:          time.Local,
	}
	if err := bundle.SendContext(ctx, "probe"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the canceled context to end the request, got %v", err)
	}
}

func TestNotificationTargetRejectsBlockedRanges(t *testing.T) {
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
		"http://[2001:0:0:0:0:0:0:1]/",
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
			if _, _, err := utils.ResolveAllowedHTTPURL(rawURL); err == nil {
				t.Fatalf("expected %s to be rejected", rawURL)
			}
		})
	}
}

func TestNotificationTargetAllowsPublicAddresses(t *testing.T) {
	cases := []string{
		"http://1.1.1.1/path",
		"https://8.8.8.8/",
		"https://[2606:4700:4700::1111]/",
	}

	for _, rawURL := range cases {
		t.Run(rawURL, func(t *testing.T) {
			parsedURL, _, err := utils.ResolveAllowedHTTPURL(rawURL)
			if err != nil {
				t.Fatalf("expected %s to be allowed, got %v", rawURL, err)
			}
			if parsedURL == nil {
				t.Fatalf("expected parsed url for %s", rawURL)
			}
		})
	}
}

func TestNotificationHTTPClientInvertsVerifyTLSFlag(t *testing.T) {
	// newNotificationHTTPClient takes verifyTLS, utils.NewRestrictedHTTPClient
	// takes skipVerifyTLS. The wrapper must invert the boolean; if a future
	// refactor drops the negation, TLS verification silently turns off.
	// SNI / redirect / IP-pinning are covered by pkg/utils/http_test.go.
	cases := []struct {
		name             string
		verifyTLS        bool
		wantSkipVerifyOn bool
	}{
		{"verifyTLS_true_means_skipVerify_false", true, false},
		{"verifyTLS_false_means_skipVerify_true", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := newNotificationHTTPClient(context.Background(), "https://1.1.1.1/webhook", tc.verifyTLS)
			if err != nil {
				t.Fatalf("expected client construction: %v", err)
			}
			transport, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("expected *http.Transport, got %T", client.Transport)
			}
			if transport.TLSClientConfig == nil {
				t.Fatalf("expected TLSClientConfig to be set")
			}
			if got := transport.TLSClientConfig.InsecureSkipVerify; got != tc.wantSkipVerifyOn {
				t.Fatalf("verifyTLS=%v: expected InsecureSkipVerify=%v, got %v",
					tc.verifyTLS, tc.wantSkipVerifyOn, got)
			}
		})
	}
}

// A form field or a query parameter may hold a JSON document (Slack's
// payload={"text":"#NEZHA#"}). The message and the server name are not written
// by the owner of the notification, so they must stay inside the JSON string
// the template puts them in.
func TestNotificationPlaceholderValuesStayInNestedJSON(t *testing.T) {
	message := `[Down] mail Error: x", "channel": "#mallory", "text": "[Good] mail`
	name := `web", "channel": "#mallory`
	ns := NotificationServerBundle{
		Server: &Server{Name: name},
		Loc:    time.Local,
	}
	check := func(what, document string) {
		t.Helper()
		var got map[string]string
		if err := json.Unmarshal([]byte(document), &got); err != nil {
			t.Fatalf("%s is not JSON: %v: %s", what, err, document)
		}
		expect := map[string]string{"channel": "#oncall", "server": name, "text": message}
		if !maps.Equal(got, expect) {
			t.Fatalf("placeholder value changed the JSON document in %s: %s", what, document)
		}
	}

	for _, payload := range []string{
		`{"channel": "#oncall", "server": "#SERVER.NAME#", "text": "#NEZHA#"}`,
		`"{\"channel\": \"#oncall\", \"server\": \"#SERVER.NAME#\", \"text\": \"#NEZHA#\"}"`,
	} {
		ns.Notification = &Notification{
			URL:           "https://example.com/hook",
			RequestMethod: NotificationRequestMethodPOST,
			RequestType:   NotificationRequestTypeForm,
			RequestBody:   `{"plain": "[#SERVER.NAME#] #NEZHA#", "payload": ` + payload + `}`,
		}
		body, err := ns.reqBody(message)
		if err != nil {
			t.Fatal(err)
		}
		form, err := url.ParseQuery(body)
		if err != nil {
			t.Fatal(err)
		}
		check("form field", form.Get("payload"))
		// Plain text fields are sent as before.
		if got := form.Get("plain"); got != "["+name+"] "+message {
			t.Fatalf("plain text field changed: %s", got)
		}
	}

	for _, query := range []string{
		`payload={"channel":"%23oncall","server":"#SERVER.NAME#","text":"#NEZHA#"}`,
		`payload=%7B%22channel%22%3A%22%23oncall%22%2C%22server%22%3A%22#SERVER.NAME#%22%2C%22text%22%3A%22#NEZHA#%22%7D`,
	} {
		ns.Notification = &Notification{
			URL:           "https://example.com/hook?plain=[#SERVER.NAME#]+#NEZHA#&" + query,
			RequestMethod: NotificationRequestMethodGET,
		}
		u, err := url.Parse(ns.reqURL(message))
		if err != nil {
			t.Fatal(err)
		}
		check("query parameter", u.Query().Get("payload"))
		if got := u.Query().Get("plain"); got != "["+name+"] "+message {
			t.Fatalf("plain text query parameter changed: %s", got)
		}
	}
}

func TestJSONTemplateEscape(t *testing.T) {
	cases := map[string]bool{
		// JSON documents
		`{"text":"#NEZHA#"}`:                                         true,
		` [{"text": "#SERVER.NAME#: #NEZHA#"}] `:                     true,
		`{"text":"#NEZHA#","swap":#SERVER.SWAPUSED#}`:                true,
		"{\n  \"channel\": \"#oncall\",\n  \"text\": \"#NEZHA#\"\n}": true,
		// plain text
		``:                           false,
		`#NEZHA#`:                    false,
		`"#NEZHA#"`:                  false,
		`[#SERVER.NAME#]`:            false,
		`[#SERVER.NAME#] #NEZHA#`:    false,
		`["Nezha"] #NEZHA#`:          false,
		`{#NEZHA#}`:                  false,
		`{"text":"#NEZHA#"} #NEZHA#`: false,
	}
	for value, expect := range cases {
		if got := jsonTemplateEscape(value) != nil; got != expect {
			t.Errorf("jsonTemplateEscape(%q): JSON document %v, expected %v", value, got, expect)
		}
	}
}

func TestNotificationZeroTotalsRenderFiniteUsage(t *testing.T) {
	const body = `{"mem": #SERVER.MEM#, "swap": #SERVER.SWAP#, "disk": #SERVER.DISK#}`
	cases := []struct {
		name   string
		host   Host
		state  HostState
		expect string
	}{
		{
			name:   "no host report yet",
			expect: `{"mem": 0.000000, "swap": 0.000000, "disk": 0.000000}`,
		},
		{
			name:   "zero totals with non-zero usage",
			state:  HostState{MemUsed: 1, SwapUsed: 1, DiskUsed: 1},
			expect: `{"mem": 0.000000, "swap": 0.000000, "disk": 0.000000}`,
		},
		{
			name:   "host without swap",
			host:   Host{MemTotal: 4, DiskTotal: 4},
			state:  HostState{MemUsed: 1, DiskUsed: 1},
			expect: `{"mem": 0.250000, "swap": 0.000000, "disk": 0.250000}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ns := NotificationServerBundle{
				Notification: &Notification{
					RequestMethod: NotificationRequestMethodPOST,
					RequestType:   NotificationRequestTypeJSON,
					RequestBody:   body,
				},
				Server: &Server{Host: &c.host, State: &c.state},
				Loc:    time.UTC,
			}
			got, err := ns.reqBody(msg)
			if err != nil {
				t.Fatalf("Error: %s", err)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("zero totals rendered an invalid JSON body: %s", got)
			}
			if got != c.expect {
				t.Fatalf("Expected %s, but got %s", c.expect, got)
			}

			formatMetricUnits := true
			ns.Notification.FormatMetricUnits = &formatMetricUnits
			got = ns.replaceParamsInString("#SERVER.MEM# #SERVER.SWAP# #SERVER.DISK#", msg, nil)
			if strings.Contains(got, "NaN") || strings.Contains(got, "Inf") {
				t.Fatalf("zero totals rendered a non-finite usage: %s", got)
			}
		})
	}
}
