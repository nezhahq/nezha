package rpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/grpcx"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

const (
	stalledAgentUserID   = 100
	stalledAgentServerID = 3
	// The relay goroutine of a session, parked in its write to the agent.
	stalledRelayWrite = "grpcx.(*IOStreamWrapper).Write"
)

func goroutinesIn(frame string) int {
	stacks := make([]byte, 8<<20)
	stacks = stacks[:runtime.Stack(stacks, true)]
	return strings.Count(string(stacks), frame)
}

// newIOStreamAgentFixture serves a NezhaHandler the way the dashboard does,
// gRPC over cleartext HTTP/2 on grpc-go's ServeHTTP transport, and connects an
// agent of user stalledAgentUserID's server stalledAgentServerID to it.
func newIOStreamAgentFixture(t *testing.T) (*NezhaHandler, pb.NezhaServiceClient, context.Context) {
	t.Helper()
	t.Cleanup(setupAuthAgentFixture(t))
	const agentSecret, hostUUID = "stalled-agent-secret", "5e7e7000-0000-4000-8000-000000000001"
	if err := singleton.DB.Create(&model.Server{
		Common: model.Common{ID: stalledAgentServerID, UserID: stalledAgentUserID},
		UUID:   hostUUID,
	}).Error; err != nil {
		t.Fatalf("create server: %v", err)
	}
	singleton.ServerShared = singleton.NewServerClass()
	singleton.UserLock.Lock()
	originalSecrets := singleton.AgentSecretToUserId
	singleton.AgentSecretToUserId = map[string]uint64{agentSecret: stalledAgentUserID}
	singleton.UserLock.Unlock()
	t.Cleanup(func() {
		singleton.UserLock.Lock()
		singleton.AgentSecretToUserId = originalSecrets
		singleton.UserLock.Unlock()
	})

	handler := NewNezhaHandler()
	grpcServer := grpc.NewServer()
	pb.RegisterNezhaServiceServer(grpcServer, handler)
	dashboard := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grpcx.ServeAbortable(grpcServer, w, r)
	}))
	dashboard.Config.Protocols = new(http.Protocols)
	dashboard.Config.Protocols.SetUnencryptedHTTP2(true)
	dashboard.Start()
	t.Cleanup(dashboard.Close)

	// A fixed window disables grpc-go's dynamic window, so a stream nobody
	// reads accepts 65,535 bytes and no more.
	conn, err := grpc.NewClient(dashboard.Listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithInitialWindowSize(65535))
	if err != nil {
		t.Fatalf("dial dashboard: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, hangUp := context.WithCancel(metadata.AppendToOutgoingContext(context.Background(),
		"client_secret", agentSecret, "client_uuid", hostUUID))
	t.Cleanup(hangUp)
	return handler, pb.NewNezhaServiceClient(conn), ctx
}

// runIOStreamSession runs one terminal / fm style session to its end: the
// agent attaches, the user sends more than the agent's flow-control window,
// then the agent half-closes its stream and keeps it open. It returns once
// CloseStream has released the session's quota slot. An agent that reads
// reports on the returned channel how its stream ended; a stalled agent never
// reads at all.
func runIOStreamSession(t *testing.T, handler *NezhaHandler, client pb.NezhaServiceClient, ctx context.Context, sessionID string, agentReads bool) <-chan error {
	t.Helper()
	if err := handler.CreateStream(sessionID, stalledAgentUserID, stalledAgentServerID); err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	agent, err := client.IOStream(ctx)
	if err != nil {
		t.Fatalf("open IOStream: %v", err)
	}
	if err := agent.Send(&pb.IOStreamData{Data: append([]byte{0xff, 0x05, 0xff, 0x05}, sessionID...)}); err != nil {
		t.Fatalf("send stream id: %v", err)
	}
	if _, attached := handler.WaitForAgent(ctx, sessionID, 5*time.Second); !attached {
		t.Fatal("agent did not attach")
	}
	var received atomic.Int64
	streamEnd := make(chan error, 1)
	if agentReads {
		go func() {
			for {
				data, err := agent.Recv()
				if err != nil {
					streamEnd <- err
					return
				}
				received.Add(int64(len(data.GetData())))
			}
		}()
	}
	user := newPipeReadWriter()
	if err := handler.UserConnected(sessionID, user); err != nil {
		t.Fatalf("UserConnected: %v", err)
	}
	relayDone := make(chan struct{})
	go func() {
		_ = handler.StartStream(sessionID, 5*time.Second)
		close(relayDone)
	}()
	// The pipe hands each message to the relay before Write returns. With a
	// stalled agent the first one parks the transport's writer on flow control,
	// the second one queues behind it and parks the relay goroutine with its
	// buffer.
	sent := 0
	for _, message := range [][]byte{make([]byte, 128<<10), {0}} {
		if _, err := user.Write(message); err != nil {
			t.Fatalf("user write: %v", err)
		}
		sent += len(message)
	}
	for deadline := time.Now().Add(5 * time.Second); agentReads && received.Load() != int64(sent); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("agent received %d of %d bytes", received.Load(), sent)
		}
	}
	if err := agent.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	<-relayDone
	_ = handler.CloseStream(sessionID)
	return streamEnd
}

// TestIOStream_ClosedSessionReleasedWhenAgentStalls pins the invariant of
// TestStartStream_NoGoroutineLeakAfterClose for the real agent endpoint: a
// session whose agent never reads its IOStream must not outlive CloseStream.
// On grpc-go's ServeHTTP transport the relay write queued behind HTTP/2 flow
// control used to stay parked, outside the per-user / per-server stream caps,
// until the agent dropped its connection.
func TestIOStream_ClosedSessionReleasedWhenAgentStalls(t *testing.T) {
	handler, client, ctx := newIOStreamAgentFixture(t)

	runIOStreamSession(t, handler, client, ctx, "stalled-session", false)
	if n := handler.StreamCount(); n != 0 {
		t.Fatalf("closed session still holds a quota slot: StreamCount = %d", n)
	}
	if goroutinesIn(stalledRelayWrite) == 0 {
		t.Fatal("relay never blocked on the stalled agent")
	}

	for deadline := time.Now().Add(ioStreamCloseGrace + 5*time.Second); goroutinesIn(stalledRelayWrite) != 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("relay goroutine of a closed session is still parked in a write to the stalled agent")
		}
	}
}

// TestIOStream_ClosingSessionsAreBounded covers sessions that are opened and
// closed faster than ioStreamCloseGrace releases them: no more than
// maxClosingStreams of them may be waiting for their agent at any time.
func TestIOStream_ClosingSessionsAreBounded(t *testing.T) {
	handler, client, ctx := newIOStreamAgentFixture(t)

	started := time.Now()
	for i := 0; i < maxClosingStreams+10; i++ {
		runIOStreamSession(t, handler, client, ctx, fmt.Sprintf("stalled-session-%d", i), false)
	}
	for deadline := time.Now().Add(500 * time.Millisecond); ; time.Sleep(10 * time.Millisecond) {
		parked := goroutinesIn(stalledRelayWrite)
		if parked <= maxClosingStreams {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d closed sessions are still parked in a write to the stalled agent %v after the first one was opened, want at most %d",
				parked, time.Since(started), maxClosingStreams)
		}
	}
}

// TestIOStream_ClosedSessionEndsCleanlyWhenAgentReads guards the other side:
// an agent that keeps up gets everything the user sent and then the normal end
// of its stream, never a reset, however many sessions are closed in a row.
func TestIOStream_ClosedSessionEndsCleanlyWhenAgentReads(t *testing.T) {
	handler, client, ctx := newIOStreamAgentFixture(t)

	for i := 0; i < maxClosingStreams+10; i++ {
		streamEnd := runIOStreamSession(t, handler, client, ctx, fmt.Sprintf("healthy-session-%d", i), true)
		select {
		case err := <-streamEnd:
			if err != io.EOF {
				t.Fatalf("session %d: agent stream ended with %v, want io.EOF", i, err)
			}
		case <-time.After(ioStreamCloseGrace + 5*time.Second):
			t.Fatalf("session %d: agent stream did not end", i)
		}
	}
}
