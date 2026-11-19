package controller

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// blockedResponseWriter models an HTTP client whose connection no longer
// accepts bytes and whose transport does not act on the write deadline (a
// stalled HTTP/2 connection): Write returns only once the client reads again.
type blockedResponseWriter struct {
	*countingDiscardWriter
	writeEntered     chan struct{}
	writeEnteredOnce sync.Once
	clientReads      chan struct{}
}

func (w *blockedResponseWriter) Write(p []byte) (int, error) {
	w.writeEnteredOnce.Do(func() { close(w.writeEntered) })
	<-w.clientReads
	return len(p), nil
}

// The download spool slots are shared by all users, so a download must give its
// slot and tmpfile back when the transfer context ends (deadline, revoked PAT),
// even while its body write is still blocked, and must not resume afterwards.
func TestRelayDownloadFrames_ReleasesSpoolSlotWhenContextEndsWhileWriteIsBlocked(t *testing.T) {
	// More than one io.Copy buffer, so the relay has to read the spool again
	// after the blocked write.
	payload := bytes.Repeat([]byte{'x'}, 1<<20)
	var src bytes.Buffer
	writeChunkFrame(&src, payload)
	writeOKFrame(&src, uint64(len(payload)))

	sink := &blockedResponseWriter{countingDiscardWriter: newCountingDiscardWriter(),
		writeEntered: make(chan struct{}), clientReads: make(chan struct{})}
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		returned <- relayDownloadFrames(ctx, c, &fixedSizeFrameStream{buf: src}, int64(len(payload)), nil)
	}()

	select {
	case <-sink.writeEntered:
	case err := <-returned:
		t.Fatalf("the relay returned before writing the body: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not start writing the body")
	}
	require.Len(t, downloadSpoolSlots, 1, "a download in progress holds a slot")

	cancel()
	require.Eventually(t, func() bool { return len(downloadSpoolSlots) == 0 }, 5*time.Second, 10*time.Millisecond,
		"the slot is still held after the transfer context ended")
	select {
	case err := <-returned:
		t.Fatalf("the relay was expected to be blocked in Write, it returned %v", err)
	default:
	}

	close(sink.clientReads)
	require.Error(t, <-returned, "the relay must not keep serving from a released spool")
	require.Empty(t, downloadSpoolSlots, "the slot must be released exactly once")
}
