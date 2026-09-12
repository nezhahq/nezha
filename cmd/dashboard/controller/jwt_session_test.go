package controller

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/idcodec"
	"github.com/nezhahq/nezha/service/singleton"
)

const jwtSessionTestMasterKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func setupJWTSessionTest(t *testing.T) (cleanup func()) {
	t.Helper()
	require.NoError(t, idcodec.Init([]byte(jwtSessionTestMasterKey)))

	originalDB := singleton.DB
	originalConf := singleton.Conf
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.JWTSession{}, &model.WAF{}))
	singleton.DB = db
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{JWTTimeout: 1}}

	require.NoError(t, db.Create(&model.User{
		Common:       model.Common{ID: 100},
		Username:     "victim",
		Role:         model.RoleMember,
		TokenVersion: 7,
	}).Error)

	return func() {
		_ = sqlDB.Close()
		singleton.DB = originalDB
		singleton.Conf = originalConf
	}
}

func newCtxForUser(userID uint64, ip, ua string) *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	ctx.Request.Header.Set("User-Agent", ua)
	ctx.Set(model.CtxKeyRealIPStr, ip)
	if userID != 0 {
		ctx.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: userID}})
	}
	return ctx
}

func TestIssueJWTSessionWritesRow(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	ctx := newCtxForUser(0, "1.2.3.4", "test-ua")
	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	claims, err := issueJWTSession(ctx, &user, 1)
	require.NoError(t, err)

	hashUID, _ := claims[jwtClaimUserID].(string)
	keyID, _ := claims[jwtClaimKeyID].(string)
	assert.NotEqual(t, "100", hashUID, "uid claim must be obfuscated, not raw integer")
	got, err := idcodec.Decode(hashUID)
	require.NoError(t, err)
	assert.Equal(t, uint64(100), got)

	var sess model.JWTSession
	require.NoError(t, singleton.DB.First(&sess, "key_id = ?", keyID).Error)
	assert.Equal(t, uint64(100), sess.UserID)
	assert.Equal(t, "1.2.3.4", sess.IP)
	assert.Equal(t, uint64(7), sess.TokenVersion)
	assert.True(t, sess.ExpiresAt.After(time.Now()))
}

func TestIdentityHandlerHappyPath(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	claims, err := issueJWTSession(ctx, &user, 1)
	require.NoError(t, err)

	verify := newCtxForUser(0, "1.2.3.4", "ua")
	verify.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: claims[jwtClaimUserID],
		jwtClaimKeyID:  claims[jwtClaimKeyID],
	})

	identity := identityHandler()(verify)
	require.NotNil(t, identity, "happy path must return user identity")
	u := identity.(*model.User)
	assert.Equal(t, uint64(100), u.ID)
}

func TestIdentityHandlerRejectsMismatchedClaimUID(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	claims, err := issueJWTSession(ctx, &user, 1)
	require.NoError(t, err)

	forgedUID, err := idcodec.Encode(999)
	require.NoError(t, err)

	verify := newCtxForUser(0, "1.2.3.4", "ua")
	verify.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: forgedUID,
		jwtClaimKeyID:  claims[jwtClaimKeyID],
	})

	identity := identityHandler()(verify)
	assert.Nil(t, identity, "claim uid not matching session.user_id must reject")
}

func TestIdentityHandlerRejectsRevokedSession(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	claims, err := issueJWTSession(ctx, &user, 1)
	require.NoError(t, err)
	keyID := claims[jwtClaimKeyID].(string)
	require.NoError(t, singleton.RevokeJWTSession(keyID))

	verify := newCtxForUser(0, "1.2.3.4", "ua")
	verify.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: claims[jwtClaimUserID],
		jwtClaimKeyID:  claims[jwtClaimKeyID],
	})

	identity := identityHandler()(verify)
	assert.Nil(t, identity, "revoked session must reject")
}

func TestIdentityHandlerRejectsTokenVersionBump(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	claims, err := issueJWTSession(ctx, &user, 1)
	require.NoError(t, err)

	require.NoError(t, singleton.DB.Model(&model.User{}).
		Where("id = ?", 100).
		Update("token_version", 8).Error)

	verify := newCtxForUser(0, "1.2.3.4", "ua")
	verify.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: claims[jwtClaimUserID],
		jwtClaimKeyID:  claims[jwtClaimKeyID],
	})

	identity := identityHandler()(verify)
	assert.Nil(t, identity, "session whose TokenVersion is stale must reject")
}

func TestIdentityHandlerFlagsIPMismatch(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	claims, err := issueJWTSession(ctx, &user, 1)
	require.NoError(t, err)

	verify := newCtxForUser(0, "9.9.9.9", "ua")
	verify.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: claims[jwtClaimUserID],
		jwtClaimKeyID:  claims[jwtClaimKeyID],
	})

	identity := identityHandler()(verify)
	assert.Nil(t, identity, "IP mismatch must reject")
	assert.True(t, verify.GetBool(model.CtxKeyIsIPMismatch))
}

func TestIdentityHandlerAllowsIPMismatchWhenConfigured(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()
	singleton.Conf.SetJWTIPChangeAllowed(true)

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	claims, err := issueJWTSession(ctx, &user, 1)
	require.NoError(t, err)

	verify := newCtxForUser(0, "9.9.9.9", "ua")
	verify.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: claims[jwtClaimUserID],
		jwtClaimKeyID:  claims[jwtClaimKeyID],
	})

	identity := identityHandler()(verify)
	require.NotNil(t, identity, "IP mismatch must be accepted when allow_jwt_ip_change is enabled")
	assert.False(t, verify.GetBool(model.CtxKeyIsIPMismatch))
}

func TestIdentityHandlerConcurrentIPChangeSetting(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	claims, err := issueJWTSession(ctx, &user, 1)
	require.NoError(t, err)

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				singleton.Conf.SetJWTIPChangeAllowed(true)
				singleton.Conf.SetJWTIPChangeAllowed(false)
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	for i := 0; i < 100; i++ {
		verify := newCtxForUser(0, "1.2.3.4", "ua")
		verify.Set("JWT_PAYLOAD", jwt.MapClaims(claims))
		require.NotNil(t, identityHandler()(verify), "matching IP must pass during setting updates")
	}
}

func TestIdentityHandlerRejectsUnknownKeyID(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	hashUID, err := idcodec.Encode(100)
	require.NoError(t, err)

	verify := newCtxForUser(0, "1.2.3.4", "ua")
	verify.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: hashUID,
		jwtClaimKeyID:  "this-key-id-was-never-issued",
	})

	identity := identityHandler()(verify)
	assert.Nil(t, identity, "key id absent from sessions table must reject (no oracle to confirm secret)")
}

func TestAuthenticatorPersistsCurrentTokenVersion(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	pw, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, singleton.DB.Model(&model.User{}).
		Where("id = ?", 100).
		Update("password", string(pw)).Error)

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	body, _ := json.Marshal(model.LoginRequest{Username: "victim", Password: "correct horse"})
	ctx.Request = httptest.NewRequest("POST", "/api/v1/login", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("User-Agent", "ua")
	ctx.Set(model.CtxKeyRealIPStr, "1.2.3.4")

	data, err := authenticator()(ctx)
	require.NoError(t, err)
	claims, ok := data.(map[string]interface{})
	require.True(t, ok, "authenticator must return claims map")
	keyID, _ := claims[jwtClaimKeyID].(string)
	require.NotEmpty(t, keyID)

	var sess model.JWTSession
	require.NoError(t, singleton.DB.First(&sess, "key_id = ?", keyID).Error)
	assert.Equal(t, uint64(7), sess.TokenVersion,
		"session must record the user's current token_version, otherwise identityHandler will reject the freshly-issued token")

	verify := newCtxForUser(0, "1.2.3.4", "ua")
	verify.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: claims[jwtClaimUserID],
		jwtClaimKeyID:  claims[jwtClaimKeyID],
	})
	assert.NotNil(t, identityHandler()(verify),
		"the very next request with the freshly-issued token must authenticate")
}

func TestAuthenticator_BadPasswordReturnsFailedAuth(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	pw, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, singleton.DB.Model(&model.User{}).
		Where("id = ?", 100).
		Update("password", string(pw)).Error)

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	body, _ := json.Marshal(model.LoginRequest{Username: "victim", Password: "wrong"})
	ctx.Request = httptest.NewRequest("POST", "/api/v1/login", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("User-Agent", "ua")
	ctx.Set(model.CtxKeyRealIPStr, "1.2.3.4")

	_, err = authenticator()(ctx)
	require.Error(t, err, "wrong password must fail authentication")
	require.Equal(t, jwt.ErrFailedAuthentication, err)

	var w model.WAF
	require.NoError(t, singleton.DB.Where("block_identifier = ?", int64(100)).First(&w).Error,
		"bad password must increment WAF counter under user-specific BlockID")
	require.GreaterOrEqual(t, w.Count, uint64(1))
}

func TestAuthenticator_UnknownUserReturnsFailedAuth(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	body, _ := json.Marshal(model.LoginRequest{Username: "ghost", Password: "anything"})
	ctx.Request = httptest.NewRequest("POST", "/api/v1/login", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(model.CtxKeyRealIPStr, "1.2.3.4")

	_, err := authenticator()(ctx)
	require.Error(t, err)
	require.Equal(t, jwt.ErrFailedAuthentication, err)

	var w model.WAF
	require.NoError(t, singleton.DB.Where("block_identifier = ?", int64(model.BlockIDUnknownUser)).First(&w).Error,
		"unknown user must increment WAF counter under BlockIDUnknownUser")
}

func TestAuthenticator_RejectPasswordUserRefused(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	pw, err := bcrypt.GenerateFromPassword([]byte("ok"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, singleton.DB.Model(&model.User{}).
		Where("id = ?", 100).
		Updates(map[string]any{"password": string(pw), "reject_password": true}).Error)

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	body, _ := json.Marshal(model.LoginRequest{Username: "victim", Password: "ok"})
	ctx.Request = httptest.NewRequest("POST", "/api/v1/login", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(model.CtxKeyRealIPStr, "1.2.3.4")

	_, err = authenticator()(ctx)
	require.Equal(t, jwt.ErrFailedAuthentication, err,
		"users with reject_password=true must not be able to log in via password even with correct one")
}

func TestIdentityHandler_ExpiredSessionRejected(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	claims, err := issueJWTSession(ctx, &user, 1)
	require.NoError(t, err)
	keyID := claims[jwtClaimKeyID].(string)

	require.NoError(t, singleton.DB.Model(&model.JWTSession{}).
		Where("key_id = ?", keyID).
		Update("expires_at", time.Now().Add(-time.Hour)).Error)

	verify := newCtxForUser(0, "1.2.3.4", "ua")
	verify.Set("JWT_PAYLOAD", jwt.MapClaims{
		jwtClaimUserID: claims[jwtClaimUserID],
		jwtClaimKeyID:  claims[jwtClaimKeyID],
	})

	identity := identityHandler()(verify)
	require.Nil(t, identity, "session whose expires_at is in the past must reject")
}

func TestRefreshResponse_UpdatesSessionExpires(t *testing.T) {
	cleanup := setupJWTSessionTest(t)
	defer cleanup()

	ctx := newCtxForUser(0, "1.2.3.4", "ua")
	user := model.User{Common: model.Common{ID: 100}, TokenVersion: 7}
	claims, err := issueJWTSession(ctx, &user, 1)
	require.NoError(t, err)
	keyID := claims[jwtClaimKeyID].(string)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/refresh-token", nil)
	c.Set(jwtClaimKeyID, keyID)

	newExpire := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	refreshResponse(c, 200, "fake-token", newExpire)

	var sess model.JWTSession
	require.NoError(t, singleton.DB.First(&sess, "key_id = ?", keyID).Error)
	require.WithinDuration(t, newExpire, sess.ExpiresAt, time.Second,
		"refreshResponse must extend the session's expires_at to the new expiry")
	require.WithinDuration(t, time.Now(), sess.LastUsedAt, 5*time.Second,
		"refreshResponse must touch last_used_at")
}
