package controller

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// Every route that takes a server ID must answer a member exactly the same
// for another user's server (online or offline) and for an ID that does not
// exist. Otherwise the error tells the member which server IDs exist across
// tenants, including hide_for_guest ones, and whether their agents are
// online right now.
func TestServerIDRoutesDoNotRevealForeignServers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream, reset := setupServerOwnershipFixture(t) // alice (100) owns server 1, online
	defer reset()
	require.NotNil(t, stream)
	require.NoError(t, singleton.DB.AutoMigrate(&model.ServerGroup{}, &model.ServerGroupServer{}, &model.NAT{}, &model.APIToken{}))
	originalDDNS := singleton.DDNSShared
	singleton.DDNSShared = singleton.NewEmptyDDNSClassForTest()
	t.Cleanup(func() { singleton.DDNSShared = originalDDNS })

	const bob = uint64(200)
	const foreignOnline, unknown = uint64(1), uint64(99)

	request := func(body any, query string, params gin.Params) *gin.Context {
		c := ctxAs(bob, model.RoleMember)
		b, _ := json.Marshal(body)
		c.Request = httptest.NewRequest("POST", "/"+query, bytes.NewReader(b))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = params
		return c
	}
	errorOf := func(_ any, err error) string {
		if err == nil {
			return "<nil>"
		}
		return err.Error()
	}
	routes := map[string]func(id uint64) string{
		"POST /terminal": func(id uint64) string {
			return errorOf(createTerminal(request(map[string]any{"server_id": id}, "", nil)))
		},
		"POST /file": func(id uint64) string {
			return errorOf(createFM(request(nil, "?id="+strconv.FormatUint(id, 10), nil)))
		},
		"PATCH /server/:id": func(id uint64) string {
			return errorOf(updateServer(request(map[string]any{"name": "probe"}, "", gin.Params{{Key: "id", Value: strconv.FormatUint(id, 10)}})))
		},
		"POST /batch-delete/server": func(id uint64) string {
			return errorOf(batchDeleteServer(request([]uint64{id}, "", nil)))
		},
		"POST /nat": func(id uint64) string {
			return errorOf(createNAT(request(map[string]any{"server_id": id, "name": "p", "domain": "p.example", "host": "127.0.0.1:1"}, "", nil)))
		},
		"PATCH /nat/:id": func(id uint64) string {
			return errorOf(updateNAT(request(map[string]any{"server_id": id, "name": "p", "domain": "p.example", "host": "127.0.0.1:1"}, "", gin.Params{{Key: "id", Value: "1"}})))
		},
		"POST /server-group": func(id uint64) string {
			return errorOf(createServerGroup(request(map[string]any{"name": "p", "servers": []uint64{id}}, "", nil)))
		},
		"POST /api-tokens": func(id uint64) string {
			return errorOf(createAPIToken(request(map[string]any{"name": "p", "scopes": []string{model.ScopeServerRead}, "server_ids": []uint64{id}}, "", nil)))
		},
	}
	for name, call := range routes {
		t.Run(name, func(t *testing.T) {
			online := call(foreignOnline)
			missing := call(unknown)
			require.NotEqual(t, "<nil>", online, "the member must not act on another user's server")
			require.Equal(t, missing, online, "another user's online server answers differently from an unknown ID")
		})
	}

	// Offline: the terminal and file manager used to reveal the agent state.
	alice, _ := singleton.ServerShared.Get(foreignOnline)
	alice.SetTaskStream(nil)
	for _, name := range []string{"POST /terminal", "POST /file"} {
		require.Equal(t, routes[name](unknown), routes[name](foreignOnline), "%s: offline foreign server answers differently", name)
	}
}

// Administrators can see every server, so they keep the precise errors for
// unknown IDs (and a server batch delete still skips them). Only members get
// the shared answer that hides which server IDs exist.
func TestServerIDRoutesKeepPreciseErrorsForAdmins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, reset := setupServerOwnershipFixture(t)
	defer reset()
	require.NoError(t, singleton.DB.AutoMigrate(&model.ServerGroup{}, &model.ServerGroupServer{}, &model.NAT{}, &model.APIToken{}))
	originalDDNS := singleton.DDNSShared
	singleton.DDNSShared = singleton.NewEmptyDDNSClassForTest()
	t.Cleanup(func() { singleton.DDNSShared = originalDDNS })

	const unknown = uint64(99)
	request := func(body any, query string, params gin.Params) *gin.Context {
		c := ctxAs(1, model.RoleAdmin)
		b, _ := json.Marshal(body)
		c.Request = httptest.NewRequest("POST", "/"+query, bytes.NewReader(b))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = params
		return c
	}
	errorOf := func(_ any, err error) string {
		if err == nil {
			return "<nil>"
		}
		return err.Error()
	}
	nat := map[string]any{"server_id": unknown, "name": "p", "domain": "p.example", "host": "127.0.0.1:1"}
	want := map[string]string{
		"POST /terminal":            "server not found or not connected",
		"POST /file":                "server not found or not connected",
		"PATCH /server/:id":         "server id 99 does not exist",
		"POST /batch-delete/server": "<nil>",
		"POST /nat":                 "have invalid server id",
		"PATCH /nat/:id":            "have invalid server id",
		"POST /server-group":        "have invalid server id",
		"POST /api-tokens":          "server not found",
	}
	got := map[string]string{
		"POST /terminal":            errorOf(createTerminal(request(map[string]any{"server_id": unknown}, "", nil))),
		"POST /file":                errorOf(createFM(request(nil, "?id=99", nil))),
		"PATCH /server/:id":         errorOf(updateServer(request(map[string]any{"name": "probe"}, "", gin.Params{{Key: "id", Value: "99"}}))),
		"POST /batch-delete/server": errorOf(batchDeleteServer(request([]uint64{unknown}, "", nil))),
		"POST /nat":                 errorOf(createNAT(request(nat, "", nil))),
		"PATCH /nat/:id":            errorOf(updateNAT(request(nat, "", gin.Params{{Key: "id", Value: "1"}}))),
		"POST /server-group":        errorOf(createServerGroup(request(map[string]any{"name": "p", "servers": []uint64{unknown}}, "", nil))),
		"POST /api-tokens":          errorOf(createAPIToken(request(map[string]any{"name": "p", "scopes": []string{model.ScopeServerRead}, "server_ids": []uint64{unknown}}, "", nil))),
	}
	require.Equal(t, want, got)
}
