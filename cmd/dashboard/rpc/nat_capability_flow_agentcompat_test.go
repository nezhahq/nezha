//go:build agentcompat

package rpc

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/agentcompatcontract"
	"github.com/nezhahq/nezha/proto"
	serviceRPC "github.com/nezhahq/nezha/service/rpc"
	"github.com/stretchr/testify/require"
)

func TestServeNATAgentCompatPublishesExactStreamAfterRequestTransfer(t *testing.T) {
	fixture := newServeNATFixture(t)
	installNATCredentialTestGate(t)
	capability, err := fixture.handler.RegisterAgentCompatIOStreamCapability(context.Background(), serviceRPC.AgentCompatCapabilityRegistration{
		Owner:               serviceRPC.AgentCompatCapabilityOwner{PATID: 1, UserID: 2},
		Purpose:             serviceRPC.AgentCompatCapabilityNAT,
		TargetServerID:      fixture.server.ID,
		ResourceID:          91,
		ServerAccessAllowed: true,
	})
	require.NoError(t, err)
	access, err := serviceRPC.ParseAgentCompatIOStreamCapability(capability.String())
	require.NoError(t, err)
	connection := newServeNATConn()
	writer := &serveNATResponseWriter{conn: connection, prefetched: []byte("payload")}
	agent := &orderedNATAgent{writeMarker: []byte("payload"), readStarted: make(chan struct{}), readRelease: make(chan struct{}), writeStarted: make(chan struct{})}
	taskStreamIDReady := make(chan string, 1)
	request := &http.Request{
		Method:        http.MethodPost,
		URL:           &url.URL{Scheme: "http", Host: "example.test", Path: "/nat"},
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader("payload")),
		ContentLength: int64(len("payload")),
	}
	request.Header.Set(agentcompatcontract.IOStreamCapabilityHeader, capability.String())
	request.Header.Set("Authorization", natTestForeignJWT)
	request.Header.Set("X-Ordinary-NAT", "ordinary")
	fixture.taskStream.onSend = func(task *proto.Task) error {
		var nat model.TaskNAT
		if err := json.Unmarshal([]byte(task.Data), &nat); err != nil {
			return err
		}
		if err := fixture.handler.AgentConnected(nat.StreamID, agent); err != nil {
			return err
		}
		taskStreamIDReady <- nat.StreamID
		return nil
	}
	serveDone := make(chan struct{})
	go func() {
		ServeNAT(writer, request, &model.NAT{Common: model.Common{ID: 91, UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})
		close(serveDone)
	}()
	streamID, err := fixture.handler.WaitAgentCompatIOStreamCapability(context.Background(), serviceRPC.AgentCompatCapabilityAccess{
		Capability:          access,
		Owner:               serviceRPC.AgentCompatCapabilityOwner{PATID: 1, UserID: 2},
		Purpose:             serviceRPC.AgentCompatCapabilityNAT,
		TargetServerID:      fixture.server.ID,
		ResourceID:          91,
		ServerAccessAllowed: true,
	})
	require.NoError(t, err)
	taskStreamID := <-taskStreamIDReady
	require.Equal(t, taskStreamID, streamID)
	select {
	case <-agent.readStarted:
	case <-serveDone:
		t.Fatal("ServeNAT returned before StartStream read")
	}
	select {
	case <-agent.writeStarted:
	case <-serveDone:
		t.Fatal("ServeNAT returned before request reached agent")
	}
	close(agent.readRelease)
	require.NoError(t, connection.Close())
	completionContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-serveDone:
	case <-completionContext.Done():
		t.Fatal("ServeNAT did not complete after StartStream")
	}
	require.NoError(t, fixture.handler.CancelAgentCompatIOStreamCapability(serviceRPC.AgentCompatCapabilityAccess{
		Capability:          access,
		Owner:               serviceRPC.AgentCompatCapabilityOwner{PATID: 1, UserID: 2},
		Purpose:             serviceRPC.AgentCompatCapabilityNAT,
		TargetServerID:      fixture.server.ID,
		ResourceID:          91,
		ServerAccessAllowed: true,
	}))
	require.NotContains(t, string(agent.writtenBytes()), capability.String())
	forwarded := string(agent.writtenBytes())
	for _, expected := range []string{"POST /nat HTTP/1.1", "Host: example.test", "X-Ordinary-Nat: ordinary", "payload"} {
		require.Contains(t, forwarded, expected)
	}
	require.NotContains(t, forwarded, agentcompatcontract.IOStreamCapabilityHeader)
	// Foreign credentials are ordinary request data for the NAT backend and
	// must survive the transfer; dashboard-issued values are stripped before
	// any byte leaves the dashboard.
	require.Contains(t, forwarded, "Authorization: "+natTestForeignJWT)
}

func TestServeNATAgentCompatDoesNotForwardDashboardCredentials(t *testing.T) {
	fixture := newServeNATFixture(t)
	installNATCredentialTestGate(t)
	capability, err := fixture.handler.RegisterAgentCompatIOStreamCapability(context.Background(), serviceRPC.AgentCompatCapabilityRegistration{
		Owner:               serviceRPC.AgentCompatCapabilityOwner{PATID: 1, UserID: 2},
		Purpose:             serviceRPC.AgentCompatCapabilityNAT,
		TargetServerID:      fixture.server.ID,
		ResourceID:          91,
		ServerAccessAllowed: true,
	})
	require.NoError(t, err)
	connection := newServeNATConn()
	writer := &serveNATResponseWriter{conn: connection}
	agent := &serveNATAgent{readErr: io.EOF, writeDone: make(chan struct{})}
	request := &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Scheme: "http", Host: "example.test", Path: "/nat"},
		Header: make(http.Header),
		Body:   io.NopCloser(strings.NewReader("payload")),
	}
	request.Header.Set(agentcompatcontract.IOStreamCapabilityHeader, capability.String())
	request.Header.Set("Authorization", natTestDashboardJWT)
	fixture.taskStream.onSend = func(task *proto.Task) error {
		var nat model.TaskNAT
		if err := json.Unmarshal([]byte(task.Data), &nat); err != nil {
			return err
		}
		return fixture.handler.AgentConnected(nat.StreamID, agent)
	}
	done := make(chan struct{})
	go func() {
		ServeNAT(writer, request, &model.NAT{Common: model.Common{ID: 91, UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})
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
		t.Fatal("ServeNAT did not complete")
	}
	forwarded := strings.ToLower(string(agent.writtenBytes()))
	require.NotContains(t, forwarded, "authorization:")
	for _, dashboardCredential := range []string{natTestDashboardJWT, natTestDashboardExpiredJWT, natTestDashboardAPIToken} {
		require.NotContains(t, forwarded, strings.ToLower(dashboardCredential))
	}
}

type orderedNATAgent struct {
	writeMarker  []byte
	mu           sync.Mutex
	written      bytes.Buffer
	readStarted  chan struct{}
	readRelease  chan struct{}
	writeStarted chan struct{}
	readOnce     sync.Once
	writeOnce    sync.Once
	closeCount   int
}

func (agent *orderedNATAgent) Read([]byte) (int, error) {
	agent.readOnce.Do(func() { close(agent.readStarted) })
	<-agent.readRelease
	return 0, io.EOF
}

func (agent *orderedNATAgent) Write(data []byte) (int, error) {
	agent.mu.Lock()
	count, err := agent.written.Write(data)
	complete := len(agent.writeMarker) == 0 || bytes.Contains(agent.written.Bytes(), agent.writeMarker)
	agent.mu.Unlock()
	if complete {
		agent.writeOnce.Do(func() { close(agent.writeStarted) })
	}
	return count, err
}

func (agent *orderedNATAgent) Close() error {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.closeCount++
	return nil
}

func (agent *orderedNATAgent) writtenBytes() []byte {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return append([]byte(nil), agent.written.Bytes()...)
}

var _ io.ReadWriteCloser = (*orderedNATAgent)(nil)
