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

func newDashboardHTTPServer(handler http.Handler) *http.Server {
	// ReadHeaderTimeout protects incomplete HTTP/1 requests and is cleared after
	// headers. Unlike ReadTimeout it does not impose a lifetime on HTTP/2 streams.
	return &http.Server{Handler: handler, ReadHeaderTimeout: dashboardReadHeaderTimeout,
		MaxHeaderBytes: dashboardMaxHeaderBytes,
		ConnState: func(conn net.Conn, state http.ConnState) {
			// Go 1.26 carries the HTTP/1 header deadline into h2c. Clear it once
			// headers/preface are accepted so long-running gRPC streams stay alive.
			if state == http.StateActive {
				_ = conn.SetReadDeadline(time.Time{})
			}
		},
	}
}
