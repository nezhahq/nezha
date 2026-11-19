package controller

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
)

func relayDownloadFramesRecorded(size int64, src bytes.Buffer) (*httptest.ResponseRecorder, error) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/mcp/download/x", nil)
	err := relayDownloadFrames(context.Background(), c, &fixedSizeFrameStream{buf: src}, size, nil)
	return w, err
}

// A chunk length of 1<<63 or more is negative once narrowed to int64. It must
// be refused as oversent, not accepted as a frame that copies nothing: that is
// the zero-progress frame of mcp_transfer_zero_chunk_test.go in another
// spelling, and the number of such frames is not bounded by the declared size.
func TestRelayDownloadFrames_RejectsChunkLengthNegativeAsInt64(t *testing.T) {
	const size = int64(64)

	for _, length := range []uint64{1 << 63, 1<<63 + 1, 1<<64 - 1} {
		var src bytes.Buffer
		for i := 0; i < 1000; i++ {
			src.Write(model.MCPFsXferMagicChunk)
			var sz [8]byte
			binary.BigEndian.PutUint64(sz[:], length)
			src.Write(sz[:])
		}
		// Even if a valid chunk + final frame follow, the relay must already
		// have failed on the first frame.
		writeChunkFrame(&src, bytes.Repeat([]byte{'x'}, int(size)))
		writeOKFrame(&src, uint64(size))

		w, err := relayDownloadFramesRecorded(size, src)
		if err == nil {
			t.Fatalf("relayDownloadFrames accepted NZTC frames of length %#x", length)
		}
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "oversent") {
			t.Fatalf("length %#x: want 502 oversent, got %d %q", length, w.Code, w.Body.String())
		}
	}
}

// The agent chooses the chunk length, so a payload cut into one-byte NZTC
// frames reaches the spool as one-byte Writes. They must be gathered in the
// spool's buffer instead of costing a write(2) on the tmpfile each.
func TestTransferSpool_BuffersSmallWrites(t *testing.T) {
	spool, err := newTransferSpool()
	if err != nil {
		t.Fatalf("newTransferSpool: %v", err)
	}
	defer spool.Close()

	payload := make([]byte, transferSpoolBuffer/2)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	for i := range payload {
		if _, err := spool.Write(payload[i : i+1]); err != nil {
			t.Fatalf("spool.Write: %v", err)
		}
	}
	st, err := spool.f.Stat()
	if err != nil {
		t.Fatalf("stat spool: %v", err)
	}
	if st.Size() != 0 {
		t.Fatalf("%d one-byte writes put %d bytes in the tmpfile before the buffer was full; each Write went to the file on its own", len(payload), st.Size())
	}

	if err := spool.Rewind(); err != nil {
		t.Fatalf("spool.Rewind: %v", err)
	}
	got, err := io.ReadAll(spool)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Rewind must flush buffered writes: read back %d bytes, want %d", len(got), len(payload))
	}
}

// Buffering must not cost the short-chunk agent anything: payloads shorter
// than, equal to and just past the spool buffer still arrive intact when every
// NZTC frame carries a single byte.
func TestRelayDownloadFrames_OneByteFramesArriveIntact(t *testing.T) {
	for _, size := range []int{1, transferSpoolBuffer - 1, transferSpoolBuffer, 2*transferSpoolBuffer + 17} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i % 251)
		}
		var src bytes.Buffer
		for i := range payload {
			writeChunkFrame(&src, payload[i:i+1])
		}
		writeOKFrame(&src, uint64(size))

		w, err := relayDownloadFramesRecorded(int64(size), src)
		if err != nil {
			t.Fatalf("%d bytes in one-byte frames: %v", size, err)
		}
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), payload) {
			t.Fatalf("%d bytes in one-byte frames: got %d with %d body bytes", size, w.Code, w.Body.Len())
		}
	}
}
