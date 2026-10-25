package controller

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func oauthTransferURL(t *testing.T, s *mcpOAuthTestSession, access string, direction transferDirection) (*url.URL, transferEntry) {
	t.Helper()
	response := s.rpc(t, access, "tools/call", map[string]any{"name": "fs." + string(direction) + "_url", "arguments": map[string]any{"server_id": 7, "path": "/srv/authorized-file", "ttl_seconds": 600}})
	require.Equal(t, 200, response.Code)
	_, result := decodeRPC(response)
	require.NotNil(t, result)
	require.False(t, result.IsError, response.Body.String())
	require.NotEmpty(t, result.Content)
	var body struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].Text), &body))
	u, err := url.Parse(body.URL)
	require.NoError(t, err)
	require.NotEmpty(t, u.Path)
	credential := strings.TrimPrefix(u.Path, "/mcp/"+string(direction)+"/")
	raw, exists := transferEntries.Load(credential)
	require.True(t, exists)
	entry := raw.(transferEntry)
	require.NoError(t, revalidateTransferEntry(&entry))
	return u, entry
}

func TestMCPOAuthDerivedTransferRejectsInvalidatedAuthorization(t *testing.T) {
	for _, direction := range []transferDirection{transferDirDownload, transferDirUpload} {
		for _, change := range []string{"password rotation", "resource change", "client disabled", "grant deleted", "access expired", "access rotated"} {
			t.Run(string(direction)+"/"+change, func(t *testing.T) {
				s := setupMCPOAuthSession(t)
				PurgeTransferEntries()
				t.Cleanup(func() { PurgeTransferEntries() })
				tokens := s.tokens(t)
				var grant model.MCPOAuthGrant
				require.NoError(t, singleton.DB.First(&grant).Error)
				require.NoError(t, singleton.DB.Model(&model.APIToken{}).Where("id = ?", grant.APITokenID).Update("scopes_csv", model.ScopeServerRead+","+model.ScopeServerWrite).Error)
				u, _ := oauthTransferURL(t, s, tokens.AccessToken, direction)
				switch change {
				case "password rotation":
					require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", s.userID).Update("token_version", 1).Error)
				case "resource change":
					singleton.Conf.DashboardHost = "new.example.com"
				case "client disabled":
					require.NoError(t, singleton.DB.Model(&model.MCPOAuthClient{}).Where("id = ?", s.clientID).Update("disabled", true).Error)
				case "grant deleted":
					require.NoError(t, singleton.DB.Where("id = ?", grant.ID).Delete(&model.MCPOAuthGrant{}).Error)
				case "access expired":
					require.NoError(t, singleton.DB.Model(&grant).Update("access_expires_at", time.Now().Add(-time.Second)).Error)
				case "access rotated":
					response := s.request(t, "POST", mcpOAuthPath+"/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {s.clientID}, "resource": {grant.Resource}, "refresh_token": {tokens.RefreshToken}}, false)
					require.Equal(t, 200, response.Code, response.Body.String())
				}
				require.Equal(t, 401, s.rpc(t, tokens.AccessToken, "initialize", nil).Code)
				method := "GET"
				if direction == transferDirUpload {
					method = "POST"
				}
				response := s.request(t, method, u.RequestURI(), nil, false)
				require.Equal(t, 401, response.Code, "derived file URL must fail before reaching the agent: %s", response.Body.String())
			})
		}
	}
}

func TestMCPOAuthTransferBindsCredentialsAndCapsLifetime(t *testing.T) {
	s := setupMCPOAuthSession(t)
	PurgeTransferEntries()
	t.Cleanup(func() { PurgeTransferEntries() })
	tokens := s.tokens(t)
	var grant model.MCPOAuthGrant
	require.NoError(t, singleton.DB.First(&grant).Error)
	grant.AccessExpiresAt = time.Now().Add(2 * time.Minute)
	require.NoError(t, singleton.DB.Model(&grant).Update("access_expires_at", grant.AccessExpiresAt).Error)
	require.NoError(t, singleton.DB.Model(&model.APIToken{}).Where("id = ?", grant.APITokenID).Update("scopes_csv", model.ScopeServerRead).Error)
	u, entry := oauthTransferURL(t, s, tokens.AccessToken, transferDirDownload)
	require.Equal(t, grant.ID, entry.OAuthGrantID)
	require.Equal(t, grant.AccessHash, entry.OAuthAccessHash)
	require.True(t, entry.ExpiresAt.Equal(grant.AccessExpiresAt))
	require.NotContains(t, u.String(), grant.AccessHash)
	credential := strings.TrimPrefix(u.Path, "/mcp/download/")
	for _, field := range []string{"grant", "access hash"} {
		altered := entry
		if field == "grant" {
			altered.OAuthGrantID = "another-authorization"
		} else {
			altered.OAuthAccessHash = model.HashAPIToken("another-access-token")
		}
		transferEntries.Store(credential, altered)
		consumed, err := consumeTransferToken(credential, transferDirDownload)
		require.ErrorContains(t, err, "signature mismatch", field)
		require.Nil(t, consumed)
	}
	transferEntries.Store(credential, entry)
	consumed, err := consumeTransferToken(credential, transferDirDownload)
	require.NoError(t, err)
	require.NoError(t, revalidateTransferEntry(consumed))
	_, err = consumeTransferToken(credential, transferDirDownload)
	require.Error(t, err, "valid OAuth transfer URL must remain single-use")
}
