package controller

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/service/singleton"
)

// sessionCookieName is the login cookie (gin-jwt reads it via TokenLookup).
const sessionCookieName = "nz-jwt"

// requestIsHTTPS reports whether the browser reached the dashboard over
// HTTPS: directly (the dashboard terminates TLS) or through a TLS
// terminating proxy that sets X-Forwarded-Proto (Caddy does by default,
// nginx needs proxy_set_header X-Forwarded-Proto $scheme). The header is
// client controlled, but it is only used to add Secure to the client's own
// cookies, which a forged value can only break for that client.
func requestIsHTTPS(c *gin.Context) bool {
	return c.Request.TLS != nil || c.Request.URL.Scheme == "https" ||
		c.Request.Header.Get("X-Forwarded-Proto") == "https"
}

// setSessionCookie writes the login cookie. gin-jwt can only use a fixed
// Secure flag, which would break plain-HTTP deployments or leave HTTPS ones
// without it, so its own cookie writing is off (SendCookie: false) and
// login, refresh and OAuth2 callbacks write the cookie here. HttpOnly: no
// frontend reads it; they ask /api/v1/profile for the login state.
func setSessionCookie(c *gin.Context, token string) {
	c.SetSameSite(http.SameSiteLaxMode)
	maxAge := int((time.Hour * time.Duration(singleton.Conf.JWTTimeout)).Seconds())
	c.SetCookie(sessionCookieName, token, maxAge, "/", "", requestIsHTTPS(c), true)
}

// clearSessionCookie deletes the login cookie with the attributes it was
// written with.
func clearSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieName, "", -1, "/", "", requestIsHTTPS(c), true)
}
