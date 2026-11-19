package grpcx

import (
	"context"
	"net/http"
	"sync"
	"time"
)

type streamAbortKey struct{}

// ServeAbortable serves one gRPC request through grpc-go's ServeHTTP transport
// and gives its handler a way to end the request for real (StreamAbort). On
// that transport every Send and the final status run one after another on the
// ServeHTTP goroutine, so a response write parked on HTTP/2 flow control is
// not released when the handler returns: later Sends and the status queue
// behind it for as long as the peer keeps the stream open without reading.
func ServeAbortable(grpcHandler http.Handler, w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	controller := http.NewResponseController(w)
	var mu sync.Mutex
	done := false // the request is over or has been aborted
	abort := func() {
		mu.Lock()
		defer mu.Unlock()
		if done {
			// The ResponseWriter must not be touched after ServeHTTP returns.
			return
		}
		done = true
		// An expired write deadline resets the HTTP/2 stream, which releases
		// the parked write; cancelling the request closes grpc's handler
		// transport, so the Send and status queued behind that write return.
		_ = controller.SetWriteDeadline(time.Now().Add(-time.Second))
		cancel()
	}
	defer func() {
		mu.Lock()
		done = true
		mu.Unlock()
	}()
	grpcHandler.ServeHTTP(w, r.WithContext(context.WithValue(ctx, streamAbortKey{}, abort)))
}

// StreamAbort returns the function that resets the stream ctx belongs to, or
// nil when the stream was not served through ServeAbortable. The function is
// safe to call at any time, from any goroutine and more than once; it does
// nothing once the request is over.
func StreamAbort(ctx context.Context) func() {
	abort, _ := ctx.Value(streamAbortKey{}).(func())
	return abort
}
