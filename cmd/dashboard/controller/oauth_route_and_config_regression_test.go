package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
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
