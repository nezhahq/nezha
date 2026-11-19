package main

import (
	"net"
	"net/http"
	"time"
)

const dashboardReadHeaderTimeout = 5 * time.Second

// dashboardMaxHeaderBytes bounds the header list of one request. For HTTP/2
// net/http applies it to the decoded list, where HPACK lets one byte stand for
// a 4 KiB field. Larger requests are answered with 431.
const dashboardMaxHeaderBytes = 32 << 10

// dashboardHTTP2WriteByteTimeout closes an HTTP/2 connection whose peer accepts
// no bytes for this long while frames are waiting to be written. Without it a
// peer that stops draining the TCP connection blocks every handler writing on
// it for good: a response write deadline is delivered as a RST_STREAM frame,
// which queues behind the blocked write. Idle connections and streams waiting
// on HTTP/2 flow control are not affected.
const dashboardHTTP2WriteByteTimeout = 30 * time.Second

// net/http allocates a frame's whole payload as soon as its 9-byte header
// arrives and keeps the buffer for the life of the connection. 16 KiB is the
// smallest limit HTTP/2 allows and what every client assumes until SETTINGS
// says otherwise; the default of 1 MiB lets a client that never sends a request
// pin 1 MiB per connection. Larger frames are refused with FRAME_SIZE_ERROR.
const dashboardHTTP2MaxReadFrameSize = 16 << 10

func newDashboardHTTPServer(handler http.Handler) *http.Server {
	// ReadHeaderTimeout protects incomplete HTTP/1 requests and is cleared after
	// headers. Unlike ReadTimeout it does not impose a lifetime on HTTP/2 streams.
	return &http.Server{Handler: handler, ReadHeaderTimeout: dashboardReadHeaderTimeout,
		MaxHeaderBytes: dashboardMaxHeaderBytes,
		HTTP2:          &http.HTTP2Config{WriteByteTimeout: dashboardHTTP2WriteByteTimeout, MaxReadFrameSize: dashboardHTTP2MaxReadFrameSize},
		ConnState: func(conn net.Conn, state http.ConnState) {
			// Go 1.26 carries the HTTP/1 header deadline into h2c. Clear it once
			// headers/preface are accepted so long-running gRPC streams stay alive.
			if state == http.StateActive {
				_ = conn.SetReadDeadline(time.Time{})
			}
		},
	}
}
