package rpc

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nezhahq/nezha/service/singleton"
)

type ioStreamContext struct {
	userIo           io.ReadWriteCloser
	agentIo          io.ReadWriteCloser
	userIoConnectCh  chan struct{}
	agentIoConnectCh chan struct{}
	userIoChOnce     sync.Once
	agentIoChOnce    sync.Once
}

type bp struct {
	buf []byte
}

var bufPool = sync.Pool{
	New: func() any {
		return &bp{
			buf: make([]byte, 1024*1024),
		}
	},
}

func (s *NezhaHandler) CreateStream(streamId string) {
	s.ioStreamMutex.Lock()
	defer s.ioStreamMutex.Unlock()

	s.ioStreams[streamId] = &ioStreamContext{
		userIoConnectCh:  make(chan struct{}),
		agentIoConnectCh: make(chan struct{}),
	}
}

func (s *NezhaHandler) GetStream(streamId string) (*ioStreamContext, error) {
	s.ioStreamMutex.RLock()
	defer s.ioStreamMutex.RUnlock()

	if ctx, ok := s.ioStreams[streamId]; ok {
		return ctx, nil
	}

	return nil, errors.New("stream not found")
}

func (s *NezhaHandler) CloseStream(streamId string) error {
	s.ioStreamMutex.Lock()
	defer s.ioStreamMutex.Unlock()

	if ctx, ok := s.ioStreams[streamId]; ok {
		if ctx.userIo != nil {
			ctx.userIo.Close()
		}
		if ctx.agentIo != nil {
			ctx.agentIo.Close()
		}
		delete(s.ioStreams, streamId)
	}

	return nil
}

func (s *NezhaHandler) UserConnected(streamId string, userIo io.ReadWriteCloser) error {
	stream, err := s.GetStream(streamId)
	if err != nil {
		return err
	}

	stream.userIo = userIo
	stream.userIoChOnce.Do(func() {
		close(stream.userIoConnectCh)
	})

	return nil
}

func (s *NezhaHandler) AgentConnected(streamId string, agentIo io.ReadWriteCloser) error {
	stream, err := s.GetStream(streamId)
	if err != nil {
		return err
	}

	stream.agentIo = agentIo
	stream.agentIoChOnce.Do(func() {
		close(stream.agentIoConnectCh)
	})

	return nil
}

func (s *NezhaHandler) StartStream(streamId string, timeout time.Duration) error {
	stream, err := s.GetStream(streamId)
	if err != nil {
		return err
	}

	timeoutTimer := time.NewTimer(timeout)

LOOP:
	for {
		select {
		case <-stream.userIoConnectCh:
			if stream.agentIo != nil {
				timeoutTimer.Stop()
				break LOOP
			}
		case <-stream.agentIoConnectCh:
			if stream.userIo != nil {
				timeoutTimer.Stop()
				break LOOP
			}
		case <-time.After(timeout):
			break LOOP
		}
		time.Sleep(time.Millisecond * 500)
	}

	if stream.userIo == nil && stream.agentIo == nil {
		return singleton.Localizer.ErrorT("timeout: no connection established")
	}
	if stream.userIo == nil {
		return singleton.Localizer.ErrorT("timeout: user connection not established")
	}
	if stream.agentIo == nil {
		return singleton.Localizer.ErrorT("timeout: agent connection not established")
	}

	isDone := new(atomic.Bool)
	endCh := make(chan struct{})

	go func() {
		bp := bufPool.Get().(*bp)
		defer bufPool.Put(bp)
		_, innerErr := io.CopyBuffer(stream.userIo, stream.agentIo, bp.buf)
		if innerErr != nil {
			err = innerErr
		}
		if isDone.CompareAndSwap(false, true) {
			close(endCh)
		}
	}()
	go func() {
		bp := bufPool.Get().(*bp)
		defer bufPool.Put(bp)
		_, innerErr := io.CopyBuffer(stream.agentIo, stream.userIo, bp.buf)
		if innerErr != nil {
			err = innerErr
		}
		if isDone.CompareAndSwap(false, true) {
			close(endCh)
		}
	}()

	<-endCh
	return err
}
