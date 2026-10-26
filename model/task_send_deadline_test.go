package model

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/nezhahq/nezha/proto"
	"github.com/stretchr/testify/require"
)

type stalledTaskStream struct {
	pb.NezhaService_RequestTaskServer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	sent    atomic.Int32
}

func (stream *stalledTaskStream) Context() context.Context { return context.Background() }
func (stream *stalledTaskStream) Send(*pb.Task) error {
	stream.sent.Add(1)
	stream.once.Do(func() { close(stream.entered) })
	<-stream.release
	return nil
}

func TestTaskSendReturnsWhenAgentStopsReading(t *testing.T) {
	stream := &stalledTaskStream{entered: make(chan struct{}), release: make(chan struct{})}
	server := &Server{}
	server.SetTaskStream(stream)
	done := make(chan error, 1)
	go func() { done <- server.SendTask(&pb.Task{Type: TaskTypeKeepalive}) }()
	<-stream.entered
	returned := false
	defer func() {
		close(stream.release)
		if !returned {
			<-done
		}
	}()
	select {
	case err := <-done:
		returned = true
		require.Error(t, err, "a stalled send must return a failure, not report delivery")
	case <-time.After(1500 * time.Millisecond):
		t.Error("SendTask remained blocked beyond its delivery budget")
	}
}

func TestQueuedTasksRemainBoundedAndRecheckAdmission(t *testing.T) {
	stream := &stalledTaskStream{entered: make(chan struct{}), release: make(chan struct{})}
	server := &Server{}
	server.SetTaskStream(stream)
	require.NoError(t, server.QueueTask(&pb.Task{Type: TaskTypeKeepalive}))
	<-stream.entered
	rejected := make(chan struct{}, maxQueuedAgentTasks)
	for range maxQueuedAgentTasks {
		require.NoError(t, server.QueueTaskGuarded(&pb.Task{Type: TaskTypeCommand}, func() (func(), error) { rejected <- struct{}{}; return nil, errors.New("ownership changed") }))
	}
	require.ErrorIs(t, server.QueueTask(&pb.Task{}), ErrTaskQueueFull)
	close(stream.release)
	for range maxQueuedAgentTasks {
		select {
		case <-rejected:
		case <-time.After(time.Second):
			t.Fatal("queued task was not reauthorized")
		}
	}
	require.Eventually(t, func() bool {
		holder := server.taskStream.Load()
		holder.queueMu.Lock()
		defer holder.queueMu.Unlock()
		return !holder.queueRunning
	}, time.Second, time.Millisecond)
	require.EqualValues(t, 1, stream.sent.Load(), "rejected commands must never reach the agent")
	// No command can reach Send when its final admission rejects it.
	holder := server.taskStream.Load()
	require.ErrorIs(t, holder.sendBefore(&pb.Task{}, time.Now().Add(-time.Second), nil), context.DeadlineExceeded)
}
