package controller

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// issueCSRFToken mints a signed double-submit token (nonce.HMAC-SHA256 keyed
// by the JWT secret). Signing defeats sibling-subdomain cookie tossing: a
// naive double-submit trusts any header==cookie pair, but an injected cookie
// carries no valid HMAC and fails validateCSRFToken. Returns "" pre-init
// (no secret); callers treat that as "no cookie minted".
func issueCSRFToken() string {
	secret := csrfSigningSecret()
	if secret == "" {
		return ""
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	nonce := hex.EncodeToString(b[:])
	return nonce + "." + csrfSign(nonce, secret)
}

func csrfSigningSecret() string {
	if singleton.Conf == nil {
		return ""
	}
	return singleton.Conf.JWTSecretKey
}

func csrfSign(nonce, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

// validateCSRFToken reports whether value is a well-formed nonce.signature
// pair whose signature verifies under the current server secret. Constant
// -time comparison guards against signature-probing side channels.
func validateCSRFToken(value string) bool {
	secret := csrfSigningSecret()
	if secret == "" || value == "" {
		return false
	}
	idx := strings.LastIndex(value, ".")
	if idx <= 0 || idx == len(value)-1 {
		return false
	}
	nonce, sig := value[:idx], value[idx+1:]
	return hmac.Equal([]byte(sig), []byte(csrfSign(nonce, secret)))
}

// setCSRFCookie issues a fresh signed CSRF token cookie. Called by login +
// refresh handlers so the frontend always has a paired value to mirror back
// into the X-CSRF-Token header. The cookie is intentionally HttpOnly=false —
// SPA JS must be able to read it. SameSite=Strict here (not Lax) because
// the cookie's sole purpose is the same-origin double-submit check and we
// don't want it leaking on cross-site GET navigation either.
func setCSRFCookie(c *gin.Context) {
	token := issueCSRFToken()
	if token == "" {
		return
	}
	// Secure is set only when the request arrives over HTTPS, mirroring
	// writeOauth2StateCookie. On plain HTTP (e.g. intranet deployments) a
	// Secure cookie would be dropped by the browser, breaking the
	// double-submit pair, so we must not force it unconditionally.
	secure := c.Request.URL.Scheme == "https" || c.Request.TLS != nil
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(csrfCookieName, token, 0, "/", "", secure, false)
}

const (
	csrfCookieName = "nz-csrf"
	csrfHeaderName = "X-CSRF-Token"
)

// csrfMiddleware enforces a double-submit-cookie CSRF gate on unsafe
// HTTP methods for cookie-authenticated requests.
//
// Why: SameSite=Lax on the nz-jwt cookie blocks the simplest cross-site
// form POST, but it does not stop same-site XSS-pivot CSRF, header method
// override, redirect-leaking auth helpers, or carefully chained sub-domain
// attacks. The double-submit pattern (server sets a JS-readable nz-csrf
// cookie, client mirrors the value into X-CSRF-Token) closes the gap
// without coupling auth state to a server-side session.
//
// Bypass conditions:
//   - Safe methods (GET/HEAD/OPTIONS): no state mutation, no CSRF risk.
//   - Bearer-token PAT requests (`Authorization: Bearer nzp_*`): stateless,
//     no ambient cookie, so a CSRF attack cannot induce them.
//
// Reject conditions:
//   - Missing or empty X-CSRF-Token header.
//   - Missing or empty nz-csrf cookie.
//   - Header value != cookie value.
//   - Cookie value not signed by the server (validateCSRFToken fails).
//
// The middleware DOES NOT set the csrf cookie on its own — that is the
// JWT login / refresh handler's job, since those are the only places that
// know when to mint a fresh value. The pair just has to exist by the time
// any unsafe call reaches here.
func csrfMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// Self-heal sessions that predate the CSRF cookie (or whose
			// nz-csrf expired): seed a fresh value on a safe method so the
			// double-submit pair exists before the next unsafe call. Safe
			// methods mutate nothing, so minting here carries no CSRF risk.
			if cookie, err := c.Cookie(csrfCookieName); err != nil || cookie == "" {
				setCSRFCookie(c)
			}
			c.Next()
			return
		}
		// A PAT request carries no ambient cookie, so CSRF cannot induce it.
		// The exemption must check the authenticated PAT identity resolved by
		// apiTokenAuthMiddleware, not a forgeable Authorization header value.
		if APITokenFromContext(c) != nil {
			c.Next()
			return
		}
		header := c.GetHeader(csrfHeaderName)
		cookie, err := c.Cookie(csrfCookieName)
		// Both halves must be present, mirror each other, AND carry a valid
		// server signature. The signature check is what stops a cookie-tossed
		// pair from a sibling subdomain.
		if err != nil || cookie == "" || header == "" || header != cookie || !validateCSRFToken(cookie) {
			c.AbortWithStatusJSON(http.StatusForbidden, model.CommonResponse[any]{
				Success: false,
				Error:   "ApiErrorForbidden: missing or invalid CSRF token",
			})
			return
		}
		c.Next()
	}
}
