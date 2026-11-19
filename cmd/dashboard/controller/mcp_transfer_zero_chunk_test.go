package controller

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
)

// writeZeroChunkFrame emits a well-formed NZTC chunk header that declares a
// zero-length payload. A malicious or buggy agent can emit an unbounded run of
// these: each frame is syntactically valid but carries no data, so a relay
// that treats them as no-op `continue` never makes progress toward `remaining`
// and never reaches the final NZTO frame — pinning a dashboard goroutine, gRPC
// stream and spool tmpfile until the client disconnects.
func writeZeroChunkFrame(out *bytes.Buffer) {
	writeChunkFrame(out, nil)
}

// A download that declares size > 0 but then streams zero-length NZTC frames
// must be rejected as a protocol violation, not relayed forever. The relay
// must not accept a zero-length data frame while it still expects bytes.
func TestRelayDownloadFrames_RejectsZeroLengthDataFrames(t *testing.T) {
	const size = int64(64)

	var src bytes.Buffer
	// A burst of zero-length chunk frames. With the buggy `continue`, the
	// loop consumes all of these without decrementing `remaining`; once the
	// buffer drains it hits EOF on the next ReadFull and returns a *bad
	// gateway* error — but against a real (blocking) stream the same code
	// path loops forever. We assert the relay rejects the zero-length frame
	// the moment it sees one, before draining a long run of them.
	for i := 0; i < 1000; i++ {
		writeZeroChunkFrame(&src)
	}
	// Even if a valid chunk + final frame follow, the relay must already have
	// failed on the first zero-length data frame.
	writeChunkFrame(&src, bytes.Repeat([]byte{'x'}, int(size)))
	writeOKFrame(&src, uint64(size))

	stream := &fixedSizeFrameStream{buf: src}

	sink := newCountingDiscardWriter()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(sink)

	err := relayDownloadFrames(context.Background(), c, stream, size, nil)
	if err == nil {
		t.Fatalf("relayDownloadFrames accepted a stream of zero-length NZTC frames; a zero-length data frame while remaining>0 must be rejected to avoid an unbounded relay loop")
	}
	if sink.status == 0 || sink.status == http.StatusOK {
		t.Fatalf("a rejected transfer must set a non-200 status, got %d", sink.status)
	}
}

// Sanity: a single zero-length leading frame is just as invalid; the relay
// must not silently swallow it as progress.
func TestRelayDownloadFrames_SingleZeroLengthFrameRejected(t *testing.T) {
	const size = int64(8)

	var src bytes.Buffer
	writeZeroChunkFrame(&src)
	writeChunkFrame(&src, bytes.Repeat([]byte{'y'}, int(size)))
	writeOKFrame(&src, uint64(size))

	stream := &fixedSizeFrameStream{buf: src}
	sink := newCountingDiscardWriter()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(sink)

	if err := relayDownloadFrames(context.Background(), c, stream, size, nil); err == nil {
		t.Fatalf("relayDownloadFrames must reject a zero-length data frame while remaining>0")
	}
	_ = model.MCPFsXferMagicChunk
}
