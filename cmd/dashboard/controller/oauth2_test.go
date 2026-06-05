package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/service/singleton"
)

// OAuth2 callback 测试核心安全语义：state CSRF、provider 校验、解绑权限。
//
// 这些测试用 verifyState 的私有路径直接构造场景，因为 callback 的完整链路涉及
// 真实 IdP HTTP 调用；safety-critical 的 state 校验本身可以单测。

func setupOAuth2Test(t *testing.T) func() {
	t.Helper()
	originalDB := singleton.DB
	originalConf := singleton.Conf
	originalCache := singleton.Cache
	originalLocalizer := singleton.Localizer
	if singleton.Localizer == nil {
		singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	}
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Oauth2Bind{}, &model.WAF{}))
	singleton.DB = db
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{
		Oauth2: map[string]*model.Oauth2Config{
			"github": {ClientID: "x", ClientSecret: "y"},
		},
	}}
	singleton.Cache = cache.New(time.Minute, time.Minute)

	return func() {
		singleton.DB = originalDB
		singleton.Conf = originalConf
		singleton.Cache = originalCache
		singleton.Localizer = originalLocalizer
	}
}

func newOAuth2Ctx(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/oauth2/callback", nil)
	c.Set(model.CtxKeyRealIPStr, "1.2.3.4")
	return c, w
}

func TestOAuth2_VerifyState_RejectsMissingCookie(t *testing.T) {
	defer setupOAuth2Test(t)()
	c, _ := newOAuth2Ctx(t)

	_, err := verifyState(c, "any-state-value")
	require.Error(t, err, "missing nz-o2s cookie must be rejected")
}

func TestOAuth2_VerifyState_RejectsUnknownCookie(t *testing.T) {
	defer setupOAuth2Test(t)()
	c, _ := newOAuth2Ctx(t)
	c.Request.AddCookie(&http.Cookie{Name: "nz-o2s", Value: "never-issued-key"})

	_, err := verifyState(c, "any-state")
	require.Error(t, err, "unknown state key (no cache entry) must be rejected")
}

func TestOAuth2_VerifyState_RejectsStateMismatch(t *testing.T) {
	defer setupOAuth2Test(t)()
	c, _ := newOAuth2Ctx(t)

	stateKey := "k-1"
	singleton.Cache.Set(
		fmt.Sprintf("%s%s", model.CacheKeyOauth2State, stateKey),
		&model.Oauth2State{State: "real-state", Provider: "github"},
		cache.DefaultExpiration,
	)
	c.Request.AddCookie(&http.Cookie{Name: "nz-o2s", Value: stateKey})

	_, err := verifyState(c, "forged-state")
	require.Error(t, err, "attacker-supplied state that differs from cached must be rejected (CSRF defense)")
}

func TestOAuth2_VerifyState_HappyPath(t *testing.T) {
	defer setupOAuth2Test(t)()
	c, _ := newOAuth2Ctx(t)

	stateKey := "k-ok"
	singleton.Cache.Set(
		fmt.Sprintf("%s%s", model.CacheKeyOauth2State, stateKey),
		&model.Oauth2State{State: "good-state", Provider: "github", Action: model.RTypeBind},
		cache.DefaultExpiration,
	)
	c.Request.AddCookie(&http.Cookie{Name: "nz-o2s", Value: stateKey})

	st, err := verifyState(c, "good-state")
	require.NoError(t, err)
	require.Equal(t, "github", st.Provider)
	require.Equal(t, model.RTypeBind, st.Action)
}

// GHSA-9rc6-8cjv-rcvx: getRedirectURL must not echo an attacker-controlled
// Host header into the OAuth2 callback URL. It may only trust a Host that the
// operator declared (InstallHost / ListenHost / ReservedHosts); any other Host
// must fall back to the configured InstallHost so a forged value cannot divert
// the victim's authorization code.

func setRedirectHostConf(t *testing.T, installHost, reservedHosts string) {
	t.Helper()
	prev := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{
		ConfigDashboard: model.ConfigDashboard{
			InstallHost:   installHost,
			ReservedHosts: reservedHosts,
		},
	}}
	t.Cleanup(func() { singleton.Conf = prev })
}

func TestGetRedirectURL_RejectsForgedHostFallsBackToInstallHost(t *testing.T) {
	setRedirectHostConf(t, "panel.example.com", "")
	c, _ := newOAuth2Ctx(t)
	c.Request.Host = "evil.attacker.test"

	got := getRedirectURL(c)
	require.Equal(t, "http://panel.example.com/api/v1/oauth2/callback", got,
		"a forged Host must be ignored in favour of the configured InstallHost")
}

func TestGetRedirectURL_TrustsInstallHost(t *testing.T) {
	setRedirectHostConf(t, "panel.example.com", "")
	c, _ := newOAuth2Ctx(t)
	c.Request.Host = "panel.example.com"

	got := getRedirectURL(c)
	require.Equal(t, "http://panel.example.com/api/v1/oauth2/callback", got,
		"the declared InstallHost must be trusted verbatim")
}

func TestGetRedirectURL_TrustsReservedHostForMultiDomain(t *testing.T) {
	setRedirectHostConf(t, "panel.example.com", "alt.example.com,panel2.example.com")
	c, _ := newOAuth2Ctx(t)
	c.Request.Host = "panel2.example.com"

	got := getRedirectURL(c)
	require.Equal(t, "http://panel2.example.com/api/v1/oauth2/callback", got,
		"a Host listed in ReservedHosts must be trusted so multi-domain deployments keep working")
}

func TestGetRedirectURL_HonoursForwardedProtoOnTrustedHost(t *testing.T) {
	setRedirectHostConf(t, "panel.example.com", "")
	c, _ := newOAuth2Ctx(t)
	c.Request.Host = "panel.example.com"
	c.Request.Header.Set("X-Forwarded-Proto", "https")

	got := getRedirectURL(c)
	require.Equal(t, "https://panel.example.com/api/v1/oauth2/callback", got,
		"https scheme must still be derived for reverse-proxy TLS termination")
}

func TestGetRedirectURL_ForgedHostCannotForceHTTPSOrigin(t *testing.T) {
	setRedirectHostConf(t, "panel.example.com", "")
	c, _ := newOAuth2Ctx(t)
	c.Request.Host = "evil.attacker.test"
	c.Request.Header.Set("X-Forwarded-Proto", "https")

	got := getRedirectURL(c)
	require.Equal(t, "https://panel.example.com/api/v1/oauth2/callback", got,
		"even with an https hint the host must collapse to InstallHost, never the forged origin")
}

func TestOAuth2_Unbind_UnknownProviderRejected(t *testing.T) {
	defer setupOAuth2Test(t)()

	c, _ := newOAuth2Ctx(t)
	c.Params = gin.Params{{Key: "provider", Value: "unknown-provider"}}
	c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 1}})

	_, err := unbindOauth2(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "provider not found")
}

func TestOAuth2_Unbind_BlocksLastBindWhenRejectPassword(t *testing.T) {
	defer setupOAuth2Test(t)()

	require.NoError(t, singleton.DB.Create(&model.Oauth2Bind{
		UserID:   42,
		Provider: "github",
		OpenID:   "openid-only-one",
	}).Error)

	c, _ := newOAuth2Ctx(t)
	c.Params = gin.Params{{Key: "provider", Value: "github"}}
	c.Set(model.CtxKeyAuthorizedUser, &model.User{
		Common:         model.Common{ID: 42},
		RejectPassword: true,
	})

	_, err := unbindOauth2(c)
	require.Error(t, err,
		"user with reject_password=true must NOT be able to unbind their last OAuth2 provider (would lock them out)")
}

func TestOAuth2_Unbind_AllowsWhenPasswordLoginPossible(t *testing.T) {
	defer setupOAuth2Test(t)()

	require.NoError(t, singleton.DB.Create(&model.Oauth2Bind{
		UserID:   42,
		Provider: "github",
		OpenID:   "openid-1",
	}).Error)

	c, _ := newOAuth2Ctx(t)
	c.Params = gin.Params{{Key: "provider", Value: "github"}}
	c.Set(model.CtxKeyAuthorizedUser, &model.User{
		Common:         model.Common{ID: 42},
		RejectPassword: false,
	})

	_, err := unbindOauth2(c)
	require.NoError(t, err)

	var cnt int64
	require.NoError(t, singleton.DB.Model(&model.Oauth2Bind{}).
		Where("user_id = ? AND provider = ?", 42, "github").Count(&cnt).Error)
	require.Equal(t, int64(0), cnt, "binding must be deleted")
}

func TestOAuth2_Unbind_OnlyAffectsOwnBindings(t *testing.T) {
	defer setupOAuth2Test(t)()

	require.NoError(t, singleton.DB.Create(&model.Oauth2Bind{
		UserID: 42, Provider: "github", OpenID: "mine",
	}).Error)
	require.NoError(t, singleton.DB.Create(&model.Oauth2Bind{
		UserID: 999, Provider: "github", OpenID: "victim",
	}).Error)

	c, _ := newOAuth2Ctx(t)
	c.Params = gin.Params{{Key: "provider", Value: "github"}}
	c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 42}})

	_, err := unbindOauth2(c)
	require.NoError(t, err)

	var victim model.Oauth2Bind
	require.NoError(t, singleton.DB.
		Where("user_id = ? AND provider = ?", 999, "github").
		First(&victim).Error,
		"another user's binding must not be touched")
	require.Equal(t, "victim", victim.OpenID)
}
