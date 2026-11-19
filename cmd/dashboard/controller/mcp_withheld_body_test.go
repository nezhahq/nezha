package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// withheldBody runs beforeFirstRead when the handler first asks for the body:
// the order a client gets by sending the request head and keeping the body back.
type withheldBody struct {
	io.Reader
	beforeFirstRead func()
	once            sync.Once
}

func (b *withheldBody) Read(p []byte) (int, error) {
	b.once.Do(b.beforeFirstRead)
	return b.Reader.Read(p)
}

// The credential is authenticated on the request head, but the client decides
// when the tools/call body follows. Whatever invalidates the credential in
// between must stop the call, not only an explicit revocation that is still
// remembered by the revocation registry.
func TestMCPToolsCallWithheldAcrossInvalidationIsRefused(t *testing.T) {
	cases := []struct {
		credential, mode, outcome string
	}{
		{"oauth", "still valid", model.MCPOutcomeOK},
		{"oauth", "access expiry", model.MCPOutcomePermDenied},
		{"oauth", "grant expiry", model.MCPOutcomePermDenied},
		{"oauth", "password rotation", model.MCPOutcomePermDenied},
		{"oauth", "refresh rotation", model.MCPOutcomePermDenied},
		{"oauth", "user deletion", model.MCPOutcomePermDenied},
		{"oauth", "audience change", model.MCPOutcomePermDenied},
		{"oauth", "MCP disabled", model.MCPOutcomeMCPDisabled},
		{"pat", "still valid", model.MCPOutcomeOK},
		// A row deleted without a live tombstone: a revocation older than revokeTombstoneTTL.
		{"pat", "forgotten revocation", model.MCPOutcomePermDenied},
		{"pat", "token expiry", model.MCPOutcomePermDenied},
		{"pat", "user deletion", model.MCPOutcomePermDenied},
		{"pat", "MCP disabled", model.MCPOutcomeMCPDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.credential+"/"+tc.mode, func(t *testing.T) {
			s := setupMCPOAuthSession(t)
			var (
				bearer, refresh string
				grant           model.MCPOAuthGrant
				pat             *model.APIToken
			)
			if tc.credential == "oauth" {
				tokens := s.tokens(t)
				bearer, refresh = tokens.AccessToken, tokens.RefreshToken
				require.NoError(t, singleton.DB.First(&grant).Error)
			} else {
				pat, bearer = mkToken(t, s.userID, []string{model.ScopeInventoryRead}, nil)
			}
			invalidate := func() {
				switch tc.mode {
				case "access expiry":
					require.NoError(t, singleton.DB.Model(&grant).Update("access_expires_at", time.Now().Add(-time.Second)).Error)
				case "grant expiry":
					require.NoError(t, singleton.DB.Model(&grant).Update("expires_at", time.Now().Add(-time.Second)).Error)
				case "password rotation":
					require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", s.userID).Update("token_version", 1).Error)
				case "refresh rotation":
					refreshed := s.request(t, "POST", mcpOAuthPath+"/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {s.clientID}, "resource": {"https://example.com/mcp"}, "refresh_token": {refresh}}, false)
					require.Equal(t, http.StatusOK, refreshed.Code, refreshed.Body.String())
				case "user deletion":
					require.NoError(t, singleton.DB.Delete(&model.User{}, s.userID).Error)
				case "audience change":
					singleton.Conf.DashboardHost = "other.example"
				case "MCP disabled":
					singleton.Conf.SetMCPEnabled(false)
				case "forgotten revocation":
					require.NoError(t, singleton.DB.Delete(&model.APIToken{}, pat.ID).Error)
				case "token expiry":
					require.NoError(t, singleton.DB.Model(&model.APIToken{}).Where("id = ?", pat.ID).Update("expires_at", time.Now().Add(-time.Second)).Error)
				}
			}
			raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "server.list", "arguments": map[string]any{}}})
			require.NoError(t, err)
			request := httptest.NewRequest("POST", "https://example.com/mcp", &withheldBody{Reader: bytes.NewReader(raw), beforeFirstRead: invalidate})
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+bearer)
			response := httptest.NewRecorder()
			s.router.ServeHTTP(response, request)

			_, result := decodeRPC(response)
			require.NotNil(t, result, response.Body.String())
			var audit model.MCPAuditLog
			require.NoError(t, singleton.DB.Where("tool = ?", "server.list").Last(&audit).Error)
			require.Equal(t, tc.outcome, audit.Outcome)
			if tc.outcome == model.MCPOutcomeOK {
				require.False(t, result.IsError, response.Body.String())
				require.Contains(t, response.Body.String(), "alpha")
				return
			}
			require.True(t, result.IsError, response.Body.String())
			require.NotContains(t, response.Body.String(), "alpha")
		})
	}
}
