package controller

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

const (
	mcpOAuthPath            = "/oauth2/mcp"
	mcpOAuthFrontendPath    = "/dashboard/mcp/authorize"
	mcpOAuthAccessPrefix    = "nzo_"
	mcpOAuthRefreshPrefix   = "nzr_"
	mcpOAuthCodeLifetime    = 5 * time.Minute
	mcpOAuthGrantLifetime   = 30 * 24 * time.Hour
	mcpOAuthAccessLifetime  = time.Hour
	mcpOAuthGrantContextKey = "mcp_oauth_grant"
)

var mcpOAuthScopes = []string{model.ScopeInventoryRead, model.ScopeServerRead, model.ScopeServerWrite, model.ScopeServerDelete, model.ScopeServerExec}
var mcpOAuthPublicLimiter = newMCPRateLimiter(20, 300)
var mcpOAuthRegistrationLimiter = newMCPRateLimiter(2, 20)
var mcpOAuthMutationMu sync.Mutex

func mcpOAuthTransaction(fn func(*gorm.DB) error) error {
	// SQLite deferred transactions can fail on a read-to-write lock upgrade.
	// Serialize OAuth mutations so a concurrent code/refresh replay reaches
	// the revocation path instead of being mistaken for an invalid grant.
	mcpOAuthMutationMu.Lock()
	defer mcpOAuthMutationMu.Unlock()
	return singleton.DB.Transaction(fn)
}

// Pin OAuth discovery and token audiences to operator configuration. Never
// derive an issuer from Host, Referer or forwarded headers supplied by callers.
func mcpOAuthIssuer() (string, error) {
	if singleton.Conf == nil || !singleton.Conf.MCPEnabled() {
		return "", errors.New("MCP is disabled")
	}
	host := singleton.Conf.DashboardHost
	u, err := url.Parse("https://" + host)
	if err != nil || host == "" || u.Host != host || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(host, "\\\r\n\t ") || singleton.Conf.JWTSecretKey == "" {
		return "", errors.New("configure dashboard_host as the public HTTPS host before using MCP OAuth")
	}
	return u.String(), nil
}

func mcpOAuthError(c *gin.Context, status int, code, description string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.AbortWithStatusJSON(status, gin.H{"error": code, "error_description": description})
}

func mcpOAuthAvailable(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	if _, err := mcpOAuthIssuer(); err != nil {
		mcpOAuthError(c, http.StatusServiceUnavailable, "temporarily_unavailable", err.Error())
		return
	}
	// A single bounded global bucket also bounds anonymous registration and
	// parsing work without allocating one map entry per attacker-controlled IP.
	if !mcpOAuthPublicLimiter.Allow(1) {
		c.Header("Retry-After", "60")
		mcpOAuthError(c, http.StatusTooManyRequests, "temporarily_unavailable", "OAuth request rate exceeded")
		return
	}
	if c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32*1024)
	}
	c.Next()
}

func registerMCPOAuthRoutes(r *gin.Engine) {
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		r.GET(path, mcpOAuthAvailable, mcpOAuthResourceMetadata)
	}
	r.GET("/.well-known/oauth-authorization-server", mcpOAuthAvailable, mcpOAuthServerMetadata)
	group := r.Group(mcpOAuthPath, mcpOAuthAvailable)
	group.POST("/register", mcpOAuthRegister)
	group.GET("/authorize", mcpOAuthAuthorizeRedirect)
	group.POST("/token", mcpOAuthToken)
	group.POST("/revoke", mcpOAuthRevoke)
}

func mcpOAuthResourceMetadata(c *gin.Context) {
	issuer, _ := mcpOAuthIssuer()
	c.JSON(http.StatusOK, gin.H{"resource": issuer + "/mcp", "authorization_servers": []string{issuer}, "scopes_supported": mcpOAuthScopes, "bearer_methods_supported": []string{"header"}})
}

func mcpOAuthServerMetadata(c *gin.Context) {
	issuer, _ := mcpOAuthIssuer()
	c.JSON(http.StatusOK, gin.H{
		"issuer": issuer, "authorization_endpoint": issuer + mcpOAuthPath + "/authorize",
		"token_endpoint": issuer + mcpOAuthPath + "/token", "registration_endpoint": issuer + mcpOAuthPath + "/register",
		"revocation_endpoint": issuer + mcpOAuthPath + "/revoke", "response_types_supported": []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"code_challenge_methods_supported":      []string{"S256"}, "scopes_supported": mcpOAuthScopes,
		"authorization_response_iss_parameter_supported": true,
	})
}

func mcpOAuthRandom() string {
	// crypto/rand.Text fails closed on entropy failure and provides at least 128 bits.
	return rand.Text()
}

func mcpOAuthSign(purpose, value string) string {
	mac := hmac.New(sha256.New, []byte(singleton.Conf.JWTSecretKey))
	mac.Write([]byte(purpose + "\x00" + value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func mcpOAuthHashEqual(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

func mcpOAuthRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	// Callback result parameters belong to the authorization server. Allowing
	// prefilled code/state/error values would make denial responses ambiguous.
	for _, key := range []string{"code", "state", "iss", "error", "error_description"} {
		if _, exists := u.Query()[key]; exists {
			return false
		}
	}
	return len(raw) <= 2048 && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && !strings.ContainsAny(raw, "\\\r\n\t ")
}

func mcpOAuthRegister(c *gin.Context) {
	if !mcpOAuthRegistrationLimiter.Allow(1) {
		c.Header("Retry-After", "60")
		mcpOAuthError(c, http.StatusTooManyRequests, "temporarily_unavailable", "client registration rate exceeded")
		return
	}
	var req struct {
		Name          string   `json:"client_name"`
		RedirectURIs  []string `json:"redirect_uris"`
		AuthMethod    string   `json:"token_endpoint_auth_method"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Name) > 120 || len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 8 {
		mcpOAuthError(c, 400, "invalid_client_metadata", "provide up to eight HTTPS redirect URIs and a client name of at most 120 bytes")
		return
	}
	for _, uri := range req.RedirectURIs {
		if !mcpOAuthRedirectURI(uri) {
			mcpOAuthError(c, 400, "invalid_redirect_uri", "redirect URIs must be absolute HTTPS URLs without fragments or credentials")
			return
		}
	}
	if req.AuthMethod == "" {
		req.AuthMethod = "client_secret_basic"
	}
	if !slices.Contains([]string{"none", "client_secret_basic", "client_secret_post"}, req.AuthMethod) {
		mcpOAuthError(c, 400, "invalid_client_metadata", "unsupported token endpoint authentication method")
		return
	}
	for _, grant := range req.GrantTypes {
		if grant != "authorization_code" && grant != "refresh_token" {
			mcpOAuthError(c, 400, "invalid_client_metadata", "unsupported grant type")
			return
		}
	}
	for _, response := range req.ResponseTypes {
		if response != "code" {
			mcpOAuthError(c, 400, "invalid_client_metadata", "only the code response type is supported")
			return
		}
	}
	redirects, _ := json.Marshal(req.RedirectURIs)
	client := model.MCPOAuthClient{ID: mcpOAuthRandom(), Name: strings.TrimSpace(req.Name), RedirectURIs: string(redirects), AuthMethod: req.AuthMethod, LastUsedAt: time.Now()}
	if client.Name == "" {
		client.Name = "MCP client"
	}
	secret := ""
	if client.AuthMethod != "none" {
		secret = mcpOAuthRandom()
		client.SecretHash = model.HashAPIToken(secret)
	}
	// Prune registrations that have not been used for 90 days, and cap the
	// durable anonymous-registration surface even under sustained abuse.
	err := mcpOAuthTransaction(func(tx *gorm.DB) error {
		if err := tx.Where("disabled = ? AND last_used_at < ?", false, time.Now().Add(-90*24*time.Hour)).Delete(&model.MCPOAuthClient{}).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&model.MCPOAuthClient{}).Count(&count).Error; err != nil {
			return err
		}
		if count >= 4096 {
			return errors.New("client capacity reached")
		}
		return tx.Create(&client).Error
	})
	if err != nil {
		mcpOAuthError(c, 503, "temporarily_unavailable", "client registration unavailable")
		return
	}
	response := gin.H{"client_id": client.ID, "client_name": client.Name, "redirect_uris": req.RedirectURIs, "token_endpoint_auth_method": client.AuthMethod, "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "client_id_issued_at": time.Now().Unix()}
	if secret != "" {
		response["client_secret"] = secret
		response["client_secret_expires_at"] = 0
	}
	c.JSON(http.StatusCreated, response)
}

func mcpOAuthSetChallenge(c *gin.Context) {
	if issuer, err := mcpOAuthIssuer(); err == nil {
		c.Header("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, issuer))
	}
}

func mcpOAuthPKCE(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func mcpOAuthVerifierValid(verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, char := range verifier {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-._~", char)) {
			return false
		}
	}
	return true
}

// Separate prefix plus a server-authenticated rotation handle lets us detect
// reuse of any previously issued refresh token without retaining unbounded
// token history. An untrusted caller cannot forge a handle to revoke a grant.
func mcpOAuthNewRefresh(grantID string) string {
	payload := grantID + "." + mcpOAuthRandom()
	return mcpOAuthRefreshPrefix + payload + "." + mcpOAuthSign("mcp-refresh-v1", payload)
}

func mcpOAuthRefreshGrantID(token string) string {
	if !strings.HasPrefix(token, mcpOAuthRefreshPrefix) || len(token) > 256 {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(token, mcpOAuthRefreshPrefix), ".")
	if len(parts) != 3 || !mcpOAuthHashEqual(parts[2], mcpOAuthSign("mcp-refresh-v1", parts[0]+"."+parts[1])) {
		return ""
	}
	return parts[0]
}
