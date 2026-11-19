package controller

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/rpc"
)

// stalledAgentEndpoint behaves like the IOStream of an agent that stopped
// reading: Write waits for flow-control credit that never comes, Read has
// nothing to deliver, and Close does not interrupt either of them.
type stalledAgentEndpoint struct {
	writeStarted chan struct{}
	startOnce    sync.Once
	release      chan struct{}
}

func (agent *stalledAgentEndpoint) Read([]byte) (int, error) {
	<-agent.release
	return 0, websocket.ErrCloseSent
}

func (agent *stalledAgentEndpoint) Write([]byte) (int, error) {
	agent.startOnce.Do(func() { close(agent.writeStarted) })
	<-agent.release
	return 0, websocket.ErrCloseSent
}

func (agent *stalledAgentEndpoint) Close() error { return nil }

// Revoking the API token that opened a terminal or file-manager session has to
// release the IO stream (and with it the creator's slot) and end the handler
// even while the relay is blocked on the agent.
func TestIOStreamSessionTokenRevocationDetachesStreamFromStalledAgent(t *testing.T) {
	for name, handler := range map[string]func(*gin.Context) (any, error){
		"terminal":     terminalStream,
		"file manager": fmStream,
	} {
		t.Run(name, func(t *testing.T) {
			// Given
			gin.SetMode(gin.TestMode)
			ensureLocalizerForStreamTests(t)
			originalUpgrader := upgrader
			upgrader = &websocket.Upgrader{}
			t.Cleanup(func() { upgrader = originalUpgrader })

			const streamID = "session-on-stalled-agent"
			tokenID := uint64(time.Now().UnixNano()) // the registry remembers revoked ids across tests
			originalHandler := rpc.NezhaHandlerSingleton
			rpc.NezhaHandlerSingleton = rpc.NewNezhaHandler()
			t.Cleanup(func() { rpc.NezhaHandlerSingleton = originalHandler })
			require.NoError(t, rpc.NezhaHandlerSingleton.CreateStream(streamID, 100, 1))
			agent := &stalledAgentEndpoint{writeStarted: make(chan struct{}), release: make(chan struct{})}
			t.Cleanup(func() { close(agent.release) })
			require.NoError(t, rpc.NezhaHandlerSingleton.AgentConnected(streamID, agent))

			router := gin.New()
			router.Use(func(c *gin.Context) {
				setAuthUser(c, 100, model.RoleMember)
				c.Set(apiTokenCtxKey, &model.APIToken{ID: tokenID, UserID: 100})
				c.Next()
			})
			handlerReturned := make(chan struct{})
			router.GET("/ws/:id", commonHandler(func(c *gin.Context) (any, error) {
				defer close(handlerReturned)
				return handler(c)
			}))
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)

			tab, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/"+streamID, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tab.Close() })
			require.NoError(t, tab.WriteMessage(websocket.TextMessage, []byte("l")))
			select {
			case <-agent.writeStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("relay never wrote to the agent")
			}

			// When
			patConnectionRegistryShared.revokeToken(tokenID)

			// Then
			require.Eventually(t, func() bool {
				_, registered := rpc.NezhaHandlerSingleton.StreamOwnership(streamID)
				return !registered
			}, 5*time.Second, 10*time.Millisecond, "stream still registered after its token was revoked")
			select {
			case <-handlerReturned:
			case <-time.After(5 * time.Second):
				t.Fatal("handler still waiting for the blocked relay after its stream was detached")
			}
		})
	}
}
