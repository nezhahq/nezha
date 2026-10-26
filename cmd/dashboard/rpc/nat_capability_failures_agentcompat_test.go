//go:build agentcompat

package rpc

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/agentcompatcontract"
	"github.com/nezhahq/nezha/proto"
	serviceRPC "github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
)

func TestServeNATAgentCompatReleasesCapabilityBeforeServerDispatch(t *testing.T) {
	fixture := newServeNATFixture(t)
	capability, access := registerNATCapabilityForTest(t, fixture.handler, fixture.server.ID, 101)
	originalServers := singleton.ServerShared
	singleton.ServerShared = singleton.NewServerClass()
	t.Cleanup(func() { singleton.ServerShared = originalServers })
	request := capabilityRequest(capability)

	ServeNAT(&serveNATResponseWriter{}, request, &model.NAT{Common: model.Common{ID: 101, UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})

	assertNATCapabilitySlotReleased(t, fixture.handler, access)
	require.Equal(t, 0, fixture.handler.StreamCount())
	require.Empty(t, fixture.taskStream.sent)
}

func TestServeNATAgentCompatTaskStreamUnavailableReleasesCapabilityBeforeActivity(t *testing.T) {
	fixture := newServeNATFixture(t)
	capability, access := registerNATCapabilityForTest(t, fixture.handler, fixture.server.ID, 105)
	fixture.server.SetTaskStream(nil)
	request := capabilityRequest(capability)

	ServeNAT(&serveNATResponseWriter{}, request, &model.NAT{Common: model.Common{ID: 105, UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})

	assertNATCapabilitySlotReleased(t, fixture.handler, access)
	require.Equal(t, 0, fixture.handler.StreamCount())
	require.Empty(t, fixture.taskStream.sent)
	if request.Header.Get(agentcompatcontract.IOStreamCapabilityHeader) != "" {
		t.Fatal("capability header remained after task stream unavailable")
	}
}

func TestServeNATAgentCompatCreateQuotaFailurePreservesExistingStreams(t *testing.T) {
	fixture := newServeNATFixture(t)
	capability, access := registerNATCapabilityForTest(t, fixture.handler, fixture.server.ID, 106)
	const existingStreamCount = 40
	for index := 0; index < existingStreamCount; index++ {
		require.NoError(t, fixture.handler.CreateStreamWithPurpose("quota-existing-"+string(rune(index)), 0, fixture.server.ID, serviceRPC.PurposeNAT))
	}
	t.Cleanup(func() {
		for index := 0; index < existingStreamCount; index++ {
			_ = fixture.handler.CloseStream("quota-existing-" + string(rune(index)))
		}
	})
	request := capabilityRequest(capability)

	ServeNAT(&serveNATResponseWriter{}, request, &model.NAT{Common: model.Common{ID: 106, UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})

	assertNATCapabilitySlotReleased(t, fixture.handler, access)
	require.Equal(t, existingStreamCount, fixture.handler.StreamCount())
	require.Empty(t, fixture.taskStream.sent)
}

func TestServeNATAgentCompatPublishCancellationClosesRegistryOwnedEndpoints(t *testing.T) {
	fixture := newServeNATFixture(t)
	capability, access := registerNATCapabilityForTest(t, fixture.handler, fixture.server.ID, 107)
	connection := newServeNATConn()
	agent := &serveNATAgent{readErr: io.EOF, writeDone: make(chan struct{})}
	observerEntered := make(chan struct{})
	observerRelease := make(chan struct{})
	var observerOnce sync.Once
	fixture.handler.SetAgentCompatCapabilityPublishObserverForTest(func() {
		observerOnce.Do(func() { close(observerEntered) })
		<-observerRelease
	})
	t.Cleanup(func() { fixture.handler.SetAgentCompatCapabilityPublishObserverForTest(nil) })
	fixture.taskStream.onSend = func(task *proto.Task) error {
		var nat model.TaskNAT
		require.NoError(t, json.Unmarshal([]byte(task.Data), &nat))
		return fixture.handler.AgentConnected(nat.StreamID, agent)
	}
	request := capabilityRequest(capability)
	writer := &serveNATResponseWriter{conn: connection}
	serveDone := make(chan struct{})
	go func() {
		ServeNAT(writer, request, &model.NAT{Common: model.Common{ID: 107, UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})
		close(serveDone)
	}()
	select {
	case <-observerEntered:
	case <-serveDone:
		t.Fatal("ServeNAT returned before publish pause")
	}
	fixture.handler.CreateStreamWithPurpose("publish-replacement", 0, fixture.server.ID, serviceRPC.PurposeNAT)
	require.NoError(t, fixture.handler.CancelAgentCompatIOStreamCapability(access))
	close(observerRelease)
	completionContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case <-serveDone:
	case <-completionContext.Done():
		t.Fatal("ServeNAT did not finish after publish cancellation")
	}
	assertCapabilityReleased(t, fixture.handler, access)
	require.Equal(t, int32(1), connection.closeCount.Load())
	require.Equal(t, int32(1), agent.closeCount.Load())
	require.Equal(t, 1, fixture.handler.StreamCount())
	select {
	case <-agent.writeDone:
		t.Fatal("agent endpoint was used after publish cancellation")
	default:
	}
}

func TestServeNATAgentCompatSendTaskFailureReleasesStreamAndCapabilityBeforeWrapper(t *testing.T) {
	fixture := newServeNATFixture(t)
	capability, access := registerNATCapabilityForTest(t, fixture.handler, fixture.server.ID, 102)
	fixture.taskStream.sendErr = io.ErrClosedPipe
	connection := newServeNATConn()
	request := capabilityRequest(capability)
	request.Body = &serveNATBody{}
	writer := &serveNATResponseWriter{conn: connection}

	ServeNAT(writer, request, &model.NAT{Common: model.Common{ID: 102, UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})

	assertCapabilityReleased(t, fixture.handler, access)
	require.Equal(t, 0, fixture.handler.StreamCount())
	require.Equal(t, int32(0), connection.closeCount.Load())
}

func TestServeNATAgentCompatWrapperFailureReleasesCapabilityWithoutSecondResponse(t *testing.T) {
	fixture := newServeNATFixture(t)
	capability, access := registerNATCapabilityForTest(t, fixture.handler, fixture.server.ID, 103)
	request := capabilityRequest(capability)
	connection := newServeNATConn()
	connection.deadlineErr = io.ErrClosedPipe
	writer := &serveNATResponseWriter{conn: connection}

	ServeNAT(writer, request, &model.NAT{Common: model.Common{ID: 103, UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})

	assertCapabilityReleased(t, fixture.handler, access)
	require.Equal(t, 0, fixture.handler.StreamCount())
	require.Equal(t, 0, writer.writes)
	require.Equal(t, 0, writer.status)
	require.EqualValues(t, 1, connection.closeCount.Load())
}

func TestServeNATAgentCompatStartFailureCancelsPublishedCapabilityAndClosesEndpoints(t *testing.T) {
	fixture := newServeNATFixture(t)
	capability, access := registerNATCapabilityForTest(t, fixture.handler, fixture.server.ID, 104)
	connection := newServeNATConn()
	agent := &serveNATAgent{readErr: io.EOF, writeDone: make(chan struct{})}
	fixture.taskStream.onSend = func(task *proto.Task) error {
		var nat model.TaskNAT
		if err := json.Unmarshal([]byte(task.Data), &nat); err != nil {
			return err
		}
		return fixture.handler.AgentConnected(nat.StreamID, agent)
	}
	request := capabilityRequest(capability)
	writer := &serveNATResponseWriter{conn: connection}

	ServeNAT(writer, request, &model.NAT{Common: model.Common{ID: 104, UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})

	assertCapabilityReleased(t, fixture.handler, access)
	require.Equal(t, 0, fixture.handler.StreamCount())
	require.Equal(t, int32(1), connection.closeCount.Load())
	require.Equal(t, int32(1), agent.closeCount.Load())
}

func registerNATCapabilityForTest(t *testing.T, handler *serviceRPC.NezhaHandler, serverID, resourceID uint64) (string, serviceRPC.AgentCompatCapabilityAccess) {
	t.Helper()
	capability, err := handler.RegisterAgentCompatIOStreamCapability(context.Background(), serviceRPC.AgentCompatCapabilityRegistration{
		Owner: serviceRPC.AgentCompatCapabilityOwner{PATID: resourceID, UserID: resourceID + 1}, Purpose: serviceRPC.AgentCompatCapabilityNAT,
		TargetServerID: serverID, ResourceID: resourceID, ServerAccessAllowed: true,
	})
	require.NoError(t, err)
	parsed, err := serviceRPC.ParseAgentCompatIOStreamCapability(capability.String())
	require.NoError(t, err)
	return capability.String(), serviceRPC.AgentCompatCapabilityAccess{
		Capability: parsed, Owner: serviceRPC.AgentCompatCapabilityOwner{PATID: resourceID, UserID: resourceID + 1},
		Purpose: serviceRPC.AgentCompatCapabilityNAT, TargetServerID: serverID, ResourceID: resourceID, ServerAccessAllowed: true,
	}
}

func capabilityRequest(value string) *http.Request {
	request := &http.Request{Method: http.MethodPost, URL: &url.URL{Scheme: "http", Host: "example.test", Path: "/nat"}, Header: make(http.Header), Body: &serveNATBody{}}
	request.Header.Set(agentcompatcontract.IOStreamCapabilityHeader, value)
	return request
}

func assertCapabilityReleased(t *testing.T, handler *serviceRPC.NezhaHandler, access serviceRPC.AgentCompatCapabilityAccess) {
	t.Helper()
	_, _, err := handler.ConsumeAgentCompatNATCapabilityForProfile(access.Capability.String(), access.TargetServerID, access.ResourceID)
	require.ErrorIs(t, err, serviceRPC.ErrAgentCompatCapabilityHidden)
}

func assertNATCapabilitySlotReleased(t *testing.T, handler *serviceRPC.NezhaHandler, access serviceRPC.AgentCompatCapabilityAccess) {
	t.Helper()
	assertCapabilityReleased(t, handler, access)
	type activeCapability struct {
		capability serviceRPC.AgentCompatIOStreamCapability
		resourceID uint64
	}
	active := make([]activeCapability, 0, 16)
	for index := uint64(0); index < 16; index++ {
		capability, err := handler.RegisterAgentCompatIOStreamCapability(context.Background(), serviceRPC.AgentCompatCapabilityRegistration{
			Owner: access.Owner, Purpose: serviceRPC.AgentCompatCapabilityNAT, TargetServerID: access.TargetServerID,
			ResourceID: access.ResourceID + index + 1000, ServerAccessAllowed: true,
		})
		require.NoError(t, err)
		active = append(active, activeCapability{capability: capability, resourceID: access.ResourceID + index + 1000})
	}
	_, err := handler.RegisterAgentCompatIOStreamCapability(context.Background(), serviceRPC.AgentCompatCapabilityRegistration{
		Owner: access.Owner, Purpose: serviceRPC.AgentCompatCapabilityNAT, TargetServerID: access.TargetServerID,
		ResourceID: access.ResourceID + 2000, ServerAccessAllowed: true,
	})
	require.ErrorIs(t, err, serviceRPC.ErrAgentCompatCapabilityUnavailable)
	for _, item := range active {
		parsed, parseErr := serviceRPC.ParseAgentCompatIOStreamCapability(item.capability.String())
		require.NoError(t, parseErr)
		require.NoError(t, handler.CancelAgentCompatIOStreamCapability(serviceRPC.AgentCompatCapabilityAccess{
			Capability: parsed, Owner: access.Owner, Purpose: serviceRPC.AgentCompatCapabilityNAT,
			TargetServerID: access.TargetServerID, ResourceID: item.resourceID, ServerAccessAllowed: true,
		}))
	}
}

type nonHijackingResponseWriter struct {
	writes int
}

func (writer *nonHijackingResponseWriter) Header() http.Header { return make(http.Header) }
func (writer *nonHijackingResponseWriter) Write(data []byte) (int, error) {
	writer.writes += len(data)
	return len(data), nil
}
func (*nonHijackingResponseWriter) WriteHeader(int) {}
