package controller

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"github.com/nezhahq/nezha/cmd/dashboard/rpc"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/proto"
	rpcService "github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

// setupNATFlowTest extends setupDashboardCredentialTest with the minimal
// ServeNAT environment: a registered server with a task stream and a fresh
// handler singleton, mirroring the rpc package's serveNAT fixture.
func setupNATFlowTest(t *testing.T) (*rpcService.NezhaHandler, *model.Server, *natFlowTaskStream) {
	t.Helper()
	setupDashboardCredentialTest(t)

	require.NoError(t, singleton.DB.AutoMigrate(&model.Server{}))
	originalServerShared, originalHandler := singleton.ServerShared, rpcService.NezhaHandlerSingleton
	t.Cleanup(func() {
		singleton.ServerShared, rpcService.NezhaHandlerSingleton = originalServerShared, originalHandler
	})

	serverRow := &model.Server{Common: model.Common{ID: 7}, UUID: "nat-credential-flow-test", Name: "nat-credential-flow-test"}
	require.NoError(t, singleton.DB.Create(serverRow).Error)
	singleton.ServerShared = singleton.NewServerClass()
	handler := rpcService.NewNezhaHandler()
	rpcService.NezhaHandlerSingleton = handler

	server, ok := singleton.ServerShared.Get(serverRow.ID)
	require.True(t, ok)
	taskStream := &natFlowTaskStream{}
	server.SetTaskStream(taskStream)

	// Wire the gate exactly as cmd/dashboard/main.go does, with the real
	// classifiers built by setupDashboardCredentialTest.
	rpc.SetNATDashboardCredentialGate(ClassifyDashboardCredentialValues)
	t.Cleanup(func() { rpc.SetNATDashboardCredentialGate(nil) })

	return handler, server, taskStream
}

// TestServeNATWithRealClassifierStripsDashboardCredentialsFromAllChannels pins
// the main() wiring contract end to end: with the real classifiers wired as
// main() does, ServeNAT forwards a request whose dashboard-issued credentials
// — Authorization header, ?token= query parameter, nz-jwt cookie — are all
// stripped, while foreign values and untouched query/cookie data survive
// byte-for-byte.
func TestServeNATWithRealClassifierStripsDashboardCredentialsFromAllChannels(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "bEaReR"} {
		t.Run(scheme, func(t *testing.T) {
			handler, server, taskStream := setupNATFlowTest(t)

			dashboardJWT := mintDashboardCredentialJWT(t, time.Hour)
			oauthAccess := mcpOAuthAccessPrefix + mcpOAuthRandom()
			oauthRefresh := mcpOAuthNewRefresh(mcpOAuthRandom())
			foreignJWT := mintForeignJWT(t)
			// No api_tokens row hashes to this PAT, so the classifier proves it was
			// not issued by this dashboard and it must survive the tunnel.
			foreignPAT := model.APITokenPrefix + "foreign-issued-pat"
			dashboardPAT := model.APITokenPrefix + "dashboard-issued-pat"
			require.NoError(t, singleton.DB.Create(&model.APIToken{
				UserID:    1,
				Name:      "nat-flow",
				TokenHash: model.HashAPIToken(dashboardPAT),
			}).Error)

			connection := &natFlowConn{closed: make(chan struct{})}
			agent := &natFlowAgent{writeDone: make(chan struct{})}
			writer := &natFlowWriter{conn: connection}
			request := &http.Request{
				Method: http.MethodPost,
				URL: &url.URL{Path: "/nat", RawQuery: "keep=1&token=" + dashboardJWT + "&token=" + url.QueryEscape(foreignJWT) +
					"&token=" + foreignPAT + "&token=" + dashboardPAT + "&token=" + oauthAccess + "&token=" + oauthRefresh},
				Header: make(http.Header),
				Body:   http.NoBody,
			}
			request.Header.Set("Authorization", scheme+" "+oauthAccess)
			request.Header.Set("Cookie", "sid=abc; nz-jwt="+dashboardJWT+"; nz-jwt="+foreignJWT+"; nz-jwt="+foreignPAT+"; nz-jwt="+oauthRefresh)
			request.Header.Set("X-Ordinary-Nat", "ordinary")
			taskStream.onSend = func(task *proto.Task) error {
				var nat model.TaskNAT
				if err := json.Unmarshal([]byte(task.Data), &nat); err != nil {
					return err
				}
				return handler.AgentConnected(nat.StreamID, agent)
			}

			done := make(chan struct{})
			go func() {
				rpc.ServeNAT(writer, request, &model.NAT{ServerID: server.ID, Host: "target.example"})
				close(done)
			}()
			select {
			case <-agent.writeDone:
			case <-time.After(time.Second):
				t.Fatal("agent did not receive the transferred NAT request")
			}
			require.NoError(t, connection.Close())
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("ServeNAT did not finish")
			}

			forwarded := string(agent.writtenBytes())
			// Dashboard credentials are gone from every channel.
			require.NotContains(t, forwarded, "Authorization:")
			require.NotContains(t, forwarded, dashboardJWT)
			require.NotContains(t, forwarded, dashboardPAT)
			require.NotContains(t, forwarded, oauthAccess)
			require.NotContains(t, forwarded, oauthRefresh)
			// Foreign credentials and untouched data survive byte-for-byte.
			require.Contains(t, forwarded, "POST /nat?keep=1&token="+url.QueryEscape(foreignJWT)+"&token="+foreignPAT+" HTTP/1.1")
			require.Contains(t, forwarded, "Cookie: sid=abc; nz-jwt="+foreignJWT+"; nz-jwt="+foreignPAT)
			require.Contains(t, forwarded, "X-Ordinary-Nat: ordinary")
		})
	}
}

// TestServeNATWithRealClassifierKeepsForeignCredentials pins that the real
// classifier deterministically proves foreign values were not issued by this
// dashboard and leaves every channel untouched.
func TestServeNATWithRealClassifierKeepsForeignCredentials(t *testing.T) {
	handler, server, taskStream := setupNATFlowTest(t)

	foreignJWT := mintForeignJWT(t)
	connection := &natFlowConn{closed: make(chan struct{})}
	agent := &natFlowAgent{writeDone: make(chan struct{})}
	writer := &natFlowWriter{conn: connection}
	request := &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Path: "/nat", RawQuery: "token=" + foreignJWT},
		Header: make(http.Header),
		Body:   http.NoBody,
	}
	request.Header.Set("Authorization", "Bearer "+foreignJWT)
	request.Header.Set("Cookie", "nz-jwt="+foreignJWT)
	taskStream.onSend = func(task *proto.Task) error {
		var nat model.TaskNAT
		if err := json.Unmarshal([]byte(task.Data), &nat); err != nil {
			return err
		}
		return handler.AgentConnected(nat.StreamID, agent)
	}

	done := make(chan struct{})
	go func() {
		rpc.ServeNAT(writer, request, &model.NAT{ServerID: server.ID, Host: "target.example"})
		close(done)
	}()
	select {
	case <-agent.writeDone:
	case <-time.After(time.Second):
		t.Fatal("agent did not receive the transferred NAT request")
	}
	require.NoError(t, connection.Close())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ServeNAT did not finish")
	}

	forwarded := string(agent.writtenBytes())
	require.Contains(t, forwarded, "POST /nat?token="+foreignJWT+" HTTP/1.1")
	require.Contains(t, forwarded, "Authorization: Bearer "+foreignJWT)
	require.Contains(t, forwarded, "Cookie: nz-jwt="+foreignJWT)
}

type natFlowTaskStream struct {
	onSend func(*proto.Task) error
}

func (stream *natFlowTaskStream) Send(task *proto.Task) error {
	if stream.onSend != nil {
		return stream.onSend(task)
	}
	return nil
}
func (*natFlowTaskStream) Recv() (*proto.TaskResult, error) { return nil, io.EOF }
func (*natFlowTaskStream) SetHeader(metadata.MD) error      { return nil }
func (*natFlowTaskStream) SendHeader(metadata.MD) error     { return nil }
func (*natFlowTaskStream) SetTrailer(metadata.MD)           {}
func (*natFlowTaskStream) Context() context.Context         { return context.Background() }
func (*natFlowTaskStream) SendMsg(any) error                { return nil }
func (*natFlowTaskStream) RecvMsg(any) error                { return io.EOF }

type natFlowWriter struct{ conn *natFlowConn }

func (writer *natFlowWriter) Header() http.Header            { return http.Header{} }
func (writer *natFlowWriter) Write(data []byte) (int, error) { return len(data), nil }
func (writer *natFlowWriter) WriteHeader(int)                {}
func (writer *natFlowWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	go func() { _, _ = writer.conn.Read(make([]byte, 1)) }()
	return writer.conn, bufio.NewReadWriter(bufio.NewReader(writer.conn), bufio.NewWriter(writer.conn)), nil
}

type natFlowConn struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func (conn *natFlowConn) Read([]byte) (int, error) {
	<-conn.closed
	return 0, io.EOF
}
func (conn *natFlowConn) Write(data []byte) (int, error) { return len(data), nil }
func (conn *natFlowConn) Close() error {
	conn.closeOnce.Do(func() { close(conn.closed) })
	return nil
}
func (conn *natFlowConn) LocalAddr() net.Addr              { return natFlowAddr{} }
func (conn *natFlowConn) RemoteAddr() net.Addr             { return natFlowAddr{} }
func (conn *natFlowConn) SetDeadline(time.Time) error      { return nil }
func (conn *natFlowConn) SetReadDeadline(time.Time) error  { return nil }
func (conn *natFlowConn) SetWriteDeadline(time.Time) error { return nil }

type natFlowAddr struct{}

func (natFlowAddr) Network() string { return "test" }
func (natFlowAddr) String() string  { return "test" }

type natFlowAgent struct {
	mu        sync.Mutex
	written   bytes.Buffer
	writeDone chan struct{}
	once      sync.Once
}

func (agent *natFlowAgent) Read([]byte) (int, error) { return 0, io.EOF }
func (agent *natFlowAgent) Write(data []byte) (int, error) {
	agent.mu.Lock()
	agent.written.Write(data)
	agent.mu.Unlock()
	agent.once.Do(func() { close(agent.writeDone) })
	return len(data), nil
}
func (*natFlowAgent) Close() error { return nil }
func (agent *natFlowAgent) writtenBytes() []byte {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return append([]byte(nil), agent.written.Bytes()...)
}
