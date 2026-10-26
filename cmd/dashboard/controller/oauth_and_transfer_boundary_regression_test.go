package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
)

type configTaskRecorder struct {
	proto.NezhaService_RequestTaskServer
	tasks []*proto.Task
	err   error
}

func (stream *configTaskRecorder) Context() context.Context { return context.Background() }

func (stream *configTaskRecorder) Send(task *proto.Task) error {
	stream.tasks = append(stream.tasks, task)
	return stream.err
}

func TestSecurityBoundaryPendingTransferDeniesConfigDispatch(t *testing.T) {
	defer setupRetryServerTransferFixture(t)()
	seedServer(t, 1, 100)
	require.NotZero(t, seedPendingTransfer(t, 1, 100, 200, 100))
	require.True(t, singleton.ServerTransferShared.HasPending(1))
	server, ok := singleton.ServerShared.Get(1)
	require.True(t, ok)
	require.Equal(t, uint64(200), server.GetUserID())
	stream := &configTaskRecorder{err: errors.New("stop at simulated config task delivery")}
	server.SetTaskStream(stream)
	c := ctxAs(300, model.RoleMember)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	_, err := getServerConfig(c)
	require.Error(t, err)
	require.Empty(t, stream.tasks, "unrelated member is denied before task dispatch")
	c = ctxAs(200, model.RoleMember)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	_, err = getServerConfig(c)
	require.Error(t, err)
	require.NotErrorIs(t, err, stream.err)
	require.Empty(t, stream.tasks, "pending destination must not dispatch config reads")
	// The pending-owner gate rejects the request before a config task is sent.
}

type oauthUserInfoTransport func(*http.Request) (*http.Response, error)

func (transport oauthUserInfoTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestSecurityBoundaryOAuthRejectsInvalidUserInfo(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"normal_identifier", http.StatusOK, `{"id":"provider-user"}`, "provider-user"},
		{"missing_identifier", http.StatusOK, `{"message":"missing user"}`, ""},
		{"error_status", http.StatusUnauthorized, `{"id":"error-response-id"}`, "error-response-id"},
		{"non_json_error", http.StatusBadGateway, "provider unavailable", ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			originalTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			var paths []string
			http.DefaultTransport = oauthUserInfoTransport(func(request *http.Request) (*http.Response, error) {
				paths = append(paths, request.URL.Path)
				status, body := scenario.status, scenario.body
				switch request.URL.Path {
				case "/token":
					status, body = http.StatusOK, `{"access_token":"synthetic-provider-token","token_type":"Bearer"}`
				case "/userinfo":
				default:
					return nil, errors.New("unexpected simulated OAuth path")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})
			c, _ := newOAuth2Ctx(t)
			config := &model.Oauth2Config{ClientID: "synthetic-client", ClientSecret: "synthetic-secret",
				Endpoint:    model.Oauth2Endpoint{TokenURL: "https://provider.example.invalid/token"},
				UserInfoURL: "https://provider.example.invalid/userinfo", UserIDPath: "id"}
			identifier, err := exchangeOpenId(c, config, &model.Oauth2Callback{Code: "synthetic-code"}, "https://panel.example.invalid/callback")
			if scenario.name == "normal_identifier" {
				require.NoError(t, err)
				require.Equal(t, scenario.want, identifier)
			} else {
				require.Error(t, err)
				require.Empty(t, identifier)
			}
			require.Equal(t, []string{"/token", "/userinfo"}, paths, "both exchanges must use the in-process transport")
		})
	}
}

func TestOAuthStateConcurrentConsumptionAndSessionBinding(t *testing.T) {
	defer setupOAuth2Test(t)()
	const key = "single-use-binding"
	singleton.Cache.Set(model.CacheKeyOauth2State+key, &model.Oauth2State{State: "expected-state", Action: model.RTypeBind, UserID: 100, SessionID: "initiating-session"}, time.Minute)
	makeContext := func(userID uint64, session string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/oauth2/callback", nil)
		c.Request.AddCookie(&http.Cookie{Name: "nz-o2s", Value: key})
		c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: userID}})
		c.Set(jwtClaimKeyID, session)
		return c
	}
	_, err := verifyState(makeContext(200, "initiating-session"), "expected-state")
	require.Error(t, err)
	_, err = verifyState(makeContext(100, "different-session"), "expected-state")
	require.Error(t, err)
	results := make(chan error, 8)
	for range 8 {
		c := makeContext(100, "initiating-session")
		go func() { _, err := verifyState(c, "expected-state"); results <- err }()
	}
	accepted := 0
	for range 8 {
		if <-results == nil {
			accepted++
		}
	}
	require.Equal(t, 1, accepted, "only one callback may consume the initiating session's state")
}

func TestTransferURLCannotReviveAfterOwnershipRollback(t *testing.T) {
	defer setupRetryServerTransferFixture(t)()
	seedServer(t, 1, 100)
	token, err := mintTransferToken(transferEntry{UserID: 100, ServerID: 1, Direction: transferDirDownload, ExpiresAt: time.Now().Add(time.Minute)})
	require.NoError(t, err)
	defer transferEntries.Delete(token)
	raw, ok := transferEntries.Load(token)
	require.True(t, ok)
	entry := raw.(transferEntry)
	require.Equal(t, singleton.ServerControlEpoch(1), entry.ControlEpoch)
	transferID := seedPendingTransfer(t, 1, 100, 200, 100)
	require.NotZero(t, transferID)
	_, err = singleton.ServerTransferShared.Cancel(transferID)
	require.NoError(t, err)
	server, ok := singleton.ServerShared.Get(1)
	require.True(t, ok)
	require.Equal(t, uint64(100), server.GetUserID())
	require.ErrorContains(t, revalidateTransferEntry(&entry), "ownership changed since URL issuance")
	ctx := singleton.WithServerControlEpoch(singleton.WithServerControlIdentity(context.Background(), 100, true), entry.ControlEpoch)
	release, err := singleton.BeginServerControlFromContext(ctx, 1)
	require.Nil(t, release)
	require.ErrorContains(t, err, "ownership changed since URL issuance")
}
