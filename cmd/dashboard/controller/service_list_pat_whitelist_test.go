package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

func newServiceListPATRouter(t *testing.T, tok *model.APIToken) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		setAuthUser(c, 100, model.RoleMember)
		if tok != nil {
			c.Set(model.CtxKeyAPIToken, tok)
			c.Set(apiTokenCtxKey, tok)
		}
		c.Next()
	})
	r.GET("/api/v1/service/list", listHandler(listService))
	return r
}

// GET /api/v1/service/list must hide ServiceCoverAll rows whose SkipServers
// deny-set does not cover every owner server outside the PAT whitelist.
// DispatchTask would still probe those servers, so leaking the row to the
// list view (and exposing target/credentials/triggers) is a real PAT scope
// escape.
func TestListService_HidesCoverAllWithInsufficientSkipForLimitedPAT(t *testing.T) {
	setupServiceDispatchPATFixture(t)
	insufficient := insertServiceForDispatchTest(t, model.ServiceCoverAll, map[uint64]bool{1: true})
	sufficient := insertServiceForDispatchTest(t, model.ServiceCoverAll, map[uint64]bool{2: true})

	tok := &model.APIToken{ID: 34, UserID: 100}
	tok.SetServerIDs([]uint64{1})

	r := newServiceListPATRouter(t, tok)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/service/list", nil)
	r.ServeHTTP(w, req)

	var resp struct {
		Success bool             `json:"success"`
		Error   string           `json:"error"`
		Data    []*model.Service `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Success, resp.Error)

	seen := map[uint64]bool{}
	for _, s := range resp.Data {
		seen[s.ID] = true
	}
	assert.False(t, seen[insufficient],
		"PAT [1] must NOT see a ServiceCoverAll whose SkipServers does not cover owner server 2 (rows=%+v)", resp.Data)
	assert.True(t, seen[sufficient],
		"PAT [1] must still see a ServiceCoverAll whose SkipServers already covers every non-whitelisted owner server")
}

// GET /api/v1/service and GET /api/v1/service/:id/history must draw the same
// PAT boundary as /service/list for monitors that are NOT hide_for_guest:
// the status figures are aggregated over every probing server, so a
// server_ids PAT may only read monitors whose fan-out stays inside its
// whitelist.
func TestShowService_LimitedPATGetsOnlyMonitorsInsideWhitelist(t *testing.T) {
	setupServiceDispatchPATFixture(t)
	outside := insertServiceForDispatchTest(t, model.ServiceCoverIgnoreAll, map[uint64]bool{2: true})
	inside := insertServiceForDispatchTest(t, model.ServiceCoverIgnoreAll, map[uint64]bool{1: true})
	coverAll := insertServiceForDispatchTest(t, model.ServiceCoverAll, nil)
	stats := map[uint64]model.ServiceResponseItem{outside: {}, inside: {}, coverAll: {}}

	owner := &model.User{Common: model.Common{ID: 100}, Role: model.RoleMember}
	tok := &model.APIToken{ID: 34, UserID: 100}
	tok.SetServerIDs([]uint64{1})

	c := newServiceVisibilityCtx(owner)
	require.Len(t, filterServiceStatsForViewer(c, stats), 3, "JWT session must not be narrowed")

	c.Set(model.CtxKeyAPIToken, tok)
	visible := filterServiceStatsForViewer(c, stats)
	assert.Contains(t, visible, inside)
	assert.NotContains(t, visible, outside, "PAT [1] must NOT read a monitor probed only from server 2")
	assert.NotContains(t, visible, coverAll, "PAT [1] must NOT read a ServiceCoverAll monitor that is probed from server 2 as well")

	c.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(outside, 10)}}
	_, err := getServiceHistory(c)
	assert.Error(t, err, "PAT [1] must NOT learn the name of a monitor probed only from server 2")

	c.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(inside, 10)}}
	history, err := getServiceHistory(c)
	require.NoError(t, err)
	assert.Equal(t, inside, history.ServiceID)
}
