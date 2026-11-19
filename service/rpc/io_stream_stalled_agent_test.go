package rpc

import (
	"io"
	"testing"
	"time"
)

// stalledEndpoint blocks in Read and Write, and Close does not interrupt them,
// like the IOStream of an agent that grants no HTTP/2 flow-control credit.
type stalledEndpoint struct {
	blocked chan struct{}
	release chan struct{}
}

func (endpoint stalledEndpoint) Read([]byte) (int, error) {
	endpoint.blocked <- struct{}{}
	<-endpoint.release
	return 0, io.EOF
}

func (endpoint stalledEndpoint) Write([]byte) (int, error) {
	<-endpoint.release
	return 0, io.ErrClosedPipe
}

func (stalledEndpoint) Close() error { return nil }

// TestStartStream_ReturnsWhenClosedWhileRelayIsBlocked verifies the caller of
// StartStream is not held hostage by the relay. CloseStream closes both
// endpoints, but an endpoint is free to keep a Read or Write blocked after
// Close (a gRPC Send waiting for flow-control credit does). If StartStream only
// waits for a copy to finish, the terminal / fm / NAT handler that called it
// never returns and never runs its own cleanup.
func TestStartStream_ReturnsWhenClosedWhileRelayIsBlocked(t *testing.T) {
	h := NewNezhaHandler()
	const id = "stalled-relay"

	if err := h.CreateStream(id, 1, 1); err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	endpoint := stalledEndpoint{blocked: make(chan struct{}, 2), release: make(chan struct{})}
	t.Cleanup(func() { close(endpoint.release) })
	if err := h.AgentConnected(id, endpoint); err != nil {
		t.Fatalf("AgentConnected: %v", err)
	}
	if err := h.UserConnected(id, endpoint); err != nil {
		t.Fatalf("UserConnected: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- h.StartStream(id, time.Second*5) }()
	for i := 0; i < 2; i++ {
		select {
		case <-endpoint.blocked:
		case <-time.After(time.Second * 5):
			t.Fatal("relay did not start copying")
		}
	}

	if err := h.CloseStream(id); err != nil {
		t.Fatalf("CloseStream: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("StartStream must report that the stream was closed under it")
		}
	case <-time.After(time.Second * 5):
		t.Fatal("StartStream did not return after its stream was closed")
	}
}
