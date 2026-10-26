package rpc

import (
	"context"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

type transferResultTaskStream struct {
	*fakeTaskStream
	ctx     context.Context
	ready   chan struct{}
	results chan *pb.TaskResult
}

func (s *transferResultTaskStream) Context() context.Context { return s.ctx }
func (s *transferResultTaskStream) Recv() (*pb.TaskResult, error) {
	select {
	case s.ready <- struct{}{}:
	default:
	}
	select {
	case result := <-s.results:
		return result, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func TestCallAgentAllowsTransferFailureBeforeMCPResult(t *testing.T) {
	server := &model.Server{Common: model.Common{ID: 9, UserID: 200}, UUID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", Name: "transfer-subject"}
	setupApplyConfigAuthzFixture(t, []*model.Server{server}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember, AgentSecret: "source-secret"}, 300: {Role: model.RoleMember, AgentSecret: "destination-secret"},
	}, map[string]uint64{"source-secret": 200, "destination-secret": 300})
	transfer := initiateAndRegisterPendingTransfer(t, server.ID, 200, 300, 1)
	streamContext, stopStream := context.WithCancel(metadata.NewIncomingContext(context.Background(), metadata.Pairs("client_secret", "source-secret", "client_uuid", server.UUID)))
	stream := &transferResultTaskStream{fakeTaskStream: newFakeStream(), ctx: streamContext, ready: make(chan struct{}, 1), results: make(chan *pb.TaskResult, 2)}
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- NewNezhaHandler().RequestTask(stream) }()
	defer func() {
		stopStream()
		select {
		case err := <-receiverDone:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(3 * time.Second):
			t.Error("RequestTask did not stop")
		}
	}()
	select {
	case <-stream.ready:
	case <-time.After(time.Second):
		t.Fatal("RequestTask did not attach")
	}
	callContext, stopCall := context.WithCancel(singleton.WithServerControlIdentity(context.Background(), 1, true))
	defer stopCall()
	type outcome struct {
		data string
		err  error
	}
	callDone := make(chan outcome, 1)
	go func() {
		data, err := CallAgent(callContext, server.ID, model.TaskTypeExec, model.ExecRequest{Cmd: "local simulation only"}, 2*time.Second)
		callDone <- outcome{string(data), err}
	}()
	var task *pb.Task
	deadline := time.After(time.Second)
	for task == nil {
		select {
		case sent := <-stream.sent:
			if sent.Type == model.TaskTypeExec {
				task = sent
			}
		case <-deadline:
			stopCall()
			<-callDone
			t.Fatal("MCP task was not dispatched")
		}
	}
	// RequestTask must process rollback before it can route the next MCP result.
	stream.results <- &pb.TaskResult{Id: transfer.ID, Type: model.TaskTypeServerTransferApply, Successful: false, Data: "configuration rejected"}
	stream.results <- &pb.TaskResult{Id: task.Id, Type: task.Type, Successful: true, Data: `{"exit_code":0}`}
	got := <-callDone
	require.NoError(t, got.err, "waiting for a reply must not block the receiver's ownership transition")
	require.JSONEq(t, `{"exit_code":0}`, got.data)
	var stored model.ServerTransfer
	require.NoError(t, singleton.DB.First(&stored, transfer.ID).Error)
	require.Equal(t, model.ServerTransferStatusFailed, stored.Status)
	require.Equal(t, uint64(200), server.GetUserID())
}

type stalledTransferResultTaskStream struct {
	*transferResultTaskStream
	sending      chan struct{}
	sendReturned chan struct{}
}

func (s *stalledTransferResultTaskStream) Send(task *pb.Task) error {
	if task.Type != model.TaskTypeExec {
		return s.fakeTaskStream.Send(task)
	}
	close(s.sending)
	<-s.ctx.Done()
	close(s.sendReturned)
	return s.ctx.Err()
}

func TestRequestTaskCancelsStalledSendDuringTransferRollback(t *testing.T) {
	server := &model.Server{Common: model.Common{ID: 9, UserID: 200}, UUID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", Name: "blocked-transfer-subject"}
	setupApplyConfigAuthzFixture(t, []*model.Server{server}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember, AgentSecret: "source-secret"}, 300: {Role: model.RoleMember, AgentSecret: "destination-secret"},
	}, map[string]uint64{"source-secret": 200, "destination-secret": 300})
	transfer := initiateAndRegisterPendingTransfer(t, server.ID, 200, 300, 1)
	streamContext, stopStream := context.WithCancel(metadata.NewIncomingContext(context.Background(), metadata.Pairs("client_secret", "source-secret", "client_uuid", server.UUID)))
	stream := &stalledTransferResultTaskStream{transferResultTaskStream: &transferResultTaskStream{fakeTaskStream: newFakeStream(), ctx: streamContext, ready: make(chan struct{}, 1), results: make(chan *pb.TaskResult, 1)}, sending: make(chan struct{}), sendReturned: make(chan struct{})}
	receiverDone := make(chan error, 1)
	go func() {
		err := NewNezhaHandler().RequestTask(stream)
		// gRPC cancels the transport when the handler returns. No manual Send release.
		stopStream()
		receiverDone <- err
	}()
	select {
	case <-stream.ready:
	case <-time.After(time.Second):
		stopStream()
		<-receiverDone
		t.Fatal("receiver did not attach")
	}
	callDone := make(chan error, 1)
	go func() {
		_, err := CallAgent(singleton.WithServerControlIdentity(context.Background(), 1, true), server.ID, model.TaskTypeExec, model.ExecRequest{}, 5*time.Second)
		callDone <- err
	}()
	<-stream.sending
	stream.results <- &pb.TaskResult{Id: transfer.ID, Type: model.TaskTypeServerTransferApply, Successful: false, Data: "configuration rejected"}
	var receiverErr error
	select {
	case receiverErr = <-receiverDone:
	case <-time.After(3 * time.Second):
		t.Error("handler could not observe send failure while rollback waited for the control lease")
		stopStream() // Failure cleanup only; this must not be necessary for success.
		receiverErr = <-receiverDone
	}
	require.Error(t, receiverErr)
	require.Error(t, <-callDone)
	<-stream.sendReturned
	require.Eventually(t, func() bool {
		var stored model.ServerTransfer
		return singleton.DB.First(&stored, transfer.ID).Error == nil && stored.Status == model.ServerTransferStatusFailed
	}, time.Second, time.Millisecond)
	// The terminal row is written before rollback finishes; synchronize with its
	// writer lease before restoring global fixtures, including under -race.
	release, _, err := singleton.BeginServerControl(server.ID, 1, true)
	require.NoError(t, err)
	release()
	require.Equal(t, uint64(200), server.GetUserID())
}
