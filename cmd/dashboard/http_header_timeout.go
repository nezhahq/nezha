package main

import (
	"net"
	"net/http"
	"time"
)

const dashboardReadHeaderTimeout = 5 * time.Second

func newDashboardHTTPServer(handler http.Handler) *http.Server {
	// ReadHeaderTimeout protects incomplete HTTP/1 requests and is cleared after
	// headers. Unlike ReadTimeout it does not impose a lifetime on HTTP/2 streams.
	return &http.Server{Handler: handler, ReadHeaderTimeout: dashboardReadHeaderTimeout,
		ConnState: func(conn net.Conn, state http.ConnState) {
			// Go 1.26 carries the HTTP/1 header deadline into h2c. Clear it once
			// headers/preface are accepted so long-running gRPC streams stay alive.
			if state == http.StateActive {
				_ = conn.SetReadDeadline(time.Time{})
			}
		},
	}
}
