package model

import (
	"net/http"
	"strings"
	"testing"
	"time"
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
	n := Notification{
		URL:           item.url,
		RequestMethod: item.reqMethod,
		RequestType:   item.reqType,
		RequestBody:   item.body,
		RequestHeader: item.header,
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
			SwapTotal:       0,
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
		TaskStream:              nil,
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
			body:              `{"Server":"#SERVER.NAME#","ServerIP":"#SERVER.IP#","ServerSWAP":#SERVER.SWAP#}`,
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
			body:              `{"#NEZHA#":"#NEZHA#","Server":"#SERVER.NAME#","ServerIP":"#SERVER.IP#","ServerSWAP":"#SERVER.SWAP#"}`,
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
