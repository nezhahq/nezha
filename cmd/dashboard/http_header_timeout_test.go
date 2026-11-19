package main

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func TestDashboardRejectsIncompleteHTTPHeaders(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		name := "http"
		if useTLS {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("incomplete headers reached handler") }))
			server.Config = newDashboardHTTPServer(server.Config.Handler)
			require.Positive(t, server.Config.ReadHeaderTimeout)
			server.Config.ReadHeaderTimeout = 250 * time.Millisecond
			if useTLS {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			var conn net.Conn
			var err error
			if useTLS {
				conn, err = tls.Dial("tcp", server.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
			} else {
				conn, err = net.Dial("tcp", server.Listener.Addr().String())
			}
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
			_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: dashboard\r\nX-Incomplete: ")
			require.NoError(t, err)
			_, err = io.ReadAll(conn)
			if networkErr, ok := err.(net.Error); ok {
				require.False(t, networkErr.Timeout(), "server did not close incomplete request")
			}
		})
	}
}

func TestDashboardHeaderTimeoutDoesNotCancelHTTP2Stream(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		name := "h2c"
		if useTLS {
			name = "h2"
		}
		t.Run(name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					t.Errorf("expected HTTP/2, got %s", r.Proto)
				}
				require.NoError(t, http.NewResponseController(w).Flush())
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("long-lived request body: %v", err)
					return
				}
				_, _ = w.Write(body)
			})
			server := httptest.NewUnstartedServer(handler)
			server.Config = newDashboardHTTPServer(handler)
			server.Config.ReadHeaderTimeout = 250 * time.Millisecond
			server.Config.Protocols = new(http.Protocols)
			server.Config.Protocols.SetHTTP1(true)
			server.Config.Protocols.SetHTTP2(true)
			server.Config.Protocols.SetUnencryptedHTTP2(true)
			server.EnableHTTP2 = true
			if useTLS {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, Protocols: new(http.Protocols)}
			transport.Protocols.SetHTTP2(true)
			transport.Protocols.SetUnencryptedHTTP2(true)
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			request, err := http.NewRequest(http.MethodPost, server.URL, reader)
			require.NoError(t, err)
			response, err := client.Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			time.Sleep(4 * server.Config.ReadHeaderTimeout)
			_, err = writer.Write([]byte("still streaming"))
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, "still streaming", string(body))
		})
	}
}

// HPACK sends a repeated field as one byte, so the decoded list has to be bounded.
func TestDashboardHTTP2BoundsDecodedHeaderList(t *testing.T) {
	// With its 32 bytes of HPACK overhead this cookie exactly fills the dynamic table.
	cookie := hpack.HeaderField{Name: "cookie", Value: strings.Repeat("a", 4096-32-len("cookie"))}
	for _, tc := range []struct {
		cookies int
		status  string
	}{
		{dashboardMaxHeaderBytes / 4096 / 2, "200"},
		{dashboardMaxHeaderBytes / 4096 * 2, "431"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			server := httptest.NewUnstartedServer(nil)
			server.Config = newDashboardHTTPServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			server.Config.Protocols = new(http.Protocols)
			server.Config.Protocols.SetUnencryptedHTTP2(true)
			server.Start()
			defer server.Close()

			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
			_, err = io.WriteString(conn, http2.ClientPreface)
			require.NoError(t, err)
			framer := http2.NewFramer(conn, conn)
			framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
			require.NoError(t, framer.WriteSettings())

			var request bytes.Buffer
			encoder := hpack.NewEncoder(&request)
			fields := []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"},
				{Name: ":path", Value: "/"}, {Name: ":authority", Value: "dashboard"}}
			for range tc.cookies {
				fields = append(fields, cookie)
			}
			for _, field := range fields {
				require.NoError(t, encoder.WriteField(field))
			}
			require.Less(t, request.Len(), 4096, "cookies after the first must be table references")
			require.NoError(t, framer.WriteHeaders(http2.HeadersFrameParam{
				StreamID: 1, BlockFragment: request.Bytes(), EndStream: true, EndHeaders: true}))

			for {
				frame, err := framer.ReadFrame()
				require.NoError(t, err)
				if response, ok := frame.(*http2.MetaHeadersFrame); ok {
					require.Equal(t, tc.status, response.PseudoValue("status"))
					return
				}
			}
		})
	}
}

// An HTTP/2 response write deadline is delivered as a RST_STREAM frame, which
// cannot be written to a peer that stopped draining the TCP connection. The
// write byte timeout has to close such a connection, or the handlers writing on
// it (and whatever they hold) stay blocked for as long as the peer likes.
func TestDashboardHTTP2ClosesConnectionThatStoppedReading(t *testing.T) {
	handlerReturned := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected HTTP/2, got %s", r.Proto)
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
		chunk := make([]byte, 1<<20)
		for {
			if _, err := w.Write(chunk); err != nil {
				handlerReturned <- err
				return
			}
		}
	})
	server := httptest.NewUnstartedServer(handler)
	server.Config = newDashboardHTTPServer(handler)
	require.Positive(t, server.Config.HTTP2.WriteByteTimeout)
	server.Config.HTTP2.WriteByteTimeout = 250 * time.Millisecond
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetHTTP1(true)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	// Open HTTP/2 flow control all the way, so that what stops the server's
	// writes is the full TCP socket and not a stream window; then never read.
	const largestWindow = 1<<31 - 1
	_, err = io.WriteString(conn, http2.ClientPreface)
	require.NoError(t, err)
	framer := http2.NewFramer(conn, conn)
	require.NoError(t, framer.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: largestWindow}))
	require.NoError(t, framer.WriteWindowUpdate(0, largestWindow-65535))
	var request bytes.Buffer
	encoder := hpack.NewEncoder(&request)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: http.MethodGet}, {Name: ":scheme", Value: "http"},
		{Name: ":path", Value: "/"}, {Name: ":authority", Value: "dashboard"},
	} {
		require.NoError(t, encoder.WriteField(field))
	}
	require.NoError(t, framer.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: request.Bytes(), EndStream: true, EndHeaders: true}))

	select {
	case err := <-handlerReturned:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("handler is still blocked writing to a peer that stopped reading")
	}
}

// net/http allocates a frame's payload when its 9-byte header arrives and waits
// for the rest without a deadline, so the largest frame a client may announce
// is memory it can pin per connection without ever sending a request.
func TestDashboardHTTP2RefusesOversizedFrameHeader(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		name := "h2c"
		if useTLS {
			name = "h2"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unfinished frame reached handler") }))
			server.Config = newDashboardHTTPServer(server.Config.Handler)
			server.Config.Protocols = new(http.Protocols)
			server.Config.Protocols.SetHTTP1(true)
			server.Config.Protocols.SetHTTP2(true)
			server.Config.Protocols.SetUnencryptedHTTP2(true)
			server.EnableHTTP2 = true
			if useTLS {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			var conn net.Conn
			var err error
			if useTLS {
				conn, err = tls.Dial("tcp", server.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
			} else {
				conn, err = net.Dial("tcp", server.Listener.Addr().String())
			}
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
			_, err = io.WriteString(conn, http2.ClientPreface)
			require.NoError(t, err)
			framer := http2.NewFramer(conn, conn)
			require.NoError(t, framer.WriteSettings())
			// Header of an unknown-type frame on stream 0, one byte over the limit,
			// and only the first byte of its payload.
			const length = dashboardHTTP2MaxReadFrameSize + 1
			_, err = conn.Write([]byte{length >> 16, length >> 8 & 0xff, length & 0xff, 0x63, 0, 0, 0, 0, 0, 'x'})
			require.NoError(t, err)
			for {
				frame, err := framer.ReadFrame()
				require.NoError(t, err, "server waits for the oversized frame")
				if settings, ok := frame.(*http2.SettingsFrame); ok && !settings.IsAck() {
					maxFrameSize, ok := settings.Value(http2.SettingMaxFrameSize)
					require.True(t, ok)
					require.EqualValues(t, dashboardHTTP2MaxReadFrameSize, maxFrameSize)
				}
				if goAway, ok := frame.(*http2.GoAwayFrame); ok {
					require.Equal(t, http2.ErrCodeFrameSize, goAway.ErrCode)
					return
				}
			}
		})
	}
}
