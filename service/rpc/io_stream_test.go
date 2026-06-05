package rpc

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"
)

func TestIOStream(t *testing.T) {
	handler := NewNezhaHandler()

	const testStreamID = "ffffffff-ffff-ffff-ffff-ffffffffffff"

	handler.CreateStream(testStreamID, 0, 0)
	userIo, agentIo := newPipeReadWriter(), newPipeReadWriter()
	defer func() {
		userIo.Close()
		agentIo.Close()
	}()

	handler.AgentConnected(testStreamID, agentIo)
	handler.UserConnected(testStreamID, userIo)

	go handler.StartStream(testStreamID, time.Second*10)

	cases := [][]byte{
		{0, 9, 1, 3, 2, 9, 1, 4, 8},
		{3, 1, 3, 5, 2, 9, 5, 13, 53, 23},
		make([]byte, 1024),
		make([]byte, 1024*1024),
	}

	t.Run("WriteUserIO", func(t *testing.T) {
		for i, c := range cases {
			_, err := userIo.Write(c)
			if err != nil {
				t.Fatalf("write to userIo failed at case %d: %v", i, err)
			}

			b := make([]byte, len(c))
			n, err := agentIo.Read(b)
			if err != nil {
				t.Fatalf("read agentIo failed at case %d: %v", i, err)
			}

			if !reflect.DeepEqual(c, b[:n]) {
				t.Fatalf("expected %v, but got %v", c, b[:n])
			}
		}
	})

	t.Run("WriteAgentIO", func(t *testing.T) {
		for i, c := range cases {
			_, err := agentIo.Write(c)
			if err != nil {
				t.Fatalf("write to agentIo failed at case %d: %v", i, err)
			}

			b := make([]byte, len(c))
			n, err := userIo.Read(b)
			if err != nil {
				t.Fatalf("read userIo failed at case %d: %v", i, err)
			}

			if !reflect.DeepEqual(c, b[:n]) {
				t.Fatalf("Expected %v, but got %v", c, b[:n])
			}
		}
	})

	t.Run("WriteUserIOReadTwice", func(t *testing.T) {
		data := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		_, err := agentIo.Write(data)
		if err != nil {
			t.Fatalf("write to agentIo failed: %v", err)
		}

		b := make([]byte, len(data)/2)
		n, err := userIo.Read(b)
		if err != nil {
			t.Fatalf("read userIo failed: %v", err)
		}

		b2 := make([]byte, len(data)-n)
		_, err = userIo.Read(b2)
		if err != nil {
			t.Fatalf("read userIo failed: %v", err)
		}

		if !reflect.DeepEqual(data[:len(data)/2], b) {
			t.Fatalf("expected %v, but got %v", data[:len(data)/2], b)
		}

		if !reflect.DeepEqual(data[len(data)/2:], b2) {
			t.Fatalf("expected %v, but got %v", data[len(data)/2:], b2)
		}
	})
}

// The WebSocket stream endpoints (terminal / fm) were unbounded: an
// authenticated member could open thousands of streams, each spawning
// goroutines, a 1 MiB buffer, and an agent-side PTY, exhausting dashboard and
// agent resources (GHSA-jg62-j5h6-8mpq). CreateStream now caps concurrent
// streams per user and per server. These tests pin the caps and the
// dashboard-internal (uid==0) exemption.

// Baseline: a normal operator opening a terminal and a file-manager session
// against one server (the everyday case) must always succeed — the cap exists
// to stop floods, not to interfere with ordinary use.
func TestCreateStreamNormalUserEverydayUseSucceeds(t *testing.T) {
	h := NewNezhaHandler()
	const uid, serverID = uint64(7), uint64(1)

	if err := h.CreateStream("term", uid, serverID); err != nil {
		t.Fatalf("opening a terminal must succeed for a normal user, got %v", err)
	}
	if err := h.CreateStream("fm", uid, serverID); err != nil {
		t.Fatalf("opening a file manager alongside a terminal must succeed, got %v", err)
	}
}

// Several normal users working at the same time must not interfere: one user's
// streams do not consume another user's per-user budget.
func TestCreateStreamNormalUsersAreIndependent(t *testing.T) {
	h := NewNezhaHandler()

	for u := uint64(1); u <= 5; u++ {
		for i := 0; i < maxStreamsPerUser; i++ {
			id := fmt.Sprintf("u%d-s%d", u, i)
			if err := h.CreateStream(id, u, 100+u); err != nil {
				t.Fatalf("user %d stream %d must succeed; per-user budgets must be independent, got %v", u, i, err)
			}
		}
	}
}

func TestCreateStreamEnforcesPerUserCap(t *testing.T) {
	h := NewNezhaHandler()
	const uid = uint64(42)

	for i := 0; i < maxStreamsPerUser; i++ {
		if err := h.CreateStream(fmt.Sprintf("u-%d", i), uid, uint64(i)); err != nil {
			t.Fatalf("stream %d within the per-user cap must succeed, got %v", i, err)
		}
	}

	err := h.CreateStream("u-over", uid, 9999)
	if !errors.Is(err, ErrTooManyStreamsForUser) {
		t.Fatalf("the (maxStreamsPerUser+1)-th stream must be rejected with ErrTooManyStreamsForUser, got %v", err)
	}
}

func TestCreateStreamEnforcesPerServerCap(t *testing.T) {
	h := NewNezhaHandler()
	const serverID = uint64(7)

	for i := 0; i < maxStreamsPerServer; i++ {
		if err := h.CreateStream(fmt.Sprintf("s-%d", i), uint64(i+1), serverID); err != nil {
			t.Fatalf("stream %d within the per-server cap must succeed, got %v", i, err)
		}
	}

	err := h.CreateStream("s-over", 99999, serverID)
	if !errors.Is(err, ErrTooManyStreamsForServer) {
		t.Fatalf("the (maxStreamsPerServer+1)-th stream to one server must be rejected with ErrTooManyStreamsForServer, got %v", err)
	}
}

// Dashboard-internal streams (NAT, server transfer, MCP transfer) pass
// creatorUserID==0. They must NOT be capped per user, or those features would
// throttle themselves; but they must still count toward the per-server cap so
// no single server can be flooded regardless of the originating path.
func TestCreateStreamExemptsInternalStreamsFromPerUserCap(t *testing.T) {
	h := NewNezhaHandler()

	for i := 0; i < maxStreamsPerUser*3; i++ {
		if err := h.CreateStream(fmt.Sprintf("internal-%d", i), 0, uint64(i)); err != nil {
			t.Fatalf("internal stream %d (uid==0) must never hit the per-user cap, got %v", i, err)
		}
	}
}

func TestCreateStreamInternalStreamsStillCountTowardPerServerCap(t *testing.T) {
	h := NewNezhaHandler()
	const serverID = uint64(3)

	for i := 0; i < maxStreamsPerServer; i++ {
		if err := h.CreateStream(fmt.Sprintf("internal-s-%d", i), 0, serverID); err != nil {
			t.Fatalf("internal stream %d within the per-server cap must succeed, got %v", i, err)
		}
	}

	err := h.CreateStream("internal-s-over", 0, serverID)
	if !errors.Is(err, ErrTooManyStreamsForServer) {
		t.Fatalf("internal streams must still be subject to the per-server cap, got %v", err)
	}
}

// Closing a stream must free its slot so a user who hit the cap can open new
// streams after old ones end — otherwise normal churn would permanently lock
// a user out.
func TestCreateStreamFreesSlotAfterClose(t *testing.T) {
	h := NewNezhaHandler()
	const uid = uint64(55)

	for i := 0; i < maxStreamsPerUser; i++ {
		if err := h.CreateStream(fmt.Sprintf("c-%d", i), uid, 1); err != nil {
			t.Fatalf("setup stream %d must succeed, got %v", i, err)
		}
	}
	if err := h.CreateStream("c-over", uid, 1); !errors.Is(err, ErrTooManyStreamsForUser) {
		t.Fatalf("expected per-user cap to be hit, got %v", err)
	}

	if err := h.CloseStream("c-0"); err != nil {
		t.Fatalf("CloseStream failed: %v", err)
	}

	if err := h.CreateStream("c-after-close", uid, 1); err != nil {
		t.Fatalf("after closing one stream the user must be able to open another, got %v", err)
	}
}

func newPipeReadWriter() io.ReadWriteCloser {
	r, w := io.Pipe()
	return struct {
		io.Reader
		io.WriteCloser
	}{r, w}
}

func TestStreamOwnershipReturnsCreatorUserID(t *testing.T) {
	h := NewNezhaHandler()
	h.CreateStream("alice-stream", 100, 0)

	creator, found := h.StreamOwnership("alice-stream")
	if !found {
		t.Fatalf("expected stream to be found after CreateStream")
	}
	if creator != 100 {
		t.Fatalf("expected creator user ID 100, got %d", creator)
	}
}

func TestStreamOwnershipReturnsNotFoundForUnknownID(t *testing.T) {
	h := NewNezhaHandler()
	if _, found := h.StreamOwnership("nonexistent"); found {
		t.Fatalf("expected unknown stream id to report not-found")
	}
}

func TestStreamOwnershipPreservesPerStreamCreator(t *testing.T) {
	h := NewNezhaHandler()
	h.CreateStream("alice-stream", 100, 0)
	h.CreateStream("bob-stream", 200, 0)

	aliceCreator, _ := h.StreamOwnership("alice-stream")
	bobCreator, _ := h.StreamOwnership("bob-stream")
	if aliceCreator != 100 || bobCreator != 200 {
		t.Fatalf("expected per-stream creator IDs alice=100 bob=200, got alice=%d bob=%d",
			aliceCreator, bobCreator)
	}
}

func TestIsStreamAuthorizedForUserAllowsCreator(t *testing.T) {
	h := NewNezhaHandler()
	h.CreateStream("alice-stream", 100, 0)

	if !h.IsStreamAuthorizedForUser("alice-stream", 100, false) {
		t.Fatalf("creator must be authorized to attach to their own stream")
	}
}

func TestIsStreamAuthorizedForUserDeniesForeignMember(t *testing.T) {
	h := NewNezhaHandler()
	h.CreateStream("alice-stream", 100, 0)

	if h.IsStreamAuthorizedForUser("alice-stream", 200, false) {
		t.Fatalf("foreign member must not be authorized — session hijack would be possible")
	}
}

func TestIsStreamAuthorizedForUserAllowsAdmin(t *testing.T) {
	h := NewNezhaHandler()
	h.CreateStream("alice-stream", 100, 0)

	if !h.IsStreamAuthorizedForUser("alice-stream", 999, true) {
		t.Fatalf("admin must be authorized to attach regardless of creator")
	}
}

func TestIsStreamAuthorizedForUserDeniesUnknownStream(t *testing.T) {
	h := NewNezhaHandler()

	if h.IsStreamAuthorizedForUser("nonexistent", 100, true) {
		t.Fatalf("unknown stream id must not authorize even admin")
	}
}

// IOStream init messages begin with the magic marker ff05ff05. The inline
// check previously used && between byte inequalities, which due to short-
// circuit evaluation accepted almost every non-magic payload (any payload
// whose byte0 == 0xff was silently let through). These tests pin down the
// correct semantics: all four bytes must match exactly.
func TestIsValidIOStreamMagicAcceptsExactMagic(t *testing.T) {
	if !isValidIOStreamMagic([]byte{0xff, 0x05, 0xff, 0x05}) {
		t.Fatal("exact ff05ff05 magic must be accepted")
	}
	if !isValidIOStreamMagic([]byte{0xff, 0x05, 0xff, 0x05, 'p', 'a', 'y', 'l', 'o', 'a', 'd'}) {
		t.Fatal("ff05ff05 followed by payload must be accepted")
	}
}

func TestIsValidIOStreamMagicRejectsShortData(t *testing.T) {
	if isValidIOStreamMagic([]byte{}) {
		t.Fatal("empty data must be rejected")
	}
	if isValidIOStreamMagic([]byte{0xff, 0x05, 0xff}) {
		t.Fatal("3-byte payload must be rejected")
	}
}

// Agent-side stream authorization is the dual of IsStreamAuthorizedForUser:
// only the server the dashboard selected when CreateStream was called may
// attach via IOStream(). Without it, any authenticated agent that learns an
// active streamId (task-stream observation, leaked logs) can race in and
// serve a terminal/fm/NAT session originally addressed to a different
// server — a session-hijack RCE intermediation primitive.
func TestIsStreamAuthorizedForAgentAllowsBoundServer(t *testing.T) {
	h := NewNezhaHandler()
	h.CreateStream("terminal-for-server-100", 1, 100)

	if !h.IsStreamAuthorizedForAgent("terminal-for-server-100", 100) {
		t.Fatalf("the bound target server must be authorized to attach")
	}
}

func TestIsStreamAuthorizedForAgentDeniesForeignServer(t *testing.T) {
	h := NewNezhaHandler()
	h.CreateStream("terminal-for-server-100", 1, 100)

	if h.IsStreamAuthorizedForAgent("terminal-for-server-100", 200) {
		t.Fatalf("a foreign agent must not be able to attach — session hijack would be possible")
	}
}

func TestIsStreamAuthorizedForAgentDeniesUnboundStream(t *testing.T) {
	h := NewNezhaHandler()
	// targetServerID == 0 means the stream was created without a bound agent
	// — no agent should be allowed to attach.
	h.CreateStream("unbound-stream", 1, 0)

	if h.IsStreamAuthorizedForAgent("unbound-stream", 100) {
		t.Fatalf("unbound stream must not authorize any agent")
	}
	if h.IsStreamAuthorizedForAgent("unbound-stream", 0) {
		t.Fatalf("unbound stream must not authorize a zero clientID either")
	}
}

func TestIsStreamAuthorizedForAgentDeniesUnknownStreamID(t *testing.T) {
	h := NewNezhaHandler()

	if h.IsStreamAuthorizedForAgent("nonexistent", 100) {
		t.Fatalf("unknown stream id must not authorize any agent")
	}
}

func TestIsValidIOStreamMagicRejectsPartialOrWrongMagic(t *testing.T) {
	// Each case has at least one byte that does NOT match the magic. The
	// previous && short-circuit bug let cases like {0xff, 0, 0, 0} pass
	// because byte0 alone matched. Correct semantics: any single byte off
	// → reject.
	cases := [][]byte{
		{0x00, 0x00, 0x00, 0x00},
		{0xff, 0x00, 0x00, 0x00},
		{0x00, 0x05, 0x00, 0x00},
		{0x00, 0x00, 0xff, 0x00},
		{0x00, 0x00, 0x00, 0x05},
		{0xff, 0x05, 0xff, 0x00},
		{0xff, 0x05, 0x00, 0x05},
		{0xff, 0x00, 0xff, 0x05},
		{0x00, 0x05, 0xff, 0x05},
		{0xff, 0xff, 0xff, 0xff},
	}
	for _, c := range cases {
		if isValidIOStreamMagic(c) {
			t.Fatalf("non-magic payload %v must be rejected (regression: && short-circuit bug)", c)
		}
	}
}
