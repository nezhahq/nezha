package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func oauthManagementResult(t *testing.T, response *httptest.ResponseRecorder, success bool) json.RawMessage {
	t.Helper()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var body model.CommonResponse[json.RawMessage]
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, success, body.Success, response.Body.String())
	if !success {
		require.NotEmpty(t, body.Error)
	}
	return body.Data
}

func oauthManagementOtherGrant(t *testing.T, clientID string) model.MCPOAuthGrant {
	t.Helper()
	user := model.User{Common: model.Common{ID: 200}, Username: "bob", Role: model.RoleMember}
	require.NoError(t, singleton.DB.Create(&user).Error)
	permission, _ := mkToken(t, user.ID, []string{model.ScopeServerRead}, []uint64{9})
	grant := model.MCPOAuthGrant{ID: "other-user-authorization", APITokenID: permission.ID, ClientID: clientID, Resource: "https://example.com/mcp", AccessHash: model.HashAPIToken("other-user-access"), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, singleton.DB.Create(&grant).Error)
	return grant
}

func TestMCPOAuthManagementOwnershipAndRedaction(t *testing.T) {
	s := setupMCPOAuthSession(t)
	tokens := s.tokens(t)
	other := oauthManagementOtherGrant(t, s.clientID)
	var own model.MCPOAuthGrant
	require.NoError(t, singleton.DB.Where("id <> ?", other.ID).First(&own).Error)
	active, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer patConnectionRegistryShared.register(own.APITokenID, cancel)()
	response := s.request(t, "GET", "/api/v1/mcp/oauth/grants", nil, true)
	data := oauthManagementResult(t, response, true)
	var page struct {
		Items []struct {
			ID         string   `json:"id"`
			UserID     uint64   `json:"user_id"`
			ClientName string   `json:"client_name"`
			Scopes     []string `json:"scopes"`
			ServerIDs  []uint64 `json:"server_ids"`
			Status     string   `json:"status"`
		} `json:"items"`
		Total int64 `json:"total"`
	}
	require.NoError(t, json.Unmarshal(data, &page))
	require.EqualValues(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	require.Equal(t, own.ID, page.Items[0].ID)
	require.Equal(t, s.userID, page.Items[0].UserID)
	require.Equal(t, "ChatGPT test", page.Items[0].ClientName)
	require.Equal(t, []string{model.ScopeInventoryRead}, page.Items[0].Scopes)
	require.Equal(t, []uint64{7}, page.Items[0].ServerIDs)
	require.Equal(t, "active", page.Items[0].Status)
	for _, secret := range []string{tokens.AccessToken, tokens.RefreshToken, own.AccessHash, own.RefreshHash, other.ID, "secret_hash", "token_hash"} {
		require.NotContains(t, response.Body.String(), secret)
	}
	oauthManagementResult(t, s.request(t, "GET", "/api/v1/mcp/oauth/grants?all=true", nil, true), false)
	oauthManagementResult(t, s.request(t, "DELETE", "/api/v1/mcp/oauth/grants/"+other.ID, nil, true), false)
	require.NoError(t, singleton.DB.First(&other, "id = ?", other.ID).Error)
	oauthManagementResult(t, s.request(t, "DELETE", "/api/v1/mcp/oauth/grants/"+own.ID, nil, true), true)
	require.ErrorIs(t, active.Err(), context.Canceled)
	require.Equal(t, 401, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
	require.Equal(t, 400, s.request(t, "POST", mcpOAuthPath+"/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {s.clientID}, "resource": {"https://example.com/mcp"}, "refresh_token": {tokens.RefreshToken}}, false).Code)
	var count int64
	require.NoError(t, singleton.DB.Model(&model.APIToken{}).Where("id = ?", own.APITokenID).Count(&count).Error)
	require.Zero(t, count)
}

func TestMCPOAuthManagementRequiresBrowserSessionAndCSRF(t *testing.T) {
	s := setupMCPOAuthSession(t)
	tokens := s.tokens(t)
	var grant model.MCPOAuthGrant
	require.NoError(t, singleton.DB.First(&grant).Error)
	_, pat := mkToken(t, s.userID, []string{model.ScopeNezhaAll}, nil)
	paths := []struct{ method, path string }{
		{"GET", "/api/v1/mcp/oauth/grants"}, {"DELETE", "/api/v1/mcp/oauth/grants/" + grant.ID},
		{"GET", "/api/v1/mcp/oauth/clients"}, {"PUT", "/api/v1/mcp/oauth/clients/" + s.clientID},
	}
	for _, route := range paths {
		for _, credential := range []string{"", pat, tokens.AccessToken} {
			request := httptest.NewRequest(route.method, "https://example.com"+route.path, nil)
			if credential != "" {
				request.Header.Set("Authorization", "Bearer "+credential)
			}
			response := httptest.NewRecorder()
			s.router.ServeHTTP(response, request)
			if credential == pat {
				require.Equal(t, 403, response.Code, route.path)
			} else {
				// The existing JWT middleware reports unauthenticated API requests
				// as HTTP 200 with ApiErrorUnauthorized, not an OAuth HTTP 401.
				require.Equal(t, 200, response.Code, route.path)
				require.JSONEq(t, `{"error":"ApiErrorUnauthorized"}`, response.Body.String())
			}
		}
	}
	s.csrf = ""
	for _, route := range paths {
		if route.method == "GET" {
			continue
		}
		require.Equal(t, 403, s.request(t, route.method, route.path, map[string]bool{"disabled": true}, true).Code)
	}
	require.Equal(t, 200, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
}

func TestMCPOAuthManagementClientDisableRevokesAllUsersAndPendingCodes(t *testing.T) {
	s := setupMCPOAuthSession(t)
	tokens := s.tokens(t)
	other := oauthManagementOtherGrant(t, s.clientID)
	active, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer patConnectionRegistryShared.register(other.APITokenID, cancel)()
	_, pat := mkToken(t, s.userID, []string{model.ScopeInventoryRead}, nil)
	pendingCode := s.approve(t)
	ticket := s.consentTicket(t)
	path := "/api/v1/mcp/oauth/clients/" + s.clientID
	oauthManagementResult(t, s.request(t, "GET", "/api/v1/mcp/oauth/clients", nil, true), false)
	oauthManagementResult(t, s.request(t, "PUT", path, map[string]bool{"disabled": true}, true), false)
	require.Equal(t, 200, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
	require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", s.userID).Update("role", model.RoleAdmin).Error)
	data := oauthManagementResult(t, s.request(t, "GET", "/api/v1/mcp/oauth/grants?all=true", nil, true), true)
	require.Contains(t, string(data), other.ID)
	require.Contains(t, string(data), "bob")
	oauthManagementResult(t, s.request(t, "PUT", path, map[string]any{}, true), false)
	oauthManagementResult(t, s.request(t, "PUT", path, map[string]bool{"disabled": true}, true), true)
	require.ErrorIs(t, active.Err(), context.Canceled)
	for _, table := range []string{"mcp_oauth_grants", "mcp_oauth_codes"} {
		var count int64
		require.NoError(t, singleton.DB.Table(table).Count(&count).Error)
		require.Zero(t, count, table)
	}
	require.Equal(t, 200, s.rpc(t, pat, "initialize", nil).Code, "client disable must not revoke independent PATs")
	require.Equal(t, 401, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
	require.Equal(t, 401, s.request(t, "POST", mcpOAuthPath+"/token", s.exchangeForm(pendingCode), false).Code)
	require.Equal(t, 401, s.request(t, "POST", mcpOAuthPath+"/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {s.clientID}, "resource": {"https://example.com/mcp"}, "refresh_token": {tokens.RefreshToken}}, false).Code)
	oauthManagementResult(t, s.request(t, "GET", "/api/v1/mcp/oauth/consent?"+s.query.Encode(), nil, true), false)
	oauthManagementResult(t, s.request(t, "POST", "/api/v1/mcp/oauth/consent", map[string]any{"consent_token": ticket, "approve": true, "scopes": []string{model.ScopeInventoryRead}, "server_ids": []uint64{7}}, true), false)
	data = oauthManagementResult(t, s.request(t, "GET", "/api/v1/mcp/oauth/clients", nil, true), true)
	require.Contains(t, string(data), `"disabled":true`)
	require.Contains(t, string(data), `"grant_count":0`)
	require.NotContains(t, string(data), "secret_hash")
	oauthManagementResult(t, s.request(t, "PUT", path, map[string]bool{"disabled": false}, true), true)
	require.Equal(t, 401, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
	require.Equal(t, 400, s.request(t, "POST", mcpOAuthPath+"/token", s.exchangeForm(pendingCode), false).Code)
	fresh := s.tokens(t)
	require.Equal(t, 200, s.rpc(t, fresh.AccessToken, "initialize", nil).Code)
}

func TestMCPOAuthManagementDisableRollsBackOnRevocationFailure(t *testing.T) {
	s := setupMCPOAuthSession(t)
	tokens := s.tokens(t)
	require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", s.userID).Update("role", model.RoleAdmin).Error)
	require.NoError(t, singleton.DB.Exec(`CREATE TRIGGER reject_oauth_grant_delete BEFORE DELETE ON mcp_oauth_grants BEGIN SELECT RAISE(ABORT, 'revocation storage unavailable'); END`).Error)
	oauthManagementResult(t, s.request(t, "PUT", "/api/v1/mcp/oauth/clients/"+s.clientID, map[string]bool{"disabled": true}, true), false)
	var client model.MCPOAuthClient
	require.NoError(t, singleton.DB.First(&client, "id = ?", s.clientID).Error)
	require.False(t, client.Disabled)
	require.Equal(t, 200, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
	var count int64
	require.NoError(t, singleton.DB.Model(&model.APIToken{}).Count(&count).Error)
	require.EqualValues(t, 1, count, "failed disable must roll back the permission deletion too")
}

func TestMCPOAuthManagementConcurrentDisableAndRefresh(t *testing.T) {
	s := setupMCPOAuthSession(t)
	tokens := s.tokens(t)
	require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", s.userID).Update("role", model.RoleAdmin).Error)
	start := make(chan struct{})
	refreshed, disabled := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() {
		<-start
		refreshed <- s.request(t, "POST", mcpOAuthPath+"/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {s.clientID}, "resource": {"https://example.com/mcp"}, "refresh_token": {tokens.RefreshToken}}, false)
	}()
	go func() {
		<-start
		disabled <- s.request(t, "PUT", "/api/v1/mcp/oauth/clients/"+s.clientID, map[string]bool{"disabled": true}, true)
	}()
	close(start)
	refreshResult, disableResult := <-refreshed, <-disabled
	oauthManagementResult(t, disableResult, true)
	if refreshResult.Code == 200 {
		var rotated mcpOAuthTokenResponse
		require.NoError(t, json.Unmarshal(refreshResult.Body.Bytes(), &rotated))
		require.NotEmpty(t, rotated.AccessToken)
		require.Equal(t, 401, s.rpc(t, rotated.AccessToken, "initialize", nil).Code)
	} else {
		require.Contains(t, []int{400, 401}, refreshResult.Code)
		require.NotContains(t, refreshResult.Body.String(), "access_token")
	}
	var count int64
	require.NoError(t, singleton.DB.Model(&model.MCPOAuthGrant{}).Count(&count).Error)
	require.Zero(t, count, "refresh must not recreate a disabled client's grant")
	require.Equal(t, 401, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
}

func TestMCPOAuthManagementAvailableWhenMCPDisabledAndPagination(t *testing.T) {
	s := setupMCPOAuthSession(t)
	s.tokens(t)
	var own model.MCPOAuthGrant
	require.NoError(t, singleton.DB.First(&own).Error)
	other := oauthManagementOtherGrant(t, s.clientID)
	require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", s.userID).Update("role", model.RoleAdmin).Error)
	singleton.Conf.SetMCPEnabled(false)
	singleton.Conf.DashboardHost = ""
	data := oauthManagementResult(t, s.request(t, "GET", "/api/v1/mcp/oauth/grants?all=true&page=2&limit=1", nil, true), true)
	var grants struct {
		Total int `json:"total"`
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(data, &grants))
	require.Equal(t, 2, grants.Total)
	require.Len(t, grants.Items, 1)
	require.Equal(t, own.ID, grants.Items[0].ID)
	require.Equal(t, "unavailable", grants.Items[0].Status)
	data = oauthManagementResult(t, s.request(t, "GET", "/api/v1/mcp/oauth/clients?page=2&limit=1", nil, true), true)
	var clients struct {
		Total int               `json:"total"`
		Items []json.RawMessage `json:"items"`
	}
	require.NoError(t, json.Unmarshal(data, &clients))
	require.Equal(t, 1, clients.Total)
	require.Empty(t, clients.Items)
	for _, query := range []string{"page=0", "page=-1", "limit=101", "limit=invalid", "all=invalid"} {
		oauthManagementResult(t, s.request(t, "GET", "/api/v1/mcp/oauth/grants?"+query, nil, true), false)
	}
	oauthManagementResult(t, s.request(t, "DELETE", "/api/v1/mcp/oauth/grants/"+other.ID, nil, true), true)
	oauthManagementResult(t, s.request(t, "DELETE", "/api/v1/mcp/oauth/grants/"+own.ID, nil, true), true)
	page := s.request(t, "GET", "/dashboard/settings/mcp-oauth", nil, false)
	require.Equal(t, 200, page.Code)
	require.Contains(t, page.Body.String(), "independent admin frontend")
}
