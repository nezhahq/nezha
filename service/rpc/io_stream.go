package rpc

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/nezhahq/nezha/service/singleton"
)

type StreamPurpose uint8

const (
	PurposeLegacy StreamPurpose = iota
	PurposeMCPTransfer
	PurposeTerminal
	PurposeFileManager
	PurposeNAT
)

type ioStreamContext struct {
	creatorUserID    uint64
	targetServerID   uint64
	purpose          StreamPurpose
	userIo           io.ReadWriteCloser
	agentIo          io.ReadWriteCloser
	userIoConnectCh  chan struct{}
	agentIoConnectCh chan struct{}
	userIoChOnce     sync.Once
	agentIoChOnce    sync.Once
	revokedCh        chan struct{}
	revokedOnce      sync.Once
	waitStartedCh    chan struct{}
	waitStartedOnce  sync.Once
	startCaptureCh   chan struct{}
	startCaptureOnce sync.Once
}

func newIOStreamContext(creatorUserID, targetServerID uint64, purpose StreamPurpose) *ioStreamContext {
	return &ioStreamContext{
		creatorUserID: creatorUserID, targetServerID: targetServerID, purpose: purpose,
		userIoConnectCh: make(chan struct{}), agentIoConnectCh: make(chan struct{}),
		revokedCh: make(chan struct{}), waitStartedCh: make(chan struct{}), startCaptureCh: make(chan struct{}),
	}
}

func (stream *ioStreamContext) revoke() {
	stream.revokedOnce.Do(func() { close(stream.revokedCh) })
}

type bp struct{ buf []byte }

var bufPool = sync.Pool{New: func() any { return &bp{buf: make([]byte, 1024*1024)} }}

func isValidIOStreamMagic(data []byte) bool {
	return len(data) >= 4 && data[0] == 0xff && data[1] == 0x05 && data[2] == 0xff && data[3] == 0x05
}

func (s *NezhaHandler) StartStream(streamId string, timeout time.Duration) error {
	stream, err := s.GetStream(streamId)
	if err != nil {
		return err
	}
	return s.startStreamContext(streamId, stream, timeout)
}

func (s *NezhaHandler) startStreamContext(streamId string, stream *ioStreamContext, timeout time.Duration) error {
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()
	userConnected := stream.userIoConnectCh
	agentConnected := stream.agentIoConnectCh
	for {
		s.ioStreamMutex.RLock()
		if current, exists := s.ioStreams[streamId]; !exists || current != stream {
			s.ioStreamMutex.RUnlock()
			return errors.New("stream revoked")
		}
		userIo, agentIo := stream.userIo, stream.agentIo
		s.ioStreamMutex.RUnlock()
		stream.startCaptureOnce.Do(func() { close(stream.startCaptureCh) })
		if userIo != nil {
			userConnected = nil
		}
		if agentIo != nil {
			agentConnected = nil
		}
		if userIo != nil && agentIo != nil {
			break
		}
		select {
		case <-userConnected:
			userConnected = nil
		case <-agentConnected:
			agentConnected = nil
		case <-stream.revokedCh:
			return errors.New("stream revoked")
		case <-timeoutTimer.C:
			return singleton.Localizer.ErrorT("timeout: stream endpoints not established")
		}
	}
	s.ioStreamMutex.RLock()
	if current, exists := s.ioStreams[streamId]; !exists || current != stream {
		s.ioStreamMutex.RUnlock()
		return errors.New("stream revoked")
	}
	userIo, agentIo := stream.userIo, stream.agentIo
	s.ioStreamMutex.RUnlock()
	errCh := make(chan error, 2)
	go func() {
		bp := bufPool.Get().(*bp)
		defer bufPool.Put(bp)
		_, copyErr := io.CopyBuffer(userIo, agentIo, bp.buf)
		errCh <- copyErr
	}()
	go func() {
		bp := bufPool.Get().(*bp)
		defer bufPool.Put(bp)
		_, copyErr := io.CopyBuffer(agentIo, userIo, bp.buf)
		errCh <- copyErr
	}()
	// Closing the endpoints does not interrupt a copy blocked in an agent Send,
	// so a detached stream must release its caller without waiting for one.
	select {
	case err := <-errCh:
		return err
	case <-stream.revokedCh:
		return errors.New("stream revoked")
	}
}
