package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The login cookie carries the session: scripts must not read it (no
// frontend does; they ask /api/v1/profile), and it must only travel over
// HTTPS when the dashboard is reached over HTTPS, including behind a TLS
// terminating proxy (X-Forwarded-Proto). Plain HTTP deployments still work.
func TestSessionCookieIsHttpOnlyAndSecureOverHTTPS(t *testing.T) {
	env := newBackupTestEnv(t)
	login := func(forwardedProto string) *http.Cookie {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/v1/login",
			strings.NewReader(`{"username":"root","password":"`+mockBackupLoginPassword+`"}`))
		req.Header.Set("Content-Type", "application/json")
		if forwardedProto != "" {
			req.Header.Set("X-Forwarded-Proto", forwardedProto)
		}
		rec := httptest.NewRecorder()
		env.router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		for _, c := range rec.Result().Cookies() {
			if c.Name == "nz-jwt" {
				return c
			}
		}
		t.Fatalf("login set no nz-jwt cookie: %v", rec.Result().Header["Set-Cookie"])
		return nil
	}

	plain := login("")
	require.True(t, plain.HttpOnly, "nz-jwt must be HttpOnly")
	require.False(t, plain.Secure, "plain HTTP deployments must keep working")
	require.Equal(t, http.SameSiteLaxMode, plain.SameSite)
	require.Equal(t, "/", plain.Path)
	require.Positive(t, plain.MaxAge)

	proxied := login("https")
	require.True(t, proxied.HttpOnly)
	require.True(t, proxied.Secure, "behind a TLS terminating proxy the cookie must be Secure")
}
