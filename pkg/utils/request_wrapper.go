package utils

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const NATMaxBodyBytes int64 = 64 << 20
const NATMaxDuration = 5 * time.Minute

var _ io.ReadWriteCloser = (*RequestWrapper)(nil)

type RequestWrapper struct {
	req          *http.Request
	reader       io.Reader
	pipe         *io.PipeReader
	producerDone chan struct{}
	tail         io.Reader
	writer       net.Conn

	closeOnce sync.Once
	closeInit sync.Once
	closeDone chan struct{}
	closeErr  error
}

func NewRequestWrapper(req *http.Request, writer http.ResponseWriter) (*RequestWrapper, error) {
	hj, ok := writer.(http.Hijacker)
	if !ok {
		return nil, errors.New("http server does not support hijacking")
	}
	if req.ContentLength > NATMaxBodyBytes {
		return nil, errors.New("NAT request body exceeds 64 MiB")
	}
	conn, buffered, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	// Bound the entire hijacked connection, including blocked body reads and writes.
	if err := conn.SetDeadline(time.Now().Add(NATMaxDuration)); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// net/http's original Body starts a background connection read at EOF.
	// Reading it after Hijack races tunnel reads and can panic. Reconstruct only
	// its framing on a detached reader, retaining every byte already buffered.
	pending := make([]byte, buffered.Reader.Buffered())
	if _, err := io.ReadFull(buffered.Reader, pending); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var framing strings.Builder
	framing.WriteString("POST / HTTP/1.1\r\nHost: nat\r\n")
	if len(req.TransferEncoding) > 0 {
		framing.WriteString("Transfer-Encoding: chunked\r\n")
		var trailerNames []string
		for name := range req.Trailer {
			trailerNames = append(trailerNames, name)
		}
		if len(trailerNames) > 0 {
			if err := (http.Header{"Trailer": {strings.Join(trailerNames, ",")}}).Write(&framing); err != nil {
				_ = conn.Close()
				return nil, err
			}
		}
	} else {
		fmt.Fprintf(&framing, "Content-Length: %d\r\n", max(int64(0), req.ContentLength))
	}
	framing.WriteString("\r\n")
	tunnelReader := bufio.NewReader(io.MultiReader(strings.NewReader(framing.String()), bytes.NewReader(pending), conn))
	framedRequest, err := http.ReadRequest(tunnelReader)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Body = http.MaxBytesReader(nil, framedRequest.Body, NATMaxBodyBytes)
	req.Trailer = framedRequest.Trailer

	reader, producer := io.Pipe()
	wrapper := &RequestWrapper{req: req, reader: reader, pipe: reader, writer: conn, tail: tunnelReader, producerDone: make(chan struct{}), closeDone: make(chan struct{})}
	// Pipe backpressure prevents the request body from accumulating in memory.
	go func() {
		defer close(wrapper.producerDone)
		_ = producer.CloseWithError(req.Write(producer))
	}()
	return wrapper, nil
}

func (rw *RequestWrapper) Read(p []byte) (int, error) {
	count, err := rw.reader.Read(p)
	if err == nil {
		return count, nil
	}
	if err != io.EOF {
		return count, err
	}
	// request 数据读完之后等待客户端断开连接或 grpc 超时
	if rw.tail != nil {
		return rw.tail.Read(p)
	}
	return rw.writer.Read(p)
}

func (rw *RequestWrapper) Write(p []byte) (int, error) {
	return rw.writer.Write(p)
}

func (rw *RequestWrapper) Close() error {
	rw.closeInit.Do(func() {
		rw.closeDone = make(chan struct{})
	})
	rw.closeOnce.Do(func() {
		// Close transport first: Body.Close may otherwise wait for a network read.
		connErr := rw.writer.Close()
		if rw.pipe != nil {
			_ = rw.pipe.Close()
		}
		var bodyErr error
		if rw.req.Body != nil {
			bodyErr = rw.req.Body.Close()
		}
		if rw.producerDone != nil {
			<-rw.producerDone
		}
		rw.closeErr = errors.Join(bodyErr, connErr)
		close(rw.closeDone)
	})
	<-rw.closeDone
	return rw.closeErr
}
