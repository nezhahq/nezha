package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

type fakeTaskStream struct {
	sent  chan *pb.Task
	delay time.Duration
	err   error
}

func newFakeStream() *fakeTaskStream {
	return &fakeTaskStream{sent: make(chan *pb.Task, 4)}
}

func (s *fakeTaskStream) Send(t *pb.Task) error {
	if s.err != nil {
		return s.err
	}
	s.sent <- t
	return nil
}
func (s *fakeTaskStream) Recv() (*pb.TaskResult, error) { return nil, context.Canceled }
func (s *fakeTaskStream) SetHeader(metadata.MD) error   { return nil }
func (s *fakeTaskStream) SendHeader(metadata.MD) error  { return nil }
func (s *fakeTaskStream) SetTrailer(metadata.MD)        {}
func (s *fakeTaskStream) Context() context.Context      { return context.Background() }
func (s *fakeTaskStream) SendMsg(any) error             { return nil }
func (s *fakeTaskStream) RecvMsg(any) error             { return context.Canceled }

func installFakeServer(t *testing.T, id uint64, stream pb.NezhaService_RequestTaskServer) func() {
	t.Helper()
	original := singleton.ServerShared
	sc := singleton.NewEmptyServerClassForTest()
	srv := &model.Server{}
	srv.ID = id
	srv.SetTaskStream(stream)
	sc.InsertForTest(srv)
	singleton.ServerShared = sc
	return func() { singleton.ServerShared = original }
}

func TestCallAgent_RejectsNonMCPType(t *testing.T) {
	_, err := CallAgent(context.Background(), 1, model.TaskTypeCommand, struct{}{}, time.Second)
	if err == nil {
		t.Fatalf("expected error for non-MCP type")
	}
}

func TestCallAgent_OfflineWhenNoStream(t *testing.T) {
	original := singleton.ServerShared
	sc := singleton.NewEmptyServerClassForTest()
	srv := &model.Server{}
	srv.ID = 7
	sc.InsertForTest(srv)
	singleton.ServerShared = sc
	t.Cleanup(func() { singleton.ServerShared = original })

	_, err := CallAgent(context.Background(), 7, model.TaskTypeExec, struct{}{}, time.Second)
	if !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("expected ErrAgentOffline, got %v", err)
	}
}

func TestCallAgent_HappyPath_DelivlersResultByTaskID(t *testing.T) {
	stream := newFakeStream()
	cleanup := installFakeServer(t, 42, stream)
	defer cleanup()

	resultPayload, _ := json.Marshal(model.ExecResult{ExitCode: 0, Stdout: "hello"})

	var captured atomic.Uint64
	done := make(chan struct{})
	go func() {
		sent := <-stream.sent
		captured.Store(sent.GetId())
		deliverMCPResult(&pb.TaskResult{
			Id:         sent.GetId(),
			Type:       model.TaskTypeExec,
			Data:       string(resultPayload),
			Successful: true,
		})
		close(done)
	}()

	raw, err := CallAgent(context.Background(), 42, model.TaskTypeExec, model.ExecRequest{Cmd: "x"}, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	<-done
	if captured.Load() == 0 {
		t.Fatalf("task id never captured")
	}
	var got model.ExecResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("bad result json: %v", err)
	}
	if got.Stdout != "hello" {
		t.Fatalf("payload not propagated, got %+v", got)
	}
}

// A Task larger than the agent's gRPC receive limit must not be sent: escaping
// alone can grow accepted arguments past it.
func TestCallAgent_RejectsOversizedTaskBeforeSend(t *testing.T) {
	stream := newFakeStream()
	cleanup := installFakeServer(t, 4500, stream)
	defer cleanup()

	content := strings.Repeat("<", maxMCPTaskDataBytes/6+1)
	_, err := CallAgent(context.Background(), 4500, model.TaskTypeFsWrite, model.FsWriteRequest{Path: "/x", Content: content}, time.Second)
	if !errors.Is(err, ErrMCPTaskTooLarge) {
		t.Fatalf("expected ErrMCPTaskTooLarge, got %v", err)
	}
	select {
	case <-stream.sent:
		t.Fatal("oversized task must not reach the agent stream")
	default:
	}
}

func TestCallAgent_Timeout(t *testing.T) {
	stream := newFakeStream()
	cleanup := installFakeServer(t, 43, stream)
	defer cleanup()

	go func() { <-stream.sent }()

	_, err := CallAgent(context.Background(), 43, model.TaskTypeFsRead, model.FsReadRequest{Path: "/x"}, 50*time.Millisecond)
	if !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("expected ErrAgentTimeout, got %v", err)
	}
}

func TestCallAgent_LateResultIsDropped(t *testing.T) {
	stream := newFakeStream()
	cleanup := installFakeServer(t, 44, stream)
	defer cleanup()

	var taskID uint64
	got := make(chan struct{})
	go func() {
		sent := <-stream.sent
		taskID = sent.GetId()
		close(got)
	}()

	_, err := CallAgent(context.Background(), 44, model.TaskTypeFsDelete, model.FsDeleteRequest{Path: "/x"}, 50*time.Millisecond)
	if !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("expected timeout")
	}
	<-got

	deliverMCPResult(&pb.TaskResult{Id: taskID, Type: model.TaskTypeFsDelete, Successful: true, Data: "{}"})
	if _, ok := mcpInflight.Load(taskID); ok {
		t.Fatalf("inflight entry must be cleaned up after timeout")
	}
}

func TestCallAgent_UnsuccessfulIsError(t *testing.T) {
	stream := newFakeStream()
	cleanup := installFakeServer(t, 45, stream)
	defer cleanup()

	go func() {
		sent := <-stream.sent
		deliverMCPResult(&pb.TaskResult{
			Id:         sent.GetId(),
			Type:       sent.GetType(),
			Successful: false,
			Data:       "agent says nope",
		})
	}()

	_, err := CallAgent(context.Background(), 45, model.TaskTypeFsWrite, model.FsWriteRequest{Path: "/x", Content: "y"}, time.Second)
	if err == nil || err.Error() != "agent says nope" {
		t.Fatalf("expected agent error message, got %v", err)
	}
}
