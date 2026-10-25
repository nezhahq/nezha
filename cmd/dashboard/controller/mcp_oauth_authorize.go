package controller

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

type mcpOAuthAuthorization struct {
	ClientID    string   `json:"client_id"`
	RedirectURI string   `json:"redirect_uri"`
	Resource    string   `json:"resource"`
	Challenge   string   `json:"code_challenge"`
	State       string   `json:"state"`
	Scopes      []string `json:"scopes"`
}

type mcpOAuthConsentTicket struct {
	Request     mcpOAuthAuthorization `json:"request"`
	UserID      uint64                `json:"user_id"`
	UserVersion uint64                `json:"user_version"`
	ExpiresAt   int64                 `json:"expires_at"`
}

type mcpOAuthAuthorizationError struct{ code, description string }

func (err *mcpOAuthAuthorizationError) Error() string { return err.description }

func parseMCPOAuthAuthorization(values url.Values) (mcpOAuthAuthorization, *model.MCPOAuthClient, error) {
	var result mcpOAuthAuthorization
	issuer, err := mcpOAuthIssuer()
	if err != nil {
		return result, nil, err
	}
	for _, name := range []string{"client_id", "redirect_uri", "resource", "code_challenge", "code_challenge_method", "response_type", "scope", "state"} {
		if len(values[name]) > 1 {
			return result, nil, errors.New("duplicate authorization parameter")
		}
	}
	if values.Get("response_type") != "code" {
		return result, nil, &mcpOAuthAuthorizationError{"unsupported_response_type", "only authorization code responses are supported"}
	}
	if values.Get("code_challenge_method") != "S256" {
		return result, nil, errors.New("authorization code with S256 PKCE is required")
	}
	challenge, err := base64.RawURLEncoding.DecodeString(values.Get("code_challenge"))
	if err != nil || len(challenge) != 32 || len(values.Get("code_challenge")) != 43 {
		return result, nil, errors.New("invalid S256 challenge")
	}
	if values.Get("resource") != issuer+"/mcp" {
		return result, nil, &mcpOAuthAuthorizationError{"invalid_target", "resource must match this dashboard's canonical MCP URL"}
	}
	if len(values.Get("client_id")) > 64 || len(values.Get("state")) > 2048 || len(values.Get("scope")) > 512 {
		return result, nil, errors.New("authorization parameter exceeds size limit")
	}
	var client model.MCPOAuthClient
	if err := singleton.DB.First(&client, "id = ?", values.Get("client_id")).Error; err != nil {
		return result, nil, errors.New("unknown OAuth client")
	}
	if client.Disabled {
		return result, nil, &mcpOAuthAuthorizationError{"unauthorized_client", "OAuth client is disabled"}
	}
	var redirects []string
	if json.Unmarshal([]byte(client.RedirectURIs), &redirects) != nil || !slices.Contains(redirects, values.Get("redirect_uri")) {
		return result, nil, errors.New("redirect URI does not exactly match a registered URI")
	}
	scopes := strings.Fields(values.Get("scope"))
	if len(scopes) == 0 {
		scopes = []string{model.ScopeInventoryRead, model.ScopeServerRead}
	}
	for _, scope := range scopes {
		if !slices.Contains(mcpOAuthScopes, scope) {
			return result, nil, &mcpOAuthAuthorizationError{"invalid_scope", "unsupported MCP scope"}
		}
	}
	slices.Sort(scopes)
	result = mcpOAuthAuthorization{ClientID: client.ID, RedirectURI: values.Get("redirect_uri"), Resource: issuer + "/mcp", Challenge: values.Get("code_challenge"), State: values.Get("state"), Scopes: slices.Compact(scopes)}
	return result, &client, nil
}

func (request mcpOAuthAuthorization) query() url.Values {
	return url.Values{"client_id": {request.ClientID}, "redirect_uri": {request.RedirectURI}, "resource": {request.Resource}, "code_challenge": {request.Challenge}, "code_challenge_method": {"S256"}, "response_type": {"code"}, "state": {request.State}, "scope": {strings.Join(request.Scopes, " ")}}
}

func mcpOAuthAuthorizeRedirect(c *gin.Context) {
	if len(c.Request.URL.RawQuery) > 8192 {
		mcpOAuthError(c, 400, "invalid_request", "authorization request too large")
		return
	}
	request, _, err := parseMCPOAuthAuthorization(c.Request.URL.Query())
	if err != nil {
		// Once the callback is independently verified, OAuth errors belong to
		// the client along with state and iss; never redirect to untrusted URIs.
		if callback := mcpOAuthTrustedErrorCallback(c.Request.URL.Query(), err); callback != "" {
			c.Redirect(http.StatusFound, callback)
		} else {
			mcpOAuthError(c, 400, "invalid_request", err.Error())
		}
		return
	}
	issuer, _ := mcpOAuthIssuer()
	// The independent admin frontend owns all login and consent UI. This
	// protocol endpoint never renders HTML and never grants access on GET.
	c.Redirect(http.StatusFound, issuer+mcpOAuthFrontendPath+"?"+request.query().Encode())
}

func mcpOAuthTrustedErrorCallback(values url.Values, cause error) string {
	if len(values["client_id"]) != 1 || len(values["redirect_uri"]) != 1 || len(values["state"]) > 1 || len(values.Get("state")) > 2048 || len(values.Get("client_id")) > 64 {
		return ""
	}
	var client model.MCPOAuthClient
	if singleton.DB.First(&client, "id = ?", values.Get("client_id")).Error != nil {
		return ""
	}
	var redirects []string
	raw := values.Get("redirect_uri")
	if json.Unmarshal([]byte(client.RedirectURIs), &redirects) != nil || !slices.Contains(redirects, raw) || !mcpOAuthRedirectURI(raw) {
		return ""
	}
	issuer, err := mcpOAuthIssuer()
	if err != nil {
		return ""
	}
	callback, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	code := "invalid_request"
	var protocolError *mcpOAuthAuthorizationError
	if errors.As(cause, &protocolError) {
		code = protocolError.code
	}
	query := callback.Query()
	query.Set("error", code)
	query.Set("error_description", cause.Error())
	query.Set("iss", issuer)
	if _, exists := values["state"]; exists {
		query.Set("state", values.Get("state"))
	}
	callback.RawQuery = query.Encode()
	return callback.String()
}

func mcpOAuthConsentInfo(c *gin.Context) (any, error) {
	if len(c.Request.URL.RawQuery) > 8192 {
		return nil, errors.New("authorization request too large")
	}
	request, client, err := parseMCPOAuthAuthorization(c.Request.URL.Query())
	if err != nil {
		return nil, err
	}
	user, ok := c.MustGet(model.CtxKeyAuthorizedUser).(*model.User)
	if !ok || user == nil {
		return nil, errors.New("login required")
	}
	ticket := mcpOAuthConsentTicket{Request: request, UserID: user.ID, UserVersion: user.TokenVersion, ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}
	encoded, err := json.Marshal(ticket)
	if err != nil {
		return nil, err
	}
	payload := base64.RawURLEncoding.EncodeToString(encoded)
	type serverOption struct {
		ID   uint64 `json:"id"`
		Name string `json:"name"`
	}
	servers := make([]serverOption, 0)
	if singleton.ServerShared != nil {
		singleton.ServerShared.Range(func(id uint64, server *model.Server) bool {
			if server.HasPermission(c) {
				servers = append(servers, serverOption{ID: id, Name: server.Name})
			}
			return true
		})
	}
	slices.SortFunc(servers, func(a, b serverOption) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return gin.H{"client_name": client.Name, "redirect_uri": request.RedirectURI, "resource": request.Resource, "scopes": request.Scopes, "servers": servers, "consent_token": payload + "." + mcpOAuthSign("mcp-consent-v1", payload)}, nil
}

func mcpOAuthReadConsentTicket(token string, user *model.User) (*mcpOAuthConsentTicket, error) {
	parts := strings.Split(token, ".")
	if len(token) > 16384 || len(parts) != 2 || !mcpOAuthHashEqual(parts[1], mcpOAuthSign("mcp-consent-v1", parts[0])) {
		return nil, errors.New("invalid consent ticket")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("invalid consent ticket")
	}
	var ticket mcpOAuthConsentTicket
	if json.Unmarshal(raw, &ticket) != nil || ticket.UserID != user.ID || ticket.UserVersion != user.TokenVersion || ticket.ExpiresAt <= time.Now().Unix() {
		return nil, errors.New("consent expired or login changed; reload the authorization page")
	}
	// Revalidate registration, canonical audience and policy after the user
	// approves; a signed ticket must not bypass a changed server configuration.
	if _, _, err := parseMCPOAuthAuthorization(ticket.Request.query()); err != nil {
		return nil, err
	}
	return &ticket, nil
}

func mcpOAuthConsent(c *gin.Context) (any, error) {
	var request struct {
		Ticket     string   `json:"consent_token"`
		Approve    bool     `json:"approve"`
		Scopes     []string `json:"scopes"`
		ServerIDs  []uint64 `json:"server_ids"`
		AllServers bool     `json:"all_servers"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		return nil, err
	}
	user := c.MustGet(model.CtxKeyAuthorizedUser).(*model.User)
	ticket, err := mcpOAuthReadConsentTicket(request.Ticket, user)
	if err != nil {
		return nil, err
	}
	redirect, err := url.Parse(ticket.Request.RedirectURI)
	if err != nil {
		return nil, err
	}
	query := redirect.Query()
	issuer, _ := mcpOAuthIssuer()
	query.Set("iss", issuer)
	if ticket.Request.State != "" {
		query.Set("state", ticket.Request.State)
	}
	if !request.Approve {
		query.Set("error", "access_denied")
		redirect.RawQuery = query.Encode()
		return gin.H{"redirect_uri": redirect.String()}, nil
	}
	if len(request.Scopes) == 0 || len(request.Scopes) > len(mcpOAuthScopes) {
		return nil, errors.New("select at least one requested permission")
	}
	for _, scope := range request.Scopes {
		if !slices.Contains(ticket.Request.Scopes, scope) {
			return nil, errors.New("consent cannot expand the requested permissions")
		}
	}
	if len(request.ServerIDs) > 1000 || !request.AllServers && len(request.ServerIDs) == 0 || request.AllServers && len(request.ServerIDs) != 0 {
		return nil, errors.New("select servers or explicitly allow all accessible servers")
	}
	for _, id := range request.ServerIDs {
		if singleton.ServerShared == nil {
			return nil, errors.New("server unavailable")
		}
		server, found := singleton.ServerShared.Get(id)
		if id == 0 || !found || server == nil || !server.HasPermission(c) {
			return nil, errors.New("permission denied for server " + strconv.FormatUint(id, 10))
		}
	}
	slices.Sort(request.Scopes)
	slices.Sort(request.ServerIDs)
	permission := model.APIToken{}
	permission.SetScopes(slices.Compact(request.Scopes))
	permission.SetServerIDs(slices.Compact(request.ServerIDs))
	code := mcpOAuthRandom()
	record := model.MCPOAuthCode{Hash: model.HashAPIToken(code), ClientID: ticket.Request.ClientID, RedirectURI: ticket.Request.RedirectURI, Resource: ticket.Request.Resource, Challenge: ticket.Request.Challenge, UserID: user.ID, UserVersion: user.TokenVersion, ScopesCSV: permission.ScopesCSV, ServersCSV: permission.ServersCSV, ExpiresAt: time.Now().Add(mcpOAuthCodeLifetime)}
	err = mcpOAuthTransaction(func(tx *gorm.DB) error {
		// Consent may have been opened before an administrator disabled the client.
		if err := mcpOAuthRequireEnabledClient(tx, record.ClientID); err != nil {
			return err
		}
		if err := tx.Where("expires_at < ?", time.Now()).Delete(&model.MCPOAuthCode{}).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&model.MCPOAuthCode{}).Where("user_id = ?", user.ID).Count(&count).Error; err != nil {
			return err
		}
		if count >= 30 {
			return errors.New("too many pending authorization codes; try again in five minutes")
		}
		return tx.Create(&record).Error
	})
	if err != nil {
		return nil, err
	}
	query.Set("code", code)
	redirect.RawQuery = query.Encode()
	return gin.H{"redirect_uri": redirect.String()}, nil
}
