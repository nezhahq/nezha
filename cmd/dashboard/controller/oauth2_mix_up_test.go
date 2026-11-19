package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// OAuth mix-up: with several providers a code must only be posted to the
// token endpoint of a provider when it arrived on that provider's own
// redirect URI. A hostile provider that forwards the login to another
// provider must not be handed that provider's code.
func TestOAuth2_Callback_RejectsCodeFromAnotherProvider(t *testing.T) {
	session := setupMCPOAuthSession(t)
	previousCache := singleton.Cache
	singleton.Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() { singleton.Cache = previousCache })

	var tokenRequests atomic.Int32
	community := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer community.Close()
	providers := map[string]*model.Oauth2Config{
		"corporate": {ClientID: "at-corporate", Endpoint: model.Oauth2Endpoint{AuthURL: "https://corporate.example/authorize"}},
		"community": {ClientID: "at-community", Endpoint: model.Oauth2Endpoint{
			AuthURL: "https://community.example/authorize", TokenURL: community.URL}},
	}

	// startLogin asks for the provider's authorize URL like a browser does and
	// returns the redirect_uri sent to the provider, the state and the cookie.
	startLogin := func(t *testing.T, provider string) (*url.URL, string, *http.Cookie) {
		t.Helper()
		response := session.request(t, http.MethodGet, "/api/v1/oauth2/"+provider+"?type=1", nil, false)
		var body struct {
			Data model.Oauth2LoginResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
		authorize, err := url.Parse(body.Data.Redirect)
		require.NoError(t, err)
		redirectURI, err := url.Parse(authorize.Query().Get("redirect_uri"))
		require.NoError(t, err)
		for _, cookie := range response.Result().Cookies() {
			if cookie.Name == "nz-o2s" {
				return redirectURI, authorize.Query().Get("state"), cookie
			}
		}
		t.Fatal("no state cookie")
		return nil, "", nil
	}

	t.Run("each provider has its own redirect URI", func(t *testing.T) {
		singleton.Conf.Oauth2 = providers
		corporate, _, _ := startLogin(t, "corporate")
		require.Equal(t, "/api/v1/oauth2/callback/corporate", corporate.Path)
		community, _, _ := startLogin(t, "community")
		require.Equal(t, "/api/v1/oauth2/callback/community", community.Path)
	})

	t.Run("a single provider keeps the shared redirect URI", func(t *testing.T) {
		singleton.Conf.Oauth2 = map[string]*model.Oauth2Config{"community": providers["community"]}
		redirectURI, state, cookie := startLogin(t, "community")
		require.Equal(t, "/api/v1/oauth2/callback", redirectURI.Path)
		tokenRequests.Store(0)
		request := httptest.NewRequest(http.MethodGet, "https://example.com/api/v1/oauth2/callback?code=c&state="+state, nil)
		request.AddCookie(cookie)
		session.router.ServeHTTP(httptest.NewRecorder(), request)
		require.Positive(t, tokenRequests.Load())
	})

	for path, reached := range map[string]bool{
		"/api/v1/oauth2/callback":           false,
		"/api/v1/oauth2/callback/corporate": false,
		"/api/v1/oauth2/callback/community": true,
	} {
		t.Run("state of community, code arrives on "+path, func(t *testing.T) {
			singleton.Conf.Oauth2 = providers
			_, state, cookie := startLogin(t, "community")
			tokenRequests.Store(0)
			request := httptest.NewRequest(http.MethodGet, "https://example.com"+path+"?code=issued-by-corporate&state="+state, nil)
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			session.router.ServeHTTP(response, request)
			require.Equal(t, reached, tokenRequests.Load() > 0, response.Body.String())
			require.Empty(t, response.Header().Get("Set-Cookie"), "no session may be issued")
			_, pending := singleton.Cache.Get(model.CacheKeyOauth2State + cookie.Value)
			require.False(t, pending, "the state is single-use whatever the outcome")
		})
	}
}
