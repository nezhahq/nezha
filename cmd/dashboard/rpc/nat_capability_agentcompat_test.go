//go:build agentcompat

package rpc

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/agentcompatcontract"
	serviceRPC "github.com/nezhahq/nezha/service/rpc"
)

func TestPrepareNATCapabilityConsumesAndRemovesHeaderBeforeNATWork(t *testing.T) {
	// Given
	handler := serviceRPC.NewNezhaHandler()
	original := serviceRPC.NezhaHandlerSingleton
	serviceRPC.NezhaHandlerSingleton = handler
	t.Cleanup(func() { serviceRPC.NezhaHandlerSingleton = original })
	request := &http.Request{Header: make(http.Header)}
	request.Header.Set(agentcompatcontract.IOStreamCapabilityHeader, "malformed")

	// When
	lease, err := prepareNATCapability(request, &model.NAT{Common: model.Common{ID: 91}, ServerID: 81})

	// Then
	if err == nil {
		t.Fatal("malformed capability unexpectedly accepted")
	}
	if lease.active {
		t.Fatal("malformed capability unexpectedly activated")
	}
	if _, present := request.Header[agentcompatcontract.IOStreamCapabilityHeader]; present {
		t.Fatal("capability header remained after hook")
	}
}

func TestPrepareNATCapabilityRejectsDuplicateHeaderAfterRemovingAllValues(t *testing.T) {
	// Given
	request := &http.Request{Header: make(http.Header)}
	request.Header.Add(agentcompatcontract.IOStreamCapabilityHeader, "one")
	request.Header.Add(agentcompatcontract.IOStreamCapabilityHeader, "two")

	// When
	lease, err := prepareNATCapability(request, &model.NAT{Common: model.Common{ID: 92}, ServerID: 82})

	// Then
	if err == nil {
		t.Fatal("duplicate capability header unexpectedly accepted")
	}
	if lease.active {
		t.Fatal("duplicate capability header unexpectedly activated")
	}
	if _, present := request.Header[agentcompatcontract.IOStreamCapabilityHeader]; present {
		t.Fatal("duplicate capability header remained after hook")
	}
}

func TestPrepareNATCapabilityAgentCompatStripsDashboardCredentials(t *testing.T) {
	// Given — legacy path without a capability header, then the invalid
	// capability path: both must drop dashboard-issued credentials from the
	// Authorization header, the ?token= query parameter and the nz-jwt cookie.
	installNATCredentialTestGate(t)
	for name, credential := range map[string]string{
		"dashboard jwt":         natTestDashboardJWTValue,
		"expired dashboard jwt": natTestDashboardExpiredJWTValue,
		"dashboard api token":   natTestDashboardPATValue,
	} {
		t.Run(name+"/legacy path", func(t *testing.T) {
			request := &http.Request{
				Header: make(http.Header),
				URL:    &url.URL{Path: "/nat", RawQuery: "keep=1&token=" + credential},
			}
			request.Header.Set("Authorization", "Bearer "+credential)
			request.Header.Set("Cookie", "sid=abc; nz-jwt="+credential)

			lease, err := prepareNATCapability(request, &model.NAT{Common: model.Common{ID: 91}, ServerID: 81})

			if err != nil {
				t.Fatalf("legacy NAT path returned error: %v", err)
			}
			if lease.active {
				t.Fatal("legacy NAT path unexpectedly activated")
			}
			if got := request.Header.Get("Authorization"); got != "" {
				t.Fatalf("legacy NAT path retained dashboard credential in Authorization %q", got)
			}
			if got := request.URL.RawQuery; got != "keep=1" {
				t.Fatalf("legacy NAT path retained dashboard credential in query %q", got)
			}
			if got := request.Header.Get("Cookie"); got != "sid=abc" {
				t.Fatalf("legacy NAT path retained dashboard credential in cookie %q", got)
			}
		})
		t.Run(name+"/invalid capability path", func(t *testing.T) {
			request := &http.Request{
				Header: make(http.Header),
				URL:    &url.URL{Path: "/nat", RawQuery: "keep=1&token=" + credential},
			}
			request.Header.Set(agentcompatcontract.IOStreamCapabilityHeader, "malformed")
			request.Header.Set("Authorization", "Bearer "+credential)
			request.Header.Set("Cookie", "sid=abc; nz-jwt="+credential)

			lease, err := prepareNATCapability(request, &model.NAT{Common: model.Common{ID: 91}, ServerID: 81})

			if err == nil {
				t.Fatal("malformed capability unexpectedly accepted")
			}
			if lease.active {
				t.Fatal("malformed capability unexpectedly activated")
			}
			if got := request.Header.Get("Authorization"); got != "" {
				t.Fatalf("invalid capability path retained dashboard credential in Authorization %q", got)
			}
			if got := request.URL.RawQuery; got != "keep=1" {
				t.Fatalf("invalid capability path retained dashboard credential in query %q", got)
			}
			if got := request.Header.Get("Cookie"); got != "sid=abc" {
				t.Fatalf("invalid capability path retained dashboard credential in cookie %q", got)
			}
		})
	}
}

func TestPrepareNATCapabilityAgentCompatStripsDashboardCredentialAfterConsumingCapability(t *testing.T) {
	// Given — a well-formed capability that consumes successfully: the request
	// is about to be tunneled, so the dashboard credential must be gone.
	installNATCredentialTestGate(t)
	handler := serviceRPC.NewNezhaHandler()
	original := serviceRPC.NezhaHandlerSingleton
	serviceRPC.NezhaHandlerSingleton = handler
	t.Cleanup(func() { serviceRPC.NezhaHandlerSingleton = original })

	capability, err := handler.RegisterAgentCompatIOStreamCapability(context.Background(), serviceRPC.AgentCompatCapabilityRegistration{
		Owner:               serviceRPC.AgentCompatCapabilityOwner{PATID: 1, UserID: 2},
		Purpose:             serviceRPC.AgentCompatCapabilityNAT,
		TargetServerID:      81,
		ResourceID:          91,
		ServerAccessAllowed: true,
	})
	if err != nil {
		t.Fatalf("register capability: %v", err)
	}
	request := &http.Request{
		Header: make(http.Header),
		URL:    &url.URL{Path: "/nat", RawQuery: "keep=1&token=" + natTestDashboardJWTValue},
	}
	request.Header.Set(agentcompatcontract.IOStreamCapabilityHeader, capability.String())
	request.Header.Set("Authorization", natTestDashboardJWT)
	request.Header.Set("Cookie", "sid=abc; nz-jwt="+natTestDashboardJWTValue)

	// When
	lease, err := prepareNATCapability(request, &model.NAT{Common: model.Common{ID: 91}, ServerID: 81})

	// Then
	if err != nil {
		t.Fatalf("valid NAT capability rejected: %v", err)
	}
	if !lease.active {
		t.Fatal("valid NAT capability did not activate")
	}
	if got := request.Header.Get("Authorization"); got != "" {
		t.Fatalf("dashboard credential retained in Authorization after capability consumption: %q", got)
	}
	if got := request.URL.RawQuery; got != "keep=1" {
		t.Fatalf("dashboard credential retained in query after capability consumption: %q", got)
	}
	if got := request.Header.Get("Cookie"); got != "sid=abc" {
		t.Fatalf("dashboard credential retained in cookie after capability consumption: %q", got)
	}
}

func TestPrepareNATCapabilityAgentCompatKeepsForeignAndMissingAuthorization(t *testing.T) {
	// Given — legacy path: foreign credentials are ordinary request data and
	// an absent header must stay untouched.
	installNATCredentialTestGate(t)
	for name, values := range map[string][]string{
		"foreign jwt":    {natTestForeignJWT},
		"foreign random": {natTestForeignRandom},
		"empty value":    {""},
		"missing header": nil,
	} {
		t.Run(name, func(t *testing.T) {
			request := &http.Request{Header: make(http.Header)}
			for _, value := range values {
				request.Header.Set("Authorization", value)
			}

			lease, err := prepareNATCapability(request, &model.NAT{Common: model.Common{ID: 91}, ServerID: 81})

			if err != nil {
				t.Fatalf("legacy NAT path returned error: %v", err)
			}
			if lease.active {
				t.Fatal("legacy NAT path unexpectedly activated")
			}
			got := request.Header.Values("Authorization")
			if len(got) != len(values) {
				t.Fatalf("legacy NAT path changed Authorization to %q, want %q", got, values)
			}
			for i := range values {
				if got[i] != values[i] {
					t.Fatalf("legacy NAT path changed Authorization to %q, want %q", got, values)
				}
			}
		})
	}
}

func TestPrepareNATCapabilityAgentCompatKeepsForeignAndMissingQueryAndCookie(t *testing.T) {
	// Given — legacy path: foreign query tokens and cookies are ordinary
	// request data, and absent channels must stay untouched.
	installNATCredentialTestGate(t)
	for name, tc := range map[string]struct{ rawQuery, cookie string }{
		"foreign values":  {"keep=1&token=" + natTestForeignJWTValue + "&a=%2Fx", "sid=abc; nz-jwt=" + natTestForeignJWTValue},
		"empty values":    {"keep=1&token=", "nz-jwt="},
		"missing channel": {"keep=1", "sid=abc"},
	} {
		t.Run(name, func(t *testing.T) {
			request := &http.Request{
				Header: make(http.Header),
				URL:    &url.URL{Path: "/nat", RawQuery: tc.rawQuery},
			}
			request.Header.Set("Cookie", tc.cookie)

			lease, err := prepareNATCapability(request, &model.NAT{Common: model.Common{ID: 91}, ServerID: 81})

			if err != nil {
				t.Fatalf("legacy NAT path returned error: %v", err)
			}
			if lease.active {
				t.Fatal("legacy NAT path unexpectedly activated")
			}
			if got := request.URL.RawQuery; got != tc.rawQuery {
				t.Fatalf("legacy NAT path changed query to %q, want %q", got, tc.rawQuery)
			}
			if got := request.Header.Get("Cookie"); got != tc.cookie {
				t.Fatalf("legacy NAT path changed cookie to %q, want %q", got, tc.cookie)
			}
		})
	}
}

func TestServeNATAgentCompatSensitiveHeadersStayOutOfErrorsAndLogs(t *testing.T) {
	fixture := newServeNATFixture(t)
	installNATCredentialTestGate(t)
	request := &http.Request{Header: make(http.Header)}
	request.Header.Set(agentcompatcontract.IOStreamCapabilityHeader, "capability-secret")
	request.Header.Set("Authorization", "Bearer deterministic-secret")
	writer := &serveNATResponseWriter{}
	var logs bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(originalOutput) })

	ServeNAT(writer, request, &model.NAT{Common: model.Common{ID: 91, UserID: 1}, ServerID: fixture.server.ID})

	if writer.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", writer.status, http.StatusServiceUnavailable)
	}
	for _, output := range []string{writer.body, logs.String()} {
		if strings.Contains(output, "capability-secret") || strings.Contains(output, "deterministic-secret") {
			t.Fatalf("sensitive value leaked in %q", output)
		}
	}
}
