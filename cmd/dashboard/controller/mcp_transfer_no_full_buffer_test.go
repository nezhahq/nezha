package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"runtime"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
)

type fixedSizeFrameStream struct {
	buf bytes.Buffer
}

func (s *fixedSizeFrameStream) Read(p []byte) (int, error)  { return s.buf.Read(p) }
func (s *fixedSizeFrameStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *fixedSizeFrameStream) Close() error                { return nil }

func writeChunkFrame(out *bytes.Buffer, chunk []byte) {
	out.Write(model.MCPFsXferMagicChunk)
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(len(chunk)))
	out.Write(sz[:])
	out.Write(chunk)
}

func writeOKFrame(out *bytes.Buffer, size uint64) {
	out.Write(model.MCPFsXferMagicOK)
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], size)
	out.Write(sz[:])
	out.Write(make([]byte, 32))
}

// countingDiscardWriter satisfies http.ResponseWriter but throws bytes away
// after counting them, so the test can measure relayDownloadFrames heap
// pressure without httptest.ResponseRecorder caching 100MiB of body in
// memory and dominating the measurement.
type countingDiscardWriter struct {
	header  http.Header
	written int64
	status  int
}

func newCountingDiscardWriter() *countingDiscardWriter {
	return &countingDiscardWriter{header: make(http.Header)}
}

func (w *countingDiscardWriter) Header() http.Header { return w.header }
func (w *countingDiscardWriter) Write(p []byte) (int, error) {
	w.written += int64(len(p))
	return len(p), nil
}
func (w *countingDiscardWriter) WriteHeader(status int) { w.status = status }
func (w *countingDiscardWriter) Flush()                 {}
func (w *countingDiscardWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("not hijackable")
}

func TestRelayDownloadFrames_DoesNotBufferEntirePayloadInMemory(t *testing.T) {
	const size = int64(model.MCPFsTransferMaxSize)
	const chunk = 1 * 1024 * 1024

	var src bytes.Buffer
	payload := make([]byte, chunk)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	remaining := size
	for remaining > 0 {
		toWrite := int64(chunk)
		if toWrite > remaining {
			toWrite = remaining
		}
		writeChunkFrame(&src, payload[:toWrite])
		remaining -= toWrite
	}
	writeOKFrame(&src, uint64(size))

	stream := &fixedSizeFrameStream{buf: src}

	sink := newCountingDiscardWriter()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(sink)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	if err := relayDownloadFrames(context.Background(), c, stream, size, nil); err != nil {
		t.Fatalf("relayDownloadFrames returned err: %v", err)
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	const allow = 16 * 1024 * 1024
	if delta > allow {
		t.Fatalf("relayDownloadFrames retained %d bytes in heap after a %d-byte transfer (allow <= %d). 100MiB 旁路通道不应整文件缓在 dashboard 内存里。",
			delta, size, allow)
	}
	if sink.written != size {
		t.Fatalf("expected %d bytes forwarded to HTTP client, got %d", size, sink.written)
	}
}
