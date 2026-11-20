package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func newLogoutContext(claims jwt.MapClaims) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil)
	ctx.Set(model.CtxKeyRealIPStr, "192.0.2.10")
	ctx.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: claims[jwtClaimUserID],
		jwtClaimKeyID:  claims[jwtClaimKeyID],
	})
	return ctx, recorder
}

func sessionIsAccepted(claims jwt.MapClaims, clientIP string) bool {
	verify := newCtxForUser(0, clientIP, "ua")
	verify.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: claims[jwtClaimUserID],
		jwtClaimKeyID:  claims[jwtClaimKeyID],
	})
	return identityHandler()(verify) != nil
}

// Clearing the cookie in the browser does not end a session: a token copied
// out beforehand stays valid until it expires. Logging out has to revoke the
// session on the server, and only the one that asked for it.
func TestLogoutRevokesOnlyTheCurrentSession(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	loggingOut, err := issueJWTSession(newCtxForUser(0, "192.0.2.10", "ua"), &user, 1)
	require.NoError(t, err)
	otherDevice, err := issueJWTSession(newCtxForUser(0, "192.0.2.20", "ua"), &user, 1)
	require.NoError(t, err)
	require.True(t, sessionIsAccepted(loggingOut, "192.0.2.10"), "test setup: session must be valid before logout")

	ctx, recorder := newLogoutContext(loggingOut)
	_, err = logout(ctx)
	require.NoError(t, err)

	assert.False(t, sessionIsAccepted(loggingOut, "192.0.2.10"), "the session that logged out must be rejected afterwards")
	assert.True(t, sessionIsAccepted(otherDevice, "192.0.2.20"), "another device's session must stay signed in")

	var revoked model.JWTSession
	require.NoError(t, singleton.DB.First(&revoked, "key_id = ?", loggingOut[jwtClaimKeyID]).Error)
	assert.NotNil(t, revoked.RevokedAt, "revoked_at must be recorded")

	var clearedSessionCookie *http.Cookie
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == "nz-jwt" {
			clearedSessionCookie = cookie
		}
	}
	require.NotNil(t, clearedSessionCookie, "logout must expire the nz-jwt cookie")
	assert.Empty(t, clearedSessionCookie.Value)
	assert.Less(t, clearedSessionCookie.MaxAge, 0, "nz-jwt must be expired, not re-issued")
	assert.Equal(t, "/", clearedSessionCookie.Path)
}

func TestLogoutWithoutSessionClaimIsHarmless(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	existing, err := issueJWTSession(newCtxForUser(0, "192.0.2.10", "ua"), &user, 1)
	require.NoError(t, err)

	ctx, _ := newLogoutContext(jwt.MapClaims{})
	_, err = logout(ctx)
	require.NoError(t, err)

	assert.True(t, sessionIsAccepted(existing, "192.0.2.10"), "a request without a session id must not revoke anything")
}
