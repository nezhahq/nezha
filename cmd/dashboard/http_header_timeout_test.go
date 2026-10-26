package main

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
