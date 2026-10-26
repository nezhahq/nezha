package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRequestWrapperRelaysRealHTTPBodyAndTrailers(t *testing.T) {
	for _, bodySize := range []int{2, 8192} {
		for _, chunked := range []bool{false, true} {
			name := fmt.Sprintf("content-length-%d", bodySize)
			if chunked {
				name = fmt.Sprintf("chunked-with-trailer-%d", bodySize)
			}
			t.Run(name, func(t *testing.T) {
				payload := bytes.Repeat([]byte("ordinary-upload-content"), bodySize)
				type receivedRequest struct {
					body    []byte
					trailer string
					err     error
				}
				received := make(chan receivedRequest, 1)
				bodyRead := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					wrapper, err := NewRequestWrapper(r, w)
					if err != nil {
						received <- receivedRequest{err: err}
						return
					}
					defer wrapper.Close()
					forwardedReader := bufio.NewReader(wrapper)
					forwarded, err := http.ReadRequest(forwardedReader)
					if err != nil {
						received <- receivedRequest{err: err}
						return
					}
					body, err := io.ReadAll(forwarded.Body)
					close(bodyRead)
					tail := make([]byte, len("tunnel-tail"))
					if err == nil {
						_, err = io.ReadFull(forwardedReader, tail)
					}
					if err == nil && string(tail) != "tunnel-tail" {
						err = fmt.Errorf("unexpected tunnel bytes: %q", tail)
					}
					received <- receivedRequest{body: body, trailer: forwarded.Trailer.Get("X-Upload-Checksum"), err: err}
					_, _ = io.WriteString(wrapper, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
				}))
				defer server.Close()
				var source io.Reader = bytes.NewReader(payload)
				if chunked {
					source = io.NopCloser(source)
				}
				req, err := http.NewRequest(http.MethodPost, server.URL+"/upload", source)
				require.NoError(t, err)
				if chunked {
					req.Trailer = http.Header{"X-Upload-Checksum": {"complete"}}
				}
				connection, err := net.Dial("tcp", server.Listener.Addr().String())
				require.NoError(t, err)
				defer connection.Close()
				require.NoError(t, connection.SetDeadline(time.Now().Add(5*time.Second)))
				req.Close = true
				require.NoError(t, req.Write(connection))
				<-bodyRead
				_, err = io.WriteString(connection, "tunnel-tail")
				require.NoError(t, err)
				response, err := http.ReadResponse(bufio.NewReader(connection), req)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, http.StatusNoContent, response.StatusCode)
				result := <-received
				require.NoError(t, result.err)
				require.Equal(t, payload, result.body)
				if chunked {
					require.Equal(t, "complete", result.trailer)
				} else {
					require.Empty(t, result.trailer)
				}
			})
		}
	}

}
