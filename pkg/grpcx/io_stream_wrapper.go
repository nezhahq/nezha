package grpcx

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/nezhahq/nezha/proto"
)

var _ io.ReadWriteCloser = (*IOStreamWrapper)(nil)

type IOStream interface {
	Recv() (*proto.IOStreamData, error)
	Send(*proto.IOStreamData) error
	Context() context.Context
}

type IOStreamWrapper struct {
	IOStream
	dataBuf []byte
	closed  *atomic.Bool
	closeCh chan struct{}
}

func NewIOStreamWrapper(stream IOStream) *IOStreamWrapper {
	return &IOStreamWrapper{
		IOStream: stream,
		closeCh:  make(chan struct{}),
		closed:   new(atomic.Bool),
	}
}

func (iw *IOStreamWrapper) Read(p []byte) (n int, err error) {
	if len(iw.dataBuf) > 0 {
		n := copy(p, iw.dataBuf)
		iw.dataBuf = iw.dataBuf[n:]
		return n, nil
	}
	var data *proto.IOStreamData
	if data, err = iw.Recv(); err != nil {
		return 0, err
	}
	n = copy(p, data.Data)
	if n < len(data.Data) {
		iw.dataBuf = data.Data[n:]
	}
	return n, nil
}

func (iw *IOStreamWrapper) Write(p []byte) (n int, err error) {
	err = iw.Send(&proto.IOStreamData{Data: p})
	return len(p), err
}

func (iw *IOStreamWrapper) Close() error {
	if iw.closed.CompareAndSwap(false, true) {
		close(iw.closeCh)
	}
	return nil
}

func (iw *IOStreamWrapper) Wait() {
	<-iw.closeCh
}
