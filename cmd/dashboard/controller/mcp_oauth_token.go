package controller

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

var errMCPOAuthInvalidGrant = errors.New("authorization grant is invalid, expired or revoked")

func mcpOAuthClientFromForm(c *gin.Context) (*model.MCPOAuthClient, error) {
	if c.ContentType() != "application/x-www-form-urlencoded" || c.Request.URL.RawQuery != "" {
		return nil, errors.New("use a form-encoded POST without query parameters")
	}
	if err := c.Request.ParseForm(); err != nil {
		return nil, err
	}
	for _, values := range c.Request.PostForm {
		if len(values) != 1 {
			return nil, errors.New("duplicate form parameter")
		}
	}
	id, secret := c.Request.PostForm.Get("client_id"), c.Request.PostForm.Get("client_secret")
	method := "client_secret_post"
	if c.GetHeader("Authorization") != "" {
		basicID, basicSecret, ok := c.Request.BasicAuth()
		if !ok || secret != "" {
			return nil, errors.New("invalid client authentication")
		}
		formClientID := id
		var err error
		id, err = url.QueryUnescape(basicID)
		if err != nil {
			return nil, err
		}
		// A matching client_id is only an identifier, not a second auth
		// method. Reject conflicts and body secrets, not standards-compliant redundancy.
		if formClientID != "" && formClientID != id {
			return nil, errors.New("conflicting client identifiers")
		}
		secret, err = url.QueryUnescape(basicSecret)
		if err != nil {
			return nil, err
		}
		method = "client_secret_basic"
	}
	if len(id) > 64 || len(secret) > 256 {
		return nil, errors.New("invalid client credentials")
	}
	var client model.MCPOAuthClient
	if err := singleton.DB.First(&client, "id = ?", id).Error; err != nil {
		return nil, errors.New("unknown client")
	}
	if client.Disabled {
		return nil, errors.New("client disabled")
	}
	if client.AuthMethod == "none" {
		if secret != "" || c.GetHeader("Authorization") != "" {
			return nil, errors.New("public clients must not send client credentials")
		}
	} else if client.AuthMethod != method || secret == "" || !mcpOAuthHashEqual(client.SecretHash, model.HashAPIToken(secret)) {
		return nil, errors.New("invalid client credentials")
	}
	return &client, nil
}

type mcpOAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func mcpOAuthToken(c *gin.Context) {
	client, err := mcpOAuthClientFromForm(c)
	if err != nil {
		mcpOAuthError(c, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	issuer, _ := mcpOAuthIssuer()
	form := c.Request.PostForm
	if form.Get("resource") != issuer+"/mcp" {
		mcpOAuthError(c, 400, "invalid_target", "resource must match the canonical MCP URL")
		return
	}
	var response *mcpOAuthTokenResponse
	var revokedTokenID uint64
	err = mcpOAuthTransaction(func(tx *gorm.DB) error {
		// Recheck in the transaction: authentication outside it can race a disable.
		if err := mcpOAuthRequireEnabledClient(tx, client.ID); err != nil {
			return err
		}
		// Remove expired/orphaned OAuth credentials during normal use; keep
		// the existing API Token history visible in the dashboard for operators.
		if err := tx.Where("expires_at <= ? OR api_token_id NOT IN (?)", time.Now(), tx.Model(&model.APIToken{}).Select("id")).Delete(&model.MCPOAuthGrant{}).Error; err != nil {
			return err
		}
		switch form.Get("grant_type") {
		case "authorization_code":
			response, revokedTokenID, err = mcpOAuthExchangeCode(tx, client, form)
		case "refresh_token":
			response, revokedTokenID, err = mcpOAuthExchangeRefresh(tx, client, form)
		default:
			return errors.New("unsupported grant type")
		}
		if err != nil {
			return err
		}
		return tx.Model(&model.MCPOAuthClient{}).Where("id = ?", client.ID).Update("last_used_at", time.Now()).Error
	})
	if err != nil || response == nil {
		// Replay revocation is committed before returning invalid_grant; rolling
		// back that transaction would silently keep the stolen token usable.
		if err == nil && revokedTokenID != 0 {
			patConnectionRegistryShared.revokeToken(revokedTokenID)
		}
		code := "invalid_grant"
		if form.Get("grant_type") != "authorization_code" && form.Get("grant_type") != "refresh_token" {
			code = "unsupported_grant_type"
		}
		mcpOAuthError(c, 400, code, errMCPOAuthInvalidGrant.Error())
		return
	}
	c.Header("Pragma", "no-cache")
	c.JSON(http.StatusOK, response)
}

func mcpOAuthExchangeCode(tx *gorm.DB, client *model.MCPOAuthClient, form url.Values) (*mcpOAuthTokenResponse, uint64, error) {
	if !mcpOAuthVerifierValid(form.Get("code_verifier")) || len(form.Get("code")) > 128 {
		return nil, 0, errMCPOAuthInvalidGrant
	}
	var code model.MCPOAuthCode
	if tx.First(&code, "hash = ?", model.HashAPIToken(form.Get("code"))).Error != nil || !time.Now().Before(code.ExpiresAt) || code.ClientID != client.ID || code.RedirectURI != form.Get("redirect_uri") || code.Resource != form.Get("resource") || !mcpOAuthHashEqual(code.Challenge, mcpOAuthPKCE(form.Get("code_verifier"))) {
		return nil, 0, errMCPOAuthInvalidGrant
	}
	if code.GrantID != "" {
		var grant model.MCPOAuthGrant
		if err := tx.First(&grant, "id = ?", code.GrantID).Error; err != nil {
			return nil, 0, errMCPOAuthInvalidGrant
		}
		err := mcpOAuthDeleteGrant(tx, &grant)
		return nil, grant.APITokenID, err
	}
	var user model.User
	if tx.First(&user, code.UserID).Error != nil || user.TokenVersion != code.UserVersion {
		return nil, 0, errMCPOAuthInvalidGrant
	}
	var count int64
	if err := tx.Model(&model.APIToken{}).Where("user_id = ? AND name LIKE ? AND expires_at > ?", user.ID, "OAuth: %", time.Now()).Count(&count).Error; err != nil {
		return nil, 0, err
	}
	if count >= 100 {
		return nil, 0, errMCPOAuthInvalidGrant
	}
	now := time.Now()
	expiry := now.Add(mcpOAuthGrantLifetime)
	// No plaintext PAT is returned or persisted. Only the dedicated nzo_
	// credential can resolve this permission identity, and only on /mcp.
	permission := model.APIToken{UserID: user.ID, Name: "OAuth: " + client.Name, TokenHash: model.HashAPIToken(mcpOAuthRandom()), ScopesCSV: code.ScopesCSV, ServersCSV: code.ServersCSV, ExpiresAt: &expiry}
	if err := tx.Create(&permission).Error; err != nil {
		return nil, 0, err
	}
	grant := model.MCPOAuthGrant{ID: mcpOAuthRandom(), APITokenID: permission.ID, ClientID: client.ID, Resource: code.Resource, UserVersion: user.TokenVersion, ExpiresAt: expiry}
	response := mcpOAuthRotateCredentials(&grant, permission.Scopes())
	if err := tx.Create(&grant).Error; err != nil {
		return nil, 0, err
	}
	updated := tx.Model(&model.MCPOAuthCode{}).Where("hash = ? AND grant_id = ?", code.Hash, "").Update("grant_id", grant.ID)
	if updated.Error != nil {
		return nil, 0, updated.Error
	}
	if updated.RowsAffected != 1 {
		return nil, 0, errMCPOAuthInvalidGrant
	}
	return response, 0, nil
}

func mcpOAuthGrantIdentity(tx *gorm.DB, grant *model.MCPOAuthGrant) (*model.APIToken, *model.User, error) {
	if err := mcpOAuthRequireEnabledClient(tx, grant.ClientID); err != nil {
		return nil, nil, err
	}
	if !time.Now().Before(grant.ExpiresAt) {
		return nil, nil, errMCPOAuthInvalidGrant
	}
	var permission model.APIToken
	if tx.First(&permission, grant.APITokenID).Error != nil || permission.IsExpired(time.Now()) {
		return nil, nil, errMCPOAuthInvalidGrant
	}
	var user model.User
	if tx.First(&user, permission.UserID).Error != nil || user.TokenVersion != grant.UserVersion {
		return nil, nil, errMCPOAuthInvalidGrant
	}
	return &permission, &user, nil
}

func mcpOAuthExchangeRefresh(tx *gorm.DB, client *model.MCPOAuthClient, form url.Values) (*mcpOAuthTokenResponse, uint64, error) {
	plain := form.Get("refresh_token")
	id := mcpOAuthRefreshGrantID(plain)
	if id == "" {
		return nil, 0, errMCPOAuthInvalidGrant
	}
	var grant model.MCPOAuthGrant
	if tx.First(&grant, "id = ?", id).Error != nil || grant.ClientID != client.ID || grant.Resource != form.Get("resource") {
		return nil, 0, errMCPOAuthInvalidGrant
	}
	permission, _, err := mcpOAuthGrantIdentity(tx, &grant)
	if err != nil {
		return nil, 0, err
	}
	if !mcpOAuthHashEqual(grant.RefreshHash, model.HashAPIToken(plain)) {
		return nil, grant.APITokenID, mcpOAuthDeleteGrant(tx, &grant)
	}
	// Scope order and repeated names do not change the permission set.
	// Refresh cannot expand consent or silently change its permission set.
	if scope := form.Get("scope"); scope != "" {
		requested, granted := strings.Fields(scope), permission.Scopes()
		slices.Sort(requested)
		slices.Sort(granted)
		if !slices.Equal(slices.Compact(requested), slices.Compact(granted)) {
			return nil, 0, errMCPOAuthInvalidGrant
		}
	}

	previousHash := grant.RefreshHash
	response := mcpOAuthRotateCredentials(&grant, permission.Scopes())
	updated := tx.Model(&model.MCPOAuthGrant{}).Where("id = ? AND refresh_hash = ?", grant.ID, previousHash).Updates(map[string]any{"refresh_hash": grant.RefreshHash, "access_hash": grant.AccessHash, "access_expires_at": grant.AccessExpiresAt})
	if updated.Error != nil {
		return nil, 0, updated.Error
	}
	if updated.RowsAffected != 1 {
		return nil, 0, errMCPOAuthInvalidGrant
	}
	return response, 0, nil
}

func mcpOAuthRotateCredentials(grant *model.MCPOAuthGrant, scopes []string) *mcpOAuthTokenResponse {
	access, refresh := mcpOAuthAccessPrefix+mcpOAuthRandom(), mcpOAuthNewRefresh(grant.ID)
	grant.AccessHash, grant.RefreshHash = model.HashAPIToken(access), model.HashAPIToken(refresh)
	now := time.Now()
	grant.AccessExpiresAt = now.Add(mcpOAuthAccessLifetime)
	if grant.AccessExpiresAt.After(grant.ExpiresAt) {
		grant.AccessExpiresAt = grant.ExpiresAt
	}
	return &mcpOAuthTokenResponse{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", ExpiresIn: int64(grant.AccessExpiresAt.Sub(now) / time.Second), Scope: strings.Join(scopes, " ")}
}

func mcpOAuthDeleteGrant(tx *gorm.DB, grant *model.MCPOAuthGrant) error {
	if err := tx.Delete(&model.APIToken{}, grant.APITokenID).Error; err != nil {
		return err
	}
	return tx.Where("id = ?", grant.ID).Delete(&model.MCPOAuthGrant{}).Error
}

func mcpOAuthRevoke(c *gin.Context) {
	client, err := mcpOAuthClientFromForm(c)
	if err != nil {
		mcpOAuthError(c, 401, "invalid_client", "client authentication failed")
		return
	}
	plain := c.Request.PostForm.Get("token")
	var revoked uint64
	err = mcpOAuthTransaction(func(tx *gorm.DB) error {
		var grant model.MCPOAuthGrant
		query := tx.Where("client_id = ?", client.ID)
		if id := mcpOAuthRefreshGrantID(plain); id != "" {
			query = query.Where("id = ?", id)
		} else {
			query = query.Where("access_hash = ?", model.HashAPIToken(plain))
		}
		if err := query.First(&grant).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		revoked = grant.APITokenID
		return mcpOAuthDeleteGrant(tx, &grant)
	})
	if err != nil {
		mcpOAuthError(c, 503, "temporarily_unavailable", "revocation unavailable")
		return
	}
	if revoked != 0 {
		patConnectionRegistryShared.revokeToken(revoked)
	}
	c.Status(http.StatusOK)
}

func mcpOAuthAccessAuth(c *gin.Context) {
	plain := utils.BearerCredential(c.GetHeader("Authorization"))
	if !strings.HasPrefix(plain, mcpOAuthAccessPrefix) {
		return
	}
	issuer, err := mcpOAuthIssuer()
	if err != nil {
		abortAPITokenUnauthorized(c, "MCP OAuth unavailable")
		return
	}
	var grant model.MCPOAuthGrant
	if len(plain) > 128 || singleton.DB.First(&grant, "access_hash = ?", model.HashAPIToken(plain)).Error != nil || !time.Now().Before(grant.AccessExpiresAt) || grant.Resource != issuer+"/mcp" {
		abortAPITokenUnauthorized(c, "invalid or expired MCP OAuth token")
		return
	}
	permission, _, err := mcpOAuthGrantIdentity(singleton.DB, &grant)
	if err != nil {
		abortAPITokenUnauthorized(c, "MCP OAuth authorization revoked")
		return
	}
	authenticateAPITokenIdentity(c, permission)
	if !c.IsAborted() {
		c.Set(mcpOAuthGrantContextKey, &grant)
	}
}
