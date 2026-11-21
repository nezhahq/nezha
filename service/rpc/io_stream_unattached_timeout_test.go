package rpc

import (
	"testing"
	"time"
)

func streamExists(handler *NezhaHandler, streamID string) bool {
	_, err := handler.GetStream(streamID)
	return err == nil
}

// A stream created for a terminal or file session whose WebSocket never arrives
// used to stay forever, holding its quota slot and the Agent's shell.
func TestCloseStreamUnlessUserConnects_ReleasesAStreamNobodyAttachedTo(t *testing.T) {
	handler := NewNezhaHandler()
	const streamID = "22222222-2222-2222-2222-222222222222"
	if err := handler.CreateStream(streamID, 100, 1); err != nil {
		t.Fatalf("CreateStream: %v", err)
	}

	handler.CloseStreamUnlessUserConnects(streamID, 30*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for streamExists(handler, streamID) {
		if time.Now().After(deadline) {
			t.Fatal("the unattached stream was never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := handler.CreateStream("33333333-3333-3333-3333-333333333333", 100, 1); err != nil {
		t.Fatalf("a new stream after the release: %v", err)
	}
}

func TestCloseStreamUnlessUserConnects_KeepsAStreamTheUserAttachedTo(t *testing.T) {
	handler := NewNezhaHandler()
	const streamID = "44444444-4444-4444-4444-444444444444"
	if err := handler.CreateStream(streamID, 100, 1); err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	userConnection := newPipeReadWriter()
	defer userConnection.Close()

	handler.CloseStreamUnlessUserConnects(streamID, 30*time.Millisecond)
	if err := handler.UserConnected(streamID, userConnection); err != nil {
		t.Fatalf("UserConnected: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	if !streamExists(handler, streamID) {
		t.Fatal("a stream with its user attached was closed by the unattached timeout")
	}
}
