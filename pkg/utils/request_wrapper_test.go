package utils

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	errRequestWrite = errors.New("request write failed")
	errBodyClose    = errors.New("body close failed")
	errConnClose    = errors.New("connection close failed")
)

func TestNewRequestWrapper_closesHijackedConnectionWhenRequestWriteFails(t *testing.T) {
	// Given
	conn := newRequestWrapperTestConn(errConnClose)
	conn.readErr = errRequestWrite
	req := &http.Request{
		Method:        "POST",
		URL:           &url.URL{Scheme: "http", Host: "example.test", Path: "/nat"},
		Body:          &requestWrapperTestBody{readErr: errRequestWrite, closeErr: errBodyClose},
		ContentLength: 1,
	}
	writer := &requestWrapperTestResponseWriter{conn: conn}

	// When
	wrapper, err := NewRequestWrapper(req, writer)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(wrapper)
	if readErr == nil || !strings.Contains(readErr.Error(), errRequestWrite.Error()) {
		t.Fatalf("expected streamed body error, got %v", readErr)
	}
	err = wrapper.Close()
	if !errors.Is(err, errConnClose) {
		t.Fatalf("missing cleanup errors: %v", err)
	}

	if got := conn.closeCount.Load(); got != 1 {
		t.Fatalf("expected one connection close, got %d", got)
	}
}

func TestRequestWrapper_Close_joinsBodyAndConnectionErrors(t *testing.T) {
	// Given
	body := &requestWrapperTestBody{closeErr: errBodyClose}
	conn := newRequestWrapperTestConn(errConnClose)
	rw := &RequestWrapper{
		req:       &http.Request{Body: body},
		reader:    bytes.NewBuffer(nil),
		writer:    conn,
		closeDone: make(chan struct{}),
	}

	// When
	err := rw.Close()

	// Then
	if !errors.Is(err, errBodyClose) {
		t.Fatalf("expected body close error, got %v", err)
	}
	if !errors.Is(err, errConnClose) {
		t.Fatalf("expected connection close error, got %v", err)
	}
}

func TestRequestWrapper_Close_repeatedCallersReceiveRetainedErrorAndCloseOnce(t *testing.T) {
	// Given
	body := &requestWrapperTestBody{closeErr: errBodyClose}
	conn := newRequestWrapperTestConn(errConnClose)
	rw := &RequestWrapper{
		req:       &http.Request{Body: body},
		reader:    bytes.NewBuffer(nil),
		writer:    conn,
		closeDone: make(chan struct{}),
	}

	// When
	firstErr := rw.Close()
	secondErr := rw.Close()

	// Then
	if firstErr != secondErr {
		t.Fatalf("expected identical retained error, got distinct values %p and %p", firstErr, secondErr)
	}
	if got := body.closeCount.Load(); got != 1 {
		t.Fatalf("expected one body close, got %d", got)
	}
	if got := conn.closeCount.Load(); got != 1 {
		t.Fatalf("expected one connection close, got %d", got)
	}
}

func TestRequestWrapper_Close_concurrentCallersWaitForCopyToUnblock(t *testing.T) {
	// Given
	body := &requestWrapperTestBody{closeErr: errBodyClose}
	conn := newRequestWrapperTestConn(errConnClose)
	rw := &RequestWrapper{
		req:       &http.Request{Body: body},
		reader:    bytes.NewBuffer(nil),
		writer:    conn,
		closeDone: make(chan struct{}),
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := rw.Read(make([]byte, 1))
		readDone <- err
	}()
	<-conn.readStarted

	// When
	const callerCount = 8
	results := make(chan error, callerCount)
	var callers sync.WaitGroup
	callers.Add(callerCount)
	for range callerCount {
		go func() {
			defer callers.Done()
			results <- rw.Close()
		}()
	}
	callers.Wait()
	close(results)

	// Then
	var retainedErr error
	for err := range results {
		if !errors.Is(err, errConnClose) {
			t.Fatalf("expected retained connection close error, got %v", err)
		}
		if !errors.Is(err, errBodyClose) {
			t.Fatalf("expected retained body close error, got %v", err)
		}
		if retainedErr == nil {
			retainedErr = err
			continue
		}
		if err != retainedErr {
			t.Fatalf("expected identical retained error, got distinct values %p and %p", retainedErr, err)
		}
	}
	<-readDone
	if got := body.closeCount.Load(); got != 1 {
		t.Fatalf("expected one body close, got %d", got)
	}
	if got := conn.closeCount.Load(); got != 1 {
		t.Fatalf("expected one connection close, got %d", got)
	}
}

type requestWrapperTestBody struct {
	readErr    error
	closeErr   error
	closeCount atomic.Int32
}

func (b *requestWrapperTestBody) Read([]byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	return 0, io.EOF
}

func (b *requestWrapperTestBody) Close() error {
	b.closeCount.Add(1)
	return b.closeErr
}

type requestWrapperTestConn struct {
	net.Conn
	closeErr    error
	readErr     error
	closeCount  atomic.Int32
	closeOnce   sync.Once
	readStarted chan struct{}
	readDone    chan struct{}
}

func newRequestWrapperTestConn(closeErr error) *requestWrapperTestConn {
	return &requestWrapperTestConn{
		closeErr:    closeErr,
		readStarted: make(chan struct{}),
		readDone:    make(chan struct{}),
	}
}

func (c *requestWrapperTestConn) Read([]byte) (int, error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	select {
	case <-c.readStarted:
	default:
		close(c.readStarted)
	}
	<-c.readDone
	return 0, io.ErrClosedPipe
}

func (c *requestWrapperTestConn) SetDeadline(time.Time) error { return nil }

func (c *requestWrapperTestConn) Write(p []byte) (int, error) {
	return len(p), nil
}

func (c *requestWrapperTestConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeCount.Add(1)
		close(c.readDone)
	})
	return c.closeErr
}

type requestWrapperTestResponseWriter struct {
	conn net.Conn
}

func (w *requestWrapperTestResponseWriter) Header() http.Header {
	return make(http.Header)
}

func (w *requestWrapperTestResponseWriter) Write([]byte) (int, error) {
	return 0, nil
}

func (w *requestWrapperTestResponseWriter) WriteHeader(int) {}

func (w *requestWrapperTestResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(nil)), bufio.NewWriter(io.Discard)), nil
}
