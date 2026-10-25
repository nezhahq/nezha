package controller

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/idcodec"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
)

func TestMCPOAuthDiscoveryThroughDashboardRouter(t *testing.T) {
	cleanup, _ := setupMCPTest(t)
	defer cleanup()
	singleton.Conf.JWTSecretKey = "oauth-discovery-test-signing-key"
	router := gin.New()
	routers(router, fstest.MapFS{})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://example.com/.well-known/oauth-protected-resource/mcp", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &metadata))
	require.Equal(t, "https://example.com/mcp", metadata["resource"])
	require.Equal(t, []any{"https://example.com"}, metadata["authorization_servers"])
}

type mcpOAuthTestSession struct {
	router       *gin.Engine
	userID       uint64
	cookie       *http.Cookie
	csrf         string
	clientID     string
	clientSecret string
	verifier     string
	query        url.Values
}

func setupMCPOAuthSession(t *testing.T) *mcpOAuthTestSession {
	t.Helper()
	// A single in-memory connection masks SQLite lock-upgrade races between
	// concurrent code exchanges. Exercise the same shared file as production.
	cleanup, uid := setupMCPTestWithDatabase(t, filepath.Join(t.TempDir(), "mcp-oauth.sqlite"))
	t.Cleanup(cleanup)
	oldLimiter, oldRegistrationLimiter := mcpOAuthPublicLimiter, mcpOAuthRegistrationLimiter
	mcpOAuthPublicLimiter, mcpOAuthRegistrationLimiter = newMCPRateLimiter(10000, 10000), newMCPRateLimiter(10000, 10000)
	t.Cleanup(func() { mcpOAuthPublicLimiter, mcpOAuthRegistrationLimiter = oldLimiter, oldRegistrationLimiter })
	singleton.Conf.JWTSecretKey = "mcp-oauth-integration-test-signing-key"
	require.NoError(t, idcodec.Init([]byte(singleton.Conf.JWTSecretKey)))
	require.NoError(t, singleton.DB.AutoMigrate(&model.MCPOAuthClient{}, &model.MCPOAuthCode{}, &model.MCPOAuthGrant{}, &model.JWTSession{}))
	db, err := singleton.DB.DB()
	require.NoError(t, err)
	db.SetMaxOpenConns(5)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	oldParser := dashboardCredentialJWTParser
	t.Cleanup(func() { dashboardCredentialJWTParser = oldParser })
	routers(router, fstest.MapFS{"admin-dist/index.html": &fstest.MapFile{Data: []byte("independent admin frontend")}})
	singleton.Conf.AdminTemplate = "admin-dist"
	var user model.User
	require.NoError(t, singleton.DB.First(&user, uid).Error)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest("POST", "https://example.com/api/v1/login", nil)
	claims, err := issueJWTSession(context, &user, 1)
	require.NoError(t, err)
	middleware, err := jwt.New(initParams())
	require.NoError(t, err)
	cookie, _, err := middleware.TokenGenerator(claims)
	require.NoError(t, err)
	session := &mcpOAuthTestSession{router: router, userID: uid, cookie: &http.Cookie{Name: "nz-jwt", Value: cookie}, csrf: issueCSRFToken(), verifier: strings.Repeat("v", 64)}
	registration := session.request(t, "POST", mcpOAuthPath+"/register", map[string]any{"client_name": "ChatGPT test", "redirect_uris": []string{"https://chatgpt.com/connector_platform_oauth_redirect"}, "token_endpoint_auth_method": "none"}, false)
	require.Equal(t, http.StatusCreated, registration.Code, registration.Body.String())
	var client map[string]any
	require.NoError(t, json.Unmarshal(registration.Body.Bytes(), &client))
	session.clientID = client["client_id"].(string)
	session.query = url.Values{"client_id": {session.clientID}, "redirect_uri": {"https://chatgpt.com/connector_platform_oauth_redirect"}, "resource": {"https://example.com/mcp"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {mcpOAuthPKCE(session.verifier)}, "state": {"client-state"}, "scope": {model.ScopeInventoryRead + " " + model.ScopeServerRead}}
	return session
}

func (s *mcpOAuthTestSession) request(t *testing.T, method, path string, body any, browser bool) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	contentType := "application/json"
	if form, ok := body.(url.Values); ok {
		data = []byte(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	} else if body != nil {
		var err error
		data, err = json.Marshal(body)
		require.NoError(t, err)
	}
	request := httptest.NewRequest(method, "https://example.com"+path, bytes.NewReader(data))
	request.Header.Set("Content-Type", contentType)
	if browser {
		request.AddCookie(s.cookie)
		request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: s.csrf})
		request.Header.Set(csrfHeaderName, s.csrf)
	}
	response := httptest.NewRecorder()
	s.router.ServeHTTP(response, request)
	return response
}

func (s *mcpOAuthTestSession) consentTicket(t *testing.T) string {
	t.Helper()
	response := s.request(t, "GET", "/api/v1/mcp/oauth/consent?"+s.query.Encode(), nil, true)
	require.Equal(t, http.StatusOK, response.Code)
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Ticket  string `json:"consent_token"`
			Servers []struct {
				ID uint64 `json:"id"`
			} `json:"servers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.True(t, body.Success, response.Body.String())
	require.NotEmpty(t, body.Data.Ticket)
	require.Len(t, body.Data.Servers, 1)
	require.Equal(t, uint64(7), body.Data.Servers[0].ID)
	return body.Data.Ticket
}

func (s *mcpOAuthTestSession) approve(t *testing.T) string {
	t.Helper()
	response := s.request(t, "POST", "/api/v1/mcp/oauth/consent", map[string]any{"consent_token": s.consentTicket(t), "approve": true, "scopes": []string{model.ScopeInventoryRead}, "server_ids": []uint64{7}}, true)
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Redirect string `json:"redirect_uri"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.True(t, body.Success, response.Body.String())
	callback, err := url.Parse(body.Data.Redirect)
	require.NoError(t, err)
	require.Equal(t, "chatgpt.com", callback.Host)
	require.Equal(t, "client-state", callback.Query().Get("state"))
	require.Equal(t, "https://example.com", callback.Query().Get("iss"))
	require.NotEmpty(t, callback.Query().Get("code"))
	return callback.Query().Get("code")
}

func (s *mcpOAuthTestSession) exchangeForm(code string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "client_id": {s.clientID}, "code": {code}, "code_verifier": {s.verifier}, "resource": {"https://example.com/mcp"}, "redirect_uri": {s.query.Get("redirect_uri")}}
}

func (s *mcpOAuthTestSession) tokens(t *testing.T) *mcpOAuthTokenResponse {
	t.Helper()
	response := s.request(t, "POST", mcpOAuthPath+"/token", s.exchangeForm(s.approve(t)), false)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var tokens mcpOAuthTokenResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &tokens))
	require.NotEmpty(t, tokens.AccessToken)
	require.NotEmpty(t, tokens.RefreshToken)
	return &tokens
}

func (s *mcpOAuthTestSession) rpc(t *testing.T, token, method string, params any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	request := httptest.NewRequest("POST", "https://example.com/mcp", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	s.router.ServeHTTP(response, request)
	return response
}

func TestMCPOAuthAuthorizationEndToEnd(t *testing.T) {
	s := setupMCPOAuthSession(t)
	redirect := s.request(t, "GET", mcpOAuthPath+"/authorize?"+s.query.Encode(), nil, false)
	require.Equal(t, http.StatusFound, redirect.Code)
	require.Contains(t, redirect.Header().Get("Location"), "https://example.com/dashboard/mcp/authorize?")
	var count int64
	require.NoError(t, singleton.DB.Model(&model.MCPOAuthCode{}).Count(&count).Error)
	require.Zero(t, count, "GET must not grant access")
	page := s.request(t, "GET", mcpOAuthFrontendPath, nil, false)
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), "independent admin frontend")
	missing := s.rpc(t, "", "initialize", nil)
	require.Equal(t, http.StatusUnauthorized, missing.Code)
	require.Contains(t, missing.Header().Get("WWW-Authenticate"), "https://example.com/.well-known/oauth-protected-resource/mcp")
	tokens := s.tokens(t)
	require.Equal(t, "Bearer", tokens.TokenType)
	require.Equal(t, int64(3600), tokens.ExpiresIn)
	require.Equal(t, model.ScopeInventoryRead, tokens.Scope)
	var grant model.MCPOAuthGrant
	require.NoError(t, singleton.DB.First(&grant).Error)
	require.Equal(t, model.HashAPIToken(tokens.AccessToken), grant.AccessHash)
	require.Equal(t, model.HashAPIToken(tokens.RefreshToken), grant.RefreshHash)
	var permission model.APIToken
	require.NoError(t, singleton.DB.First(&permission, grant.APITokenID).Error)
	require.Equal(t, []uint64{7}, permission.ServerIDs())
	require.Equal(t, []string{model.ScopeInventoryRead}, permission.Scopes())
	// Additional owned and foreign servers must not bypass the approved whitelist.
	for _, extra := range []struct {
		id, owner uint64
		name      string
	}{{8, s.userID, "unapproved-owned-server"}, {9, 999, "foreign-server"}} {
		server := &model.Server{}
		server.ID = extra.id
		server.Name = extra.name
		server.SetUserID(extra.owner)
		singleton.ServerShared.InsertForTest(server)
	}
	response := s.rpc(t, tokens.AccessToken, "tools/call", map[string]any{"name": "server.list", "arguments": map[string]any{}})
	require.Equal(t, http.StatusOK, response.Code)
	_, result := decodeRPC(response)
	require.NotNil(t, result)
	require.False(t, result.IsError, response.Body.String())
	require.Contains(t, response.Body.String(), "alpha")
	require.NotContains(t, response.Body.String(), "unapproved-owned-server")
	require.NotContains(t, response.Body.String(), "foreign-server")
	denied := s.rpc(t, tokens.AccessToken, "tools/call", map[string]any{"name": "server.exec", "arguments": map[string]any{"server_id": 7, "command": "echo rejected"}})
	_, deniedResult := decodeRPC(denied)
	require.NotNil(t, deniedResult)
	require.True(t, deniedResult.IsError)
	var audits []model.MCPAuditLog
	require.NoError(t, singleton.DB.Find(&audits).Error)
	require.Len(t, audits, 2)
	for _, audit := range audits {
		require.Equal(t, grant.APITokenID, audit.TokenID)
		require.Equal(t, s.userID, audit.UserID)
	}
	require.Equal(t, model.MCPOutcomeOK, audits[0].Outcome)
	require.Equal(t, model.MCPOutcomeScopeDenied, audits[1].Outcome)
	_, pat := mkToken(t, s.userID, []string{model.ScopeInventoryRead}, []uint64{7})
	patResponse := s.rpc(t, pat, "initialize", nil)
	require.Equal(t, http.StatusOK, patResponse.Code)
	require.Contains(t, patResponse.Body.String(), "nezha-mcp")
	request := httptest.NewRequest("GET", "https://example.com/api/v1/api-tokens", nil)
	request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	rest := httptest.NewRecorder()
	s.router.ServeHTTP(rest, request)
	require.Contains(t, rest.Body.String(), "ApiErrorUnauthorized")
	require.NotContains(t, rest.Body.String(), "OAuth: ChatGPT test")
}

func TestMCPOAuthRejectsInvalidAuthorizationParameters(t *testing.T) {
	for _, scenario := range []struct{ field, value string }{
		{"redirect_uri", "https://attacker.example/callback"}, {"redirect_uri", "https://chatgpt.com/connector_platform_oauth_redirect/"}, {"code_challenge_method", "plain"}, {"code_challenge", "short"}, {"resource", "https://attacker.example/mcp"}, {"resource", ""}, {"scope", model.ScopeNezhaAll}, {"response_type", "token"}, {"client_id", "missing"},
	} {
		t.Run(scenario.field+"="+scenario.value, func(t *testing.T) {
			s := setupMCPOAuthSession(t)
			s.query.Set(scenario.field, scenario.value)
			response := s.request(t, "GET", mcpOAuthPath+"/authorize?"+s.query.Encode(), nil, false)
			if scenario.field == "redirect_uri" || scenario.field == "client_id" {
				require.Equal(t, 400, response.Code)
				require.Empty(t, response.Header().Get("Location"))
				require.Contains(t, response.Body.String(), "invalid_request")
			} else {
				require.Equal(t, http.StatusFound, response.Code)
				callback, err := url.Parse(response.Header().Get("Location"))
				require.NoError(t, err)
				require.Equal(t, "chatgpt.com", callback.Host)
				require.NotEmpty(t, callback.Query().Get("error"))
				require.Empty(t, callback.Query().Get("code"))
			}
		})
	}
}

func TestMCPOAuthConsentRejectsEscalationAndCSRF(t *testing.T) {
	s := setupMCPOAuthSession(t)
	ticket := s.consentTicket(t)
	for _, scenario := range []struct {
		name    string
		changes map[string]any
	}{
		{"scope escalation", map[string]any{"scopes": []string{model.ScopeServerExec}}},
		{"missing scope", map[string]any{"scopes": []string{}}},
		{"foreign server", map[string]any{"server_ids": []uint64{999}}},
		{"implicit unrestricted servers", map[string]any{"server_ids": []uint64{}}},
		{"ambiguous server consent", map[string]any{"all_servers": true}},
		{"tampered ticket", map[string]any{"consent_token": ticket + "x"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			body := map[string]any{"consent_token": ticket, "approve": true, "scopes": []string{model.ScopeInventoryRead}, "server_ids": []uint64{7}}
			for key, value := range scenario.changes {
				body[key] = value
			}
			response := s.request(t, "POST", "/api/v1/mcp/oauth/consent", body, true)
			var failure model.CommonResponse[any]
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &failure))
			require.False(t, failure.Success)
			require.NotEmpty(t, failure.Error)
			require.NotContains(t, response.Body.String(), `"redirect_uri":`)
		})
	}
	body := map[string]any{"consent_token": ticket, "approve": true, "scopes": []string{model.ScopeInventoryRead}, "server_ids": []uint64{7}}
	csrf := s.csrf
	s.csrf = ""
	response := s.request(t, "POST", "/api/v1/mcp/oauth/consent", body, true)
	s.csrf = csrf
	require.Equal(t, http.StatusForbidden, response.Code)
	_, pat := mkToken(t, s.userID, []string{model.ScopeInventoryRead}, nil)
	request := httptest.NewRequest("GET", "https://example.com/api/v1/mcp/oauth/consent?"+s.query.Encode(), nil)
	request.Header.Set("Authorization", "Bearer "+pat)
	patResponse := httptest.NewRecorder()
	s.router.ServeHTTP(patResponse, request)
	require.Equal(t, 403, patResponse.Code)
	var count int64
	require.NoError(t, singleton.DB.Model(&model.MCPOAuthCode{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestMCPOAuthDenialAndLoginBinding(t *testing.T) {
	s := setupMCPOAuthSession(t)
	ticket := s.consentTicket(t)
	denied := s.request(t, "POST", "/api/v1/mcp/oauth/consent", map[string]any{"consent_token": ticket, "approve": false}, true)
	require.Contains(t, denied.Body.String(), "access_denied")
	require.Contains(t, denied.Body.String(), "iss=https%3A%2F%2Fexample.com")
	require.Contains(t, denied.Body.String(), "state=client-state")
	var count int64
	require.NoError(t, singleton.DB.Model(&model.MCPOAuthCode{}).Count(&count).Error)
	require.Zero(t, count)
	var user model.User
	require.NoError(t, singleton.DB.First(&user, s.userID).Error)
	user.ID++
	_, err := mcpOAuthReadConsentTicket(ticket, &user)
	require.Error(t, err)
	user.ID--
	user.TokenVersion++
	_, err = mcpOAuthReadConsentTicket(ticket, &user)
	require.Error(t, err)
}

func TestMCPOAuthCodeExchangeBindingsAndReuse(t *testing.T) {
	s := setupMCPOAuthSession(t)
	code := s.approve(t)
	for _, scenario := range []struct{ field, value string }{{"code_verifier", strings.Repeat("x", 64)}, {"redirect_uri", "https://attacker.example/callback"}, {"resource", "https://other.example/mcp"}, {"code", "unknown"}, {"client_id", "unknown"}} {
		form := s.exchangeForm(code)
		form.Set(scenario.field, scenario.value)
		response := s.request(t, "POST", mcpOAuthPath+"/token", form, false)
		require.NotEqual(t, http.StatusOK, response.Code, scenario.field)
		require.NotContains(t, response.Body.String(), "access_token")
	}
	response := s.request(t, "POST", mcpOAuthPath+"/token", s.exchangeForm(code), false)
	require.Equal(t, 200, response.Code, response.Body.String())
	var token mcpOAuthTokenResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &token))
	replayed := s.request(t, "POST", mcpOAuthPath+"/token", s.exchangeForm(code), false)
	require.Equal(t, 400, replayed.Code)
	require.Contains(t, replayed.Body.String(), "invalid_grant")
	require.Equal(t, 401, s.rpc(t, token.AccessToken, "initialize", nil).Code, "reusing the authorization code revokes its grant")
}

func TestMCPOAuthRefreshRotationReplayAndRevocation(t *testing.T) {
	s := setupMCPOAuthSession(t)
	tokens := s.tokens(t)
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {s.clientID}, "resource": {"https://example.com/mcp"}, "refresh_token": {tokens.RefreshToken}}
	refresh.Set("scope", model.ScopeServerExec)
	escalation := s.request(t, "POST", mcpOAuthPath+"/token", refresh, false)
	require.Equal(t, 400, escalation.Code)
	refresh.Del("scope")
	response := s.request(t, "POST", mcpOAuthPath+"/token", refresh, false)
	require.Equal(t, 200, response.Code, response.Body.String())
	var rotated mcpOAuthTokenResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &rotated))
	require.NotEqual(t, tokens.RefreshToken, rotated.RefreshToken)
	require.NotEqual(t, tokens.AccessToken, rotated.AccessToken)
	require.Equal(t, tokens.Scope, rotated.Scope)
	require.Equal(t, 401, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
	require.Equal(t, 200, s.rpc(t, rotated.AccessToken, "initialize", nil).Code)
	forged := url.Values{"grant_type": {"refresh_token"}, "client_id": {s.clientID}, "resource": {"https://example.com/mcp"}, "refresh_token": {rotated.RefreshToken + "x"}}
	require.Equal(t, 400, s.request(t, "POST", mcpOAuthPath+"/token", forged, false).Code)
	require.Equal(t, 200, s.rpc(t, rotated.AccessToken, "initialize", nil).Code, "forged handles must not revoke a grant")
	replay := s.request(t, "POST", mcpOAuthPath+"/token", refresh, false)
	require.Equal(t, 400, replay.Code)
	require.Equal(t, 401, s.rpc(t, rotated.AccessToken, "initialize", nil).Code, "refresh replay revokes the token family")
	tokens = s.tokens(t)
	revoke := s.request(t, "POST", mcpOAuthPath+"/revoke", url.Values{"client_id": {s.clientID}, "token": {tokens.RefreshToken}}, false)
	require.Equal(t, 200, revoke.Code)
	require.Equal(t, 401, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
	refresh.Set("refresh_token", tokens.RefreshToken)
	require.Equal(t, 400, s.request(t, "POST", mcpOAuthPath+"/token", refresh, false).Code)
}

func TestMCPOAuthTokenLifecycleConstraints(t *testing.T) {
	for _, mode := range []string{"delete permission", "access expiry", "grant expiry", "password rotation", "user deletion", "MCP disabled", "audience change"} {
		t.Run(mode, func(t *testing.T) {
			s := setupMCPOAuthSession(t)
			tokens := s.tokens(t)
			var grant model.MCPOAuthGrant
			require.NoError(t, singleton.DB.First(&grant).Error)
			switch mode {
			case "delete permission":
				response := s.request(t, "DELETE", "/api/v1/api-tokens/"+strconv.FormatUint(grant.APITokenID, 10), nil, true)
				require.Contains(t, response.Body.String(), `"success":true`)
			case "access expiry":
				require.NoError(t, singleton.DB.Model(&grant).Update("access_expires_at", time.Now().Add(-time.Second)).Error)
			case "grant expiry":
				require.NoError(t, singleton.DB.Model(&grant).Update("expires_at", time.Now().Add(-time.Second)).Error)
			case "password rotation":
				require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", s.userID).Update("token_version", 1).Error)
			case "user deletion":
				require.NoError(t, singleton.DB.Delete(&model.User{}, s.userID).Error)
			case "MCP disabled":
				singleton.Conf.SetMCPEnabled(false)
			case "audience change":
				singleton.Conf.DashboardHost = "other.example"
			}
			require.Equal(t, 401, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
			if mode != "access expiry" {
				require.NotEqual(t, 200, s.request(t, "POST", mcpOAuthPath+"/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {s.clientID}, "resource": {"https://example.com/mcp"}, "refresh_token": {tokens.RefreshToken}}, false).Code)
			}
		})
	}
}

func TestMCPOAuthClientRegistrationAndAuthentication(t *testing.T) {
	s := setupMCPOAuthSession(t)
	for _, uri := range []string{"http://client.example/callback", "https://user:password@client.example/callback", "https://client.example/callback#fragment", "https://client.example/callback?code=prefilled", "https://client.example/callback?state=prefilled", "//client.example/callback", "javascript:alert(1)"} {
		response := s.request(t, "POST", mcpOAuthPath+"/register", map[string]any{"redirect_uris": []string{uri}}, false)
		require.Equal(t, 400, response.Code, uri)
		require.NotContains(t, response.Body.String(), `"client_id":`)
	}
	for _, method := range []string{"client_secret_post", "client_secret_basic"} {
		t.Run(method, func(t *testing.T) {
			response := s.request(t, "POST", mcpOAuthPath+"/register", map[string]any{"client_name": "Confidential client", "redirect_uris": []string{s.query.Get("redirect_uri")}, "token_endpoint_auth_method": method}, false)
			require.Equal(t, 201, response.Code)
			var registered struct {
				ID     string `json:"client_id"`
				Secret string `json:"client_secret"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &registered))
			require.NotEmpty(t, registered.Secret)
			var stored model.MCPOAuthClient
			require.NoError(t, singleton.DB.First(&stored, "id = ?", registered.ID).Error)
			require.Equal(t, model.HashAPIToken(registered.Secret), stored.SecretHash)
			s.clientID = registered.ID
			s.query.Set("client_id", registered.ID)
			code := s.approve(t)
			form := s.exchangeForm(code)
			require.Equal(t, 401, s.request(t, "POST", mcpOAuthPath+"/token", form, false).Code, "client secret is mandatory")
			form.Set("client_secret", "incorrect")
			require.Equal(t, 401, s.request(t, "POST", mcpOAuthPath+"/token", form, false).Code)
			form.Del("client_secret")
			if method == "client_secret_post" {
				form.Set("client_secret", registered.Secret)
				response = s.request(t, "POST", mcpOAuthPath+"/token", form, false)
			} else {
				form.Del("client_id")
				req := httptest.NewRequest("POST", "https://example.com"+mcpOAuthPath+"/token", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.SetBasicAuth(registered.ID, registered.Secret)
				response = httptest.NewRecorder()
				s.router.ServeHTTP(response, req)
			}
			require.Equal(t, 200, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "access_token")
		})
	}
}

func TestMCPOAuthExpiredAndCrossClientGrants(t *testing.T) {
	s := setupMCPOAuthSession(t)
	var user model.User
	require.NoError(t, singleton.DB.First(&user, s.userID).Error)
	ticket, err := mcpOAuthReadConsentTicket(s.consentTicket(t), &user)
	require.NoError(t, err)
	ticket.ExpiresAt = time.Now().Add(-time.Second).Unix()
	encoded, err := json.Marshal(ticket)
	require.NoError(t, err)
	payload := base64.RawURLEncoding.EncodeToString(encoded)
	_, err = mcpOAuthReadConsentTicket(payload+"."+mcpOAuthSign("mcp-consent-v1", payload), &user)
	require.Error(t, err)
	code := s.approve(t)
	require.NoError(t, singleton.DB.Model(&model.MCPOAuthCode{}).Where("hash = ?", model.HashAPIToken(code)).Update("expires_at", time.Now().Add(-time.Second)).Error)
	require.Equal(t, 400, s.request(t, "POST", mcpOAuthPath+"/token", s.exchangeForm(code), false).Code)
	code = s.approve(t)
	tokens := s.tokens(t)
	registration := s.request(t, "POST", mcpOAuthPath+"/register", map[string]any{"redirect_uris": []string{s.query.Get("redirect_uri")}, "token_endpoint_auth_method": "none"}, false)
	require.Equal(t, 201, registration.Code)
	var other struct {
		ID string `json:"client_id"`
	}
	require.NoError(t, json.Unmarshal(registration.Body.Bytes(), &other))
	require.NotEmpty(t, other.ID)
	form := s.exchangeForm(code)
	form.Set("client_id", other.ID)
	require.Equal(t, 400, s.request(t, "POST", mcpOAuthPath+"/token", form, false).Code)
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {other.ID}, "resource": {"https://example.com/mcp"}, "refresh_token": {tokens.RefreshToken}}
	require.Equal(t, 400, s.request(t, "POST", mcpOAuthPath+"/token", refresh, false).Code)
	revoke := s.request(t, "POST", mcpOAuthPath+"/revoke", url.Values{"client_id": {other.ID}, "token": {tokens.RefreshToken}}, false)
	require.Equal(t, 200, revoke.Code)
	require.Equal(t, 200, s.rpc(t, tokens.AccessToken, "initialize", nil).Code, "another client cannot revoke this grant")
	form = s.exchangeForm(code)
	form.Add("code", code)
	require.Equal(t, 401, s.request(t, "POST", mcpOAuthPath+"/token", form, false).Code)
}

func TestMCPOAuthConcurrentCodeExchangeIsSingleUse(t *testing.T) {
	s := setupMCPOAuthSession(t)
	code := s.approve(t)
	form := s.exchangeForm(code).Encode()
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		request := httptest.NewRequest("POST", "https://example.com"+mcpOAuthPath+"/token", strings.NewReader(form))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		go func() {
			response := httptest.NewRecorder()
			s.router.ServeHTTP(response, request)
			responses <- response
		}()
	}
	successes := 0
	for range 2 {
		response := <-responses
		if response.Code == 200 {
			successes++
		} else {
			require.Equal(t, 400, response.Code)
			require.Contains(t, response.Body.String(), "invalid_grant")
		}
	}
	require.Equal(t, 1, successes)
	var count int64
	require.NoError(t, singleton.DB.Model(&model.MCPOAuthGrant{}).Count(&count).Error)
	require.Zero(t, count, "the detected replay revokes the issued grant")
}

func TestMCPOAuthConcurrentRefreshRevokesTokenFamily(t *testing.T) {
	s := setupMCPOAuthSession(t)
	tokens := s.tokens(t)
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {s.clientID}, "resource": {"https://example.com/mcp"}, "refresh_token": {tokens.RefreshToken}}.Encode()
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		request := httptest.NewRequest("POST", "https://example.com"+mcpOAuthPath+"/token", strings.NewReader(form))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		go func() {
			response := httptest.NewRecorder()
			s.router.ServeHTTP(response, request)
			responses <- response
		}()
	}
	successes := 0
	var rotated mcpOAuthTokenResponse
	for range 2 {
		response := <-responses
		if response.Code == 200 {
			successes++
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &rotated))
		} else {
			require.Equal(t, 400, response.Code)
			require.Contains(t, response.Body.String(), "invalid_grant")
		}
	}
	require.Equal(t, 1, successes)
	require.NotEmpty(t, rotated.AccessToken)
	require.Equal(t, 401, s.rpc(t, rotated.AccessToken, "initialize", nil).Code)
	var count int64
	require.NoError(t, singleton.DB.Model(&model.MCPOAuthGrant{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestMCPOAuthMetadataCanonicalOriginAndBrowserHeaders(t *testing.T) {
	s := setupMCPOAuthSession(t)
	request := httptest.NewRequest("GET", "https://attacker.example/.well-known/oauth-authorization-server", nil)
	request.Header.Set("X-Forwarded-Host", "attacker.example")
	request.Header.Set("X-Forwarded-Proto", "http")
	response := httptest.NewRecorder()
	s.router.ServeHTTP(response, request)
	require.Equal(t, 200, response.Code)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &metadata))
	require.Equal(t, "https://example.com", metadata["issuer"])
	require.Equal(t, []any{"S256"}, metadata["code_challenge_methods_supported"])
	require.Equal(t, true, metadata["authorization_response_iss_parameter_supported"])
	require.NotContains(t, response.Body.String(), "attacker.example")
	page := s.request(t, "GET", mcpOAuthFrontendPath, nil, false)
	require.Equal(t, "DENY", page.Header().Get("X-Frame-Options"))
	require.Equal(t, "frame-ancestors 'none'", page.Header().Get("Content-Security-Policy"))
	singleton.Conf.DashboardHost = ""
	require.Equal(t, 503, s.request(t, "GET", "/.well-known/oauth-protected-resource/mcp", nil, false).Code)
	_, pat := mkToken(t, s.userID, []string{model.ScopeInventoryRead}, nil)
	require.Equal(t, 200, s.rpc(t, pat, "initialize", nil).Code, "OAuth configuration must not break existing PAT clients")
}

func TestMCPOAuthToolAnnotationsPreserveWriteConfirmation(t *testing.T) {
	seen := make(map[string]bool)
	for _, tool := range buildToolDescriptors() {
		seen[tool.Name] = true
		if tool.Name == "server.list" {
			require.True(t, tool.Annotations["readOnlyHint"])
			require.False(t, tool.Annotations["destructiveHint"])
		}
		if tool.Name == "server.exec" || tool.Name == "fs.write" || tool.Name == "fs.delete" {
			require.False(t, tool.Annotations["readOnlyHint"])
			require.True(t, tool.Annotations["destructiveHint"])
		}
	}
	for _, name := range []string{"server.list", "server.exec", "fs.write", "fs.delete"} {
		require.True(t, seen[name], "missing tool %s", name)
	}
	require.False(t, mcpToolAnnotations("future.tool")["readOnlyHint"])
}

func TestMCPOAuthCredentialsNeverForwardToNATTunnels(t *testing.T) {
	s := setupMCPOAuthSession(t)
	tokens := s.tokens(t)
	for _, value := range []string{tokens.AccessToken, tokens.RefreshToken} {
		require.True(t, IsDashboardCredential("Bearer "+value), "MCP OAuth credentials belong to the dashboard, not a tunneled service")
		require.True(t, IsDashboardCredentialValue(value), "query/cookie transports must receive the same protection")
	}
	require.NoError(t, singleton.DB.Where("1 = 1").Delete(&model.MCPOAuthGrant{}).Error)
	for _, value := range []string{tokens.AccessToken, tokens.RefreshToken} {
		require.True(t, IsDashboardCredentialValue(value), "expired or removed OAuth credentials must still be stripped")
	}
	require.False(t, IsDashboardCredential("Bearer external-service-token"))
}

func TestMCPOAuthBasicAuthenticationWithMatchingClientID(t *testing.T) {
	s := setupMCPOAuthSession(t)
	registration := s.request(t, "POST", mcpOAuthPath+"/register", map[string]any{"redirect_uris": []string{s.query.Get("redirect_uri")}, "token_endpoint_auth_method": "client_secret_basic"}, false)
	require.Equal(t, 201, registration.Code)
	var client struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	require.NoError(t, json.Unmarshal(registration.Body.Bytes(), &client))
	s.clientID = client.ID
	s.query.Set("client_id", client.ID)
	form := s.exchangeForm(s.approve(t))
	for _, conflict := range []struct{ field, value string }{{"client_id", "conflicting-client"}, {"client_secret", client.Secret}} {
		badForm, err := url.ParseQuery(form.Encode())
		require.NoError(t, err)
		badForm.Set(conflict.field, conflict.value)
		badRequest := httptest.NewRequest("POST", "https://example.com"+mcpOAuthPath+"/token", strings.NewReader(badForm.Encode()))
		badRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		badRequest.SetBasicAuth(client.ID, client.Secret)
		rejected := httptest.NewRecorder()
		s.router.ServeHTTP(rejected, badRequest)
		require.Equal(t, 401, rejected.Code)
		require.NotContains(t, rejected.Body.String(), "access_token")
	}
	request := httptest.NewRequest("POST", "https://example.com"+mcpOAuthPath+"/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(client.ID, client.Secret)
	response := httptest.NewRecorder()
	s.router.ServeHTTP(response, request)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"access_token":`)
}

func TestMCPOAuthRefreshScopeOrderIsNotSignificant(t *testing.T) {
	s := setupMCPOAuthSession(t)
	consent := s.request(t, "POST", "/api/v1/mcp/oauth/consent", map[string]any{"consent_token": s.consentTicket(t), "approve": true, "scopes": []string{model.ScopeInventoryRead, model.ScopeServerRead}, "server_ids": []uint64{7}}, true)
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Redirect string `json:"redirect_uri"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(consent.Body.Bytes(), &body))
	require.True(t, body.Success)
	callback, err := url.Parse(body.Data.Redirect)
	require.NoError(t, err)
	exchanged := s.request(t, "POST", mcpOAuthPath+"/token", s.exchangeForm(callback.Query().Get("code")), false)
	require.Equal(t, 200, exchanged.Code)
	var tokens mcpOAuthTokenResponse
	require.NoError(t, json.Unmarshal(exchanged.Body.Bytes(), &tokens))
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {s.clientID}, "resource": {"https://example.com/mcp"}, "refresh_token": {tokens.RefreshToken}, "scope": {model.ScopeServerRead + " " + model.ScopeInventoryRead}}
	response := s.request(t, "POST", mcpOAuthPath+"/token", refresh, false)
	require.Equal(t, 200, response.Code, response.Body.String())
	var rotated mcpOAuthTokenResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &rotated))
	require.Equal(t, tokens.Scope, rotated.Scope)
	require.NotEqual(t, tokens.RefreshToken, rotated.RefreshToken)
}

func TestMCPOAuthBearerSchemeIsCaseInsensitive(t *testing.T) {
	s := setupMCPOAuthSession(t)
	tokens := s.tokens(t)
	for _, scheme := range []string{"bearer", "BEARER", "bEaReR"} {
		t.Run(scheme, func(t *testing.T) {
			request := httptest.NewRequest("POST", "https://example.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", scheme+" "+tokens.AccessToken)
			response := httptest.NewRecorder()
			s.router.ServeHTTP(response, request)
			require.Equal(t, 200, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "nezha-mcp")
			require.True(t, IsDashboardCredential(scheme+" "+tokens.AccessToken), "accepted credentials must also be stripped by NAT ingress")
		})
	}
}

func TestMCPOAuthAuthorizationErrorsReturnToTrustedClient(t *testing.T) {
	for _, scenario := range []struct{ field, value, code string }{{"scope", "unavailable:scope", "invalid_scope"}, {"response_type", "token", "unsupported_response_type"}, {"code_challenge_method", "plain", "invalid_request"}, {"resource", "https://other.example/mcp", "invalid_target"}} {
		t.Run(scenario.field, func(t *testing.T) {
			s := setupMCPOAuthSession(t)
			s.query.Set(scenario.field, scenario.value)
			response := s.request(t, "GET", mcpOAuthPath+"/authorize?"+s.query.Encode(), nil, false)
			require.Equal(t, http.StatusFound, response.Code)
			callback, err := url.Parse(response.Header().Get("Location"))
			require.NoError(t, err)
			require.Equal(t, "https://chatgpt.com/connector_platform_oauth_redirect", callback.Scheme+"://"+callback.Host+callback.Path)
			require.Equal(t, scenario.code, callback.Query().Get("error"))
			require.Equal(t, "client-state", callback.Query().Get("state"))
			require.Equal(t, "https://example.com", callback.Query().Get("iss"))
			require.Empty(t, callback.Query().Get("code"))
		})
	}
}
