package rpc

import (
	"runtime"
	"testing"
	"time"
)

// settleGoroutines lets transient goroutines wind down so the count reflects
// only durable leaks, not in-flight teardown.
func settleGoroutines() int {
	var n int
	for i := 0; i < 50; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// TestStartStream_NoGoroutineLeakAfterClose verifies the bidirectional relay in
// StartStream does not strand a goroutine. StartStream launches two
// io.CopyBuffer goroutines (user<-agent and agent<-user) but returns after the
// first one finishes. The second goroutine stays blocked in CopyBuffer until
// its endpoints are closed. CloseStream closes both endpoints, which must
// unblock and drain that second goroutine. If it doesn't, every terminal / fm /
// NAT session leaks one goroutine for the lifetime of the dashboard.
func TestStartStream_NoGoroutineLeakAfterClose(t *testing.T) {
	base := settleGoroutines()

	const n = 20
	for i := 0; i < n; i++ {
		h := NewNezhaHandler()
		const id = "leak-stream"

		if err := h.CreateStream(id, 1, 1); err != nil {
			t.Fatalf("CreateStream: %v", err)
		}

		userIo, agentIo := newPipeReadWriter(), newPipeReadWriter()
		h.AgentConnected(id, agentIo)
		h.UserConnected(id, userIo)

		done := make(chan struct{})
		go func() {
			_ = h.StartStream(id, time.Second*5)
			close(done)
		}()

		// Close one endpoint so the first CopyBuffer returns and StartStream
		// unblocks, mirroring a peer disconnect.
		time.Sleep(10 * time.Millisecond)
		userIo.Close()
		<-done

		// The caller's defer CloseStream closes both endpoints, which must
		// drain the still-blocked second copy goroutine.
		_ = h.CloseStream(id)
		agentIo.Close()
	}

	after := settleGoroutines()
	if grew := after - base; grew > 2 {
		t.Fatalf("goroutine leak in StartStream relay: ran %d streams, goroutines grew by %d (base=%d after=%d)",
			n, grew, base, after)
	}
}
