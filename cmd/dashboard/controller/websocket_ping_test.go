package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type pingWriterFake struct {
	mu            sync.Mutex
	writeCalls    int
	writeErr      error
	writeStarted  chan struct{}
	continueWrite chan struct{}
}

func (writer *pingWriterFake) Close() error { return nil }

type permanentlyBlockedPingWriter struct {
	writeStarted    chan struct{}
	transportClosed chan struct{}
}

func (writer *permanentlyBlockedPingWriter) WriteMessage(int, []byte) error {
	close(writer.writeStarted)
	<-writer.transportClosed
	return errors.New("transport closed")
}

func (writer *permanentlyBlockedPingWriter) Close() error {
	close(writer.transportClosed)
	return nil
}

func (writer *pingWriterFake) WriteMessage(int, []byte) error {
	writer.mu.Lock()
	writer.writeCalls++
	writer.mu.Unlock()
	if writer.writeStarted != nil {
		close(writer.writeStarted)
		<-writer.continueWrite
	}
	return writer.writeErr
}

func (writer *pingWriterFake) calls() int {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writeCalls
}

func TestWebsocketPingLoop_stopsAndJoinsWithoutWritingAfterStop(t *testing.T) {
	// Given
	ticks := make(chan time.Time, 1)
	writer := &pingWriterFake{writeStarted: make(chan struct{}), continueWrite: make(chan struct{})}
	stop := startWebsocketPing(context.Background(), ticks, writer)

	// When
	ticks <- time.Time{}
	<-writer.writeStarted
	close(writer.continueWrite)
	stop()
	ticks <- time.Time{}

	// Then
	require.Equal(t, 1, writer.calls())
}

func TestWebsocketPingLoop_exitsOnWriteError(t *testing.T) {
	// Given
	ticks := make(chan time.Time, 1)
	writer := &pingWriterFake{writeErr: errors.New("closed")}
	done := make(chan error, 1)
	go func() { done <- websocketPingLoop(context.Background(), ticks, writer) }()

	// When
	ticks <- time.Time{}

	// Then
	require.Error(t, <-done)
	ticks <- time.Time{}
	require.Equal(t, 1, writer.calls())
}

func TestWebsocketPingLoop_cleanupOverlapsTickAndJoinsWriter(t *testing.T) {
	// Given
	ticks := make(chan time.Time, 1)
	writer := &pingWriterFake{
		writeStarted:  make(chan struct{}),
		continueWrite: make(chan struct{}),
	}
	stop := startWebsocketPing(context.Background(), ticks, writer)
	ticks <- time.Time{}
	<-writer.writeStarted

	// When
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		require.Fail(t, "ping worker stop returned before the in-flight write joined")
	default:
	}
	close(writer.continueWrite)
	<-stopped
	ticks <- time.Time{}

	// Then
	require.Equal(t, 1, writer.calls())
}

func TestWebsocketPingStop_unblocksPermanentlyBlockedWriteBeforeJoin(t *testing.T) {
	// Given
	ticks := make(chan time.Time, 1)
	writer := &permanentlyBlockedPingWriter{
		writeStarted:    make(chan struct{}),
		transportClosed: make(chan struct{}),
	}
	stop := startWebsocketPing(context.Background(), ticks, writer)
	ticks <- time.Time{}
	<-writer.writeStarted

	// When
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()

	// Then
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	select {
	case <-writer.transportClosed:
	case <-deadline.C:
		require.Fail(t, "stop did not close the blocked ping transport")
	}
	select {
	case <-stopped:
	case <-deadline.C:
		require.Fail(t, "stop did not join after closing the blocked ping transport")
	}
}

var _ websocketPingWriter = (*pingWriterFake)(nil)

// A failed ping is the only sign that the browser is gone while the relay is
// blocked on the agent, so it has to end the session instead of being dropped.
func TestStartWebsocketPing_closesConnectionWhenPingFails(t *testing.T) {
	// Given
	ticks := make(chan time.Time, 1)
	closed := make(chan struct{})
	writer := &pingWriterFake{writeErr: errors.New("broken pipe")}
	stop := startWebsocketPing(context.Background(), ticks, newWebsocketPingTransport(writer, func() error {
		close(closed)
		return nil
	}))

	// When
	ticks <- time.Time{}

	// Then
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("connection left open after a failed ping")
	}
	stop() // must not close a second time
}
