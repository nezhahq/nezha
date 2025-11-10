package webhook

import (
	"context"
	"testing"

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

	req, err := pw.prepareRequest(context.Background())
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
				WebhookURL:     "http://ddns.example.com/?ip=#ip#",
				WebhookMethod:  methodGET,
				WebhookHeaders: `{"ip":"#ip#","record":"#record#"}`,
			},
			expectURL:         "http://ddns.example.com/?ip=1.1.1.1",
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
				WebhookURL:         "http://ddns.example.com/api",
				WebhookMethod:      methodPOST,
				WebhookRequestType: requestTypeJSON,
				WebhookRequestBody: `{"ip":"#ip#","record":"#record#"}`,
			},
			expectURL:         "http://ddns.example.com/api",
			expectContentType: reqTypeJSON,
			expectBody:        `{"ip":"1.1.1.1","record":"A"}`,
		},
		{
			profile: model.DDNSProfile{
				Domains:            []string{"www.example.com"},
				MaxRetries:         1,
				EnableIPv4:         &ipv4,
				WebhookURL:         "http://ddns.example.com/api",
				WebhookMethod:      methodPOST,
				WebhookRequestType: requestTypeForm,
				WebhookRequestBody: `{"ip":"#ip#","record":"#record#"}`,
			},
			expectURL:         "http://ddns.example.com/api",
			expectContentType: reqTypeForm,
			expectBody:        "ip=1.1.1.1&record=A",
		},
	}

	for _, c := range cases {
		execCase(t, c)
	}
}
