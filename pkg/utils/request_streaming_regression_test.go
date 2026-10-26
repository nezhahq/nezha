package utils

import (
	"bytes"
	"io"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type countedNATBody struct {
	*requestWrapperTestConn
	reader    *bytes.Reader
	bytesRead atomic.Int64
}

func (body *countedNATBody) Read(data []byte) (int, error) {
	n, err := body.reader.Read(data)
	body.bytesRead.Add(int64(n))
	return n, err
}
func (body *countedNATBody) Close() error { return body.requestWrapperTestConn.Close() }

func TestRequestWrapperDoesNotPrebufferBody(t *testing.T) {
	payload := bytes.Repeat([]byte("bounded-request-body"), 4096)
	body := &countedNATBody{requestWrapperTestConn: newRequestWrapperTestConn(nil), reader: bytes.NewReader(payload)}
	request := httptest.NewRequest("POST", "http://nat.example.invalid/upload", nil)
	request.Body = body
	request.ContentLength = int64(len(payload))
	wrapper, err := NewRequestWrapper(request, &requestWrapperTestResponseWriter{conn: body})
	require.NoError(t, err)
	defer wrapper.Close()
	require.Zero(t, body.bytesRead.Load(), "constructing the wrapper must not drain the body")
	// Consume the exact serialized request; do not wait for the subsequent tunnel EOF.
	var expected bytes.Buffer
	reference := httptest.NewRequest("POST", "http://nat.example.invalid/upload", bytes.NewReader(payload))
	require.NoError(t, reference.Write(&expected))
	actual := make([]byte, expected.Len())
	_, err = io.ReadFull(wrapper, actual)
	require.NoError(t, err)
	require.Equal(t, expected.Bytes(), actual)
	require.Equal(t, int64(len(payload)), body.bytesRead.Load())
}
