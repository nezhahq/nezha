package model

import (
	"context"
	"errors"
	"log"
	"time"

	pb "github.com/nezhahq/nezha/proto"
	protobuf "google.golang.org/protobuf/proto"
)

const agentTaskSendTimeout = time.Second
const maxQueuedAgentTasks = 16

var ErrTaskStreamStalled = errors.New("agent task stream stalled; reconnect required")
var ErrTaskQueueFull = errors.New("agent task queue is full")

type queuedAgentTask struct {
	task     *pb.Task
	deadline time.Time
	admit    func() (func(), error)
}

func (s *Server) TaskStreamFailure(stream pb.NezhaService_RequestTaskServer) <-chan struct{} {
	holder := s.taskStream.Load()
	if holder == nil || holder.s != stream {
		failed := make(chan struct{})
		close(failed)
		return failed
	}
	return holder.failed
}

func (holder *taskStreamHolder) fail() { holder.failOnce.Do(func() { close(holder.failed) }) }

func (holder *taskStreamHolder) sendBefore(task *pb.Task, deadline time.Time, admit func() (func(), error)) error {
	if !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case holder.sendPermit <- struct{}{}:
	case <-holder.failed:
		return ErrTaskStreamStalled
	case <-holder.s.Context().Done():
		return holder.s.Context().Err()
	case <-timer.C:
		return context.DeadlineExceeded
	}
	// Never start queued work after expiry or after another send condemned this stream.
	select {
	case <-holder.failed:
		<-holder.sendPermit
		return ErrTaskStreamStalled
	default:
	}
	if !time.Now().Before(deadline) {
		<-holder.sendPermit
		return context.DeadlineExceeded
	}
	done := make(chan error, 1)
	go func() {
		var err error
		if admit != nil {
			var release func()
			release, err = admit()
			if err == nil {
				defer release()
			}
		}
		if err == nil {
			select {
			case <-holder.failed:
				err = ErrTaskStreamStalled
			default:
				if time.Now().Before(deadline) {
					err = holder.s.Send(task)
				} else {
					err = context.DeadlineExceeded
				}
			}
		}
		<-holder.sendPermit
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-holder.s.Context().Done():
		holder.fail()
		return holder.s.Context().Err()
	case <-timer.C:
		// Keep the permit held until Send exits. No second blocked sender can accumulate.
		// RequestTask observes failed and returns, which cancels the underlying transport.
		holder.fail()
		return ErrTaskStreamStalled
	}
}

// QueueTask isolates global probe/keepalive/cron fanout from a stalled agent.
// Each connection has one draining worker and a bounded queue; idle workers exit.
func (s *Server) QueueTask(task *pb.Task) error {
	return s.QueueTaskGuarded(task, nil)
}

// QueueTaskGuarded rechecks authorization after waiting for the send permit.
// The lease remains held until the underlying send exits, including timeout cleanup.
func (s *Server) QueueTaskGuarded(task *pb.Task, admit func() (func(), error)) error {
	holder := s.taskStream.Load()
	if holder == nil {
		return ErrTaskStreamOffline
	}
	select {
	case <-holder.failed:
		return ErrTaskStreamStalled
	default:
	}
	holder.queueMu.Lock()
	if len(holder.queue) >= maxQueuedAgentTasks {
		holder.queueMu.Unlock()
		return ErrTaskQueueFull
	}
	holder.queue = append(holder.queue, queuedAgentTask{protobuf.Clone(task).(*pb.Task), time.Now().Add(agentTaskSendTimeout), admit})
	if !holder.queueRunning {
		holder.queueRunning = true
		go holder.drainTasks()
	}
	holder.queueMu.Unlock()
	return nil
}

func (holder *taskStreamHolder) drainTasks() {
	for {
		holder.queueMu.Lock()
		if len(holder.queue) == 0 {
			holder.queueRunning = false
			holder.queueMu.Unlock()
			return
		}
		next := holder.queue[0]
		holder.queue[0] = queuedAgentTask{}
		holder.queue = holder.queue[1:]
		holder.queueMu.Unlock()
		if err := holder.sendBefore(next.task, next.deadline, next.admit); err != nil {
			log.Printf("NEZHA>> agent task delivery failed: %v", err)
		}
	}
}
