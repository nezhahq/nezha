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
