package rpc

import (
	"errors"
	"testing"
)

// A second WebSocket opened with the same session ID used to replace the first
// connection and start a second pair of relay goroutines over the same buffer.
// A large Agent frame then indexed past the slice and took the whole dashboard
// process down.
func TestUserConnectedRejectsASecondConnectionToTheSameStream(t *testing.T) {
	handler := NewNezhaHandler()
	const streamID = "11111111-1111-1111-1111-111111111111"
	handler.CreateStream(streamID, 100, 0)
	first, second := newPipeReadWriter(), newPipeReadWriter()
	defer first.Close()
	defer second.Close()

	if err := handler.UserConnected(streamID, first); err != nil {
		t.Fatalf("first connection: %v", err)
	}
	err := handler.UserConnected(streamID, second)

	if !errors.Is(err, ErrUserStreamAlreadyConnected) {
		t.Fatalf("second connection err = %v, want ErrUserStreamAlreadyConnected", err)
	}
	stream, getErr := handler.GetStream(streamID)
	if getErr != nil {
		t.Fatalf("GetStream: %v", getErr)
	}
	attached, _ := handler.streamEndpoints(stream)
	if attached != first {
		t.Fatal("the second connection replaced the first one")
	}
}
