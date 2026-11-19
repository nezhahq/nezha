package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestOAuthBindingThroughAuthenticatedDashboardRoute(t *testing.T) {
	session := setupMCPOAuthSession(t)
	previousCache := singleton.Cache
	singleton.Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() { singleton.Cache = previousCache })
	singleton.Conf.Oauth2 = map[string]*model.Oauth2Config{"github": {ClientID: "dashboard", Endpoint: model.Oauth2Endpoint{AuthURL: "https://provider.example/authorize"}}}
	for _, test := range []struct {
		name, action     string
		browser, success bool
	}{
		{"authenticated binding", "2", true, true},
		{"anonymous binding", "2", false, false},
		{"anonymous login", "1", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := session.request(t, http.MethodGet, "/api/v1/oauth2/github?type="+test.action, nil, test.browser)
			var body struct {
				Success bool                      `json:"success"`
				Data    model.Oauth2LoginResponse `json:"data"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			require.Equal(t, test.success, body.Success, response.Body.String())
			if !test.success {
				require.Empty(t, response.Result().Cookies())
				require.Empty(t, body.Data.Redirect)
				return
			}
			redirect, err := url.Parse(body.Data.Redirect)
			require.NoError(t, err)
			require.Equal(t, "provider.example", redirect.Host)
			var stateCookie *http.Cookie
			for _, cookie := range response.Result().Cookies() {
				if cookie.Name == "nz-o2s" {
					stateCookie = cookie
				}
			}
			require.NotNil(t, stateCookie)
			cached, ok := singleton.Cache.Get(model.CacheKeyOauth2State + stateCookie.Value)
			require.True(t, ok)
			state := cached.(*model.Oauth2State)
			require.Equal(t, redirect.Query().Get("state"), state.State)
			require.Equal(t, "github", state.Provider)
			if test.browser {
				require.Equal(t, session.userID, state.UserID)
				var stored model.JWTSession
				require.NoError(t, singleton.DB.Where("key_id = ?", state.SessionID).First(&stored).Error)
				require.Equal(t, session.userID, stored.UserID)
				require.Equal(t, model.RTypeBind, state.Action)
			} else {
				require.Zero(t, state.UserID)
				require.Empty(t, state.SessionID)
				require.Equal(t, model.RTypeLogin, state.Action)
			}
		})
	}
}

func TestPublicAuthenticationURLsWaitForConfigCommit(t *testing.T) {
	for _, reader := range []string{"issuer", "transfer", "oauth callback"} {
		for _, rollback := range []bool{false, true} {
			name := reader + "/commit"
			if rollback {
				name = reader + "/rollback"
			}
			t.Run(name, func(t *testing.T) {
				cleanup, _ := setupMCPTest(t)
				defer cleanup()
				singleton.Conf.JWTSecretKey = "configuration-commit-signing-key"
				entered, finish := make(chan struct{}), make(chan struct{})
				committed := make(chan error, 1)
				persistenceError := errors.New("persistence failed")
				go func() {
					committed <- singleton.Conf.CommitNATDomainPolicy(model.SettingForm{DashboardHost: "new.example.com"}, func() error {
						close(entered)
						<-finish
						if rollback {
							return persistenceError
						}
						return nil
					})
				}()
				<-entered
				type outcome struct {
					value string
					err   error
				}
				started, result := make(chan struct{}), make(chan outcome, 1)
				go func() {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodGet, "https://untrusted.example/", nil)
					c.Request.Header.Set("X-Forwarded-Proto", "https")
					close(started)
					var value string
					var err error
					switch reader {
					case "issuer":
						value, err = mcpOAuthIssuer()
					case "transfer":
						value, err = mcpTransferURLBase(c)
					default:
						value = getRedirectURL(c)
					}
					result <- outcome{value, err}
				}()
				<-started
				var early *outcome
				select {
				case got := <-result:
					early = &got
				case <-time.After(50 * time.Millisecond):
				}
				close(finish)
				err := <-committed
				if rollback {
					require.ErrorIs(t, err, persistenceError)
				} else {
					require.NoError(t, err)
				}
				var got outcome
				if early != nil {
					got = *early
					t.Error("authentication URL read uncommitted configuration")
				} else {
					select {
					case got = <-result:
					case <-time.After(time.Second):
						t.Fatal("configuration reader did not resume")
					}
				}
				require.NoError(t, got.err)
				host := "new.example.com"
				if rollback {
					host = "example.com"
				}
				want := "https://" + host
				if reader == "oauth callback" {
					want += "/api/v1/oauth2/callback"
				}
				require.Equal(t, want, got.value)
			})
		}
	}
}

// A password change revokes every session of the account. A bind callback that
// is waiting for the provider at that moment must neither store the binding nor
// answer the revoked session with a new one.
func TestOAuthBindCallbackRechecksSessionAfterProviderExchange(t *testing.T) {
	for _, test := range []struct {
		name    string
		changed bool
	}{
		{"credentials unchanged", false},
		{"password changed while the provider answers", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := setupMCPOAuthSession(t)
			previousCache := singleton.Cache
			singleton.Cache = cache.New(time.Minute, time.Minute)
			t.Cleanup(func() { singleton.Cache = previousCache })
			previousUsers, previousSecrets := singleton.UserInfoMap, singleton.AgentSecretToUserId
			singleton.UserInfoMap, singleton.AgentSecretToUserId = map[uint64]model.UserInfo{}, map[string]uint64{}
			t.Cleanup(func() { singleton.UserInfoMap, singleton.AgentSecretToUserId = previousUsers, previousSecrets })
			require.NoError(t, singleton.DB.AutoMigrate(&model.Oauth2Bind{}))
			hash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
			require.NoError(t, err)
			require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", session.userID).UpdateColumn("password", string(hash)).Error)
			singleton.Conf.Oauth2 = map[string]*model.Oauth2Config{"github": {ClientID: "dashboard", ClientSecret: "secret",
				Endpoint:    model.Oauth2Endpoint{AuthURL: "https://provider.example.invalid/authorize", TokenURL: "https://provider.example.invalid/token"},
				UserInfoURL: "https://provider.example.invalid/userinfo", UserIDPath: "id"}}

			start := session.request(t, http.MethodGet, "/api/v1/oauth2/github?type=2", nil, true)
			var started struct {
				Data model.Oauth2LoginResponse `json:"data"`
			}
			require.NoError(t, json.Unmarshal(start.Body.Bytes(), &started))
			redirect, err := url.Parse(started.Data.Redirect)
			require.NoError(t, err, start.Body.String())
			var stateCookie *http.Cookie
			for _, cookie := range start.Result().Cookies() {
				if cookie.Name == "nz-o2s" {
					stateCookie = cookie
				}
			}
			require.NotNil(t, stateCookie)

			originalTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			http.DefaultTransport = oauthUserInfoTransport(func(request *http.Request) (*http.Response, error) {
				body := `{"access_token":"synthetic-provider-token","token_type":"Bearer"}`
				if request.URL.Path == "/userinfo" {
					body = `{"id":"provider-user"}`
					if test.changed {
						change := session.request(t, http.MethodPost, "/api/v1/profile", model.ProfileForm{OriginalPassword: "old-password", NewPassword: "new-password"}, true)
						require.Contains(t, change.Body.String(), `"success":true`)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})

			callback := httptest.NewRequest(http.MethodGet, "https://example.com/api/v1/oauth2/callback?code=synthetic-code&state="+url.QueryEscape(redirect.Query().Get("state")), nil)
			callback.AddCookie(session.cookie)
			callback.AddCookie(stateCookie)
			response := httptest.NewRecorder()
			session.router.ServeHTTP(response, callback)

			var issued *http.Cookie
			for _, cookie := range response.Result().Cookies() {
				if cookie.Name == "nz-jwt" {
					issued = cookie
				}
			}
			var binds, liveSessions int64
			require.NoError(t, singleton.DB.Model(&model.Oauth2Bind{}).Where("user_id = ?", session.userID).Count(&binds).Error)
			require.NoError(t, singleton.DB.Model(&model.JWTSession{}).Where("user_id = ? AND revoked_at IS NULL AND token_version = (?)", session.userID,
				singleton.DB.Model(&model.User{}).Select("token_version").Where("id = ?", session.userID)).Count(&liveSessions).Error)
			if !test.changed {
				require.Equal(t, http.StatusFound, response.Code, response.Body.String())
				require.NotNil(t, issued)
				require.EqualValues(t, 1, binds)
				require.EqualValues(t, 2, liveSessions, "the binding session and the one the callback issued")
				return
			}
			require.Contains(t, response.Body.String(), `"error":"unauthorized"`)
			require.Nil(t, issued, "a revoked session must not be answered with a new one")
			require.Zero(t, binds, "a revoked session must not bind an identity")
			require.Zero(t, liveSessions)
		})
	}
}
