package rpc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
)

// These characterization tests stop at in-process task streams. They open no
// listeners, execute no commands, and never connect to a dashboard or agent.
func TestSecurityBoundaryNATDispatchAfterOwnershipChange(t *testing.T) {
	fixture := newServeNATFixture(t)
	originalConf := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{DashboardHost: "panel.example"}, NATMemberDomains: []string{"nat.example.invalid"}}}
	t.Cleanup(func() { singleton.Conf = originalConf })
	require.True(t, singleton.NATDomainAllowed("nat.example.invalid", false))
	fixture.server.SetUserID(100)
	profile := &model.NAT{Common: model.Common{UserID: 100}, ServerID: fixture.server.ID, Domain: "nat.example.invalid", Host: "backend.example.invalid:80"}
	caller, _ := gin.CreateTestContext(httptest.NewRecorder())
	caller.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 100}, Role: model.RoleMember})
	require.True(t, fixture.server.HasPermission(caller))
	fixture.server.SetUserID(200)
	require.False(t, fixture.server.HasPermission(caller), "former owner must fail the ordinary server permission check")
	fixture.taskStream.sendErr = errors.New("stop at simulated NAT task delivery")
	writer := &serveNATResponseWriter{conn: newServeNATConn()}
	ServeNAT(writer, httptest.NewRequest(http.MethodGet, "http://nat.example.invalid/", nil), profile)
	require.Empty(t, fixture.taskStream.sent, "former owner must not dispatch NAT tasks")
	require.Equal(t, http.StatusForbidden, writer.status)
}

func TestSecurityBoundaryNATDispatchBeforeHTTP2Rejection(t *testing.T) {
	fixture := newServeNATFixture(t)
	request := httptest.NewRequest(http.MethodGet, "https://nat.example.invalid/", nil)
	request.Proto, request.ProtoMajor, request.ProtoMinor = "HTTP/2.0", 2, 0
	writer := httptest.NewRecorder() // Like HTTP/2 writers, this has no Hijacker.
	ServeNAT(writer, request, &model.NAT{ServerID: fixture.server.ID, Host: "backend.example.invalid:80"})
	require.Empty(t, fixture.taskStream.sent)
	require.Equal(t, http.StatusHTTPVersionNotSupported, writer.Code)
}

func TestSecurityBoundaryNATMemberCannotExhaustProcessWideSlots(t *testing.T) {
	fixture := newServeNATFixture(t)
	originalConf := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{DashboardHost: "panel.example"}, NATMemberDomains: []string{"nat.example.invalid"}}}
	t.Cleanup(func() { singleton.Conf = originalConf })
	fixture.server.SetUserID(100)
	fixture.taskStream.sendErr = errors.New("stop at simulated NAT task delivery")
	memberProfile := &model.NAT{Common: model.Common{UserID: 100}, ServerID: fixture.server.ID, Domain: "nat.example.invalid", Host: "backend.example.invalid:80"}
	adminProfile := &model.NAT{Common: model.Common{UserID: 1}, ServerID: fixture.server.ID, Domain: "intranet.example.invalid", Host: "backend.example.invalid:80"}

	// Stand in for the member's idle tunnels: each one keeps its slot until it ends.
	var held []func()
	releaseHeld := func() {
		for _, release := range held {
			release()
		}
		held = nil
	}
	t.Cleanup(releaseHeld)
	for range natMaxActiveRequests {
		release, admitted := natActiveRequests.acquire(memberProfile.UserID, false)
		if !admitted {
			break
		}
		held = append(held, release)
	}
	require.Len(t, held, natMaxActiveRequestsPerMember, "one member must not hold the whole process-wide budget")

	writer := &serveNATResponseWriter{conn: newServeNATConn()}
	ServeNAT(writer, httptest.NewRequest(http.MethodGet, "http://nat.example.invalid/", nil), memberProfile)
	require.Equal(t, http.StatusTooManyRequests, writer.status)
	require.Empty(t, fixture.taskStream.sent, "a member over their share must not task the agent")

	writer = &serveNATResponseWriter{conn: newServeNATConn()}
	ServeNAT(writer, httptest.NewRequest(http.MethodGet, "http://intranet.example.invalid/", nil), adminProfile)
	require.Len(t, fixture.taskStream.sent, 1, "another owner's NAT domain must still be served")
	require.Equal(t, http.StatusServiceUnavailable, writer.status)

	releaseHeld()
	require.Zero(t, natActiveRequests.total, "finished requests must return their slots")
	require.Empty(t, natActiveRequests.byMember)
}
