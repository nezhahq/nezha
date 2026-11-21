package rpc

import (
	"errors"
	"io"
)

var ErrAgentStreamAlreadyConnected = errors.New("agent stream already connected")

// ErrUserStreamAlreadyConnected is returned when a stream already has its user connection.
var ErrUserStreamAlreadyConnected = errors.New("user stream already connected")

func (s *NezhaHandler) IsStreamAuthorizedForAgent(streamId string, agentServerID uint64) bool {
	s.ioStreamMutex.RLock()
	defer s.ioStreamMutex.RUnlock()
	ctx, ok := s.ioStreams[streamId]
	return ok && ctx.targetServerID != 0 && ctx.targetServerID == agentServerID
}

func (s *NezhaHandler) IsStreamAuthorizedForUser(streamId string, userID uint64, isAdmin bool) bool {
	creator, found := s.StreamOwnership(streamId)
	return found && (isAdmin || creator == userID)
}

func (s *NezhaHandler) StreamOwnership(streamId string) (uint64, bool) {
	s.ioStreamMutex.RLock()
	defer s.ioStreamMutex.RUnlock()
	ctx, ok := s.ioStreams[streamId]
	if !ok {
		return 0, false
	}
	return ctx.creatorUserID, true
}

func (s *NezhaHandler) StreamTarget(streamId string) (uint64, bool) {
	s.ioStreamMutex.RLock()
	defer s.ioStreamMutex.RUnlock()
	ctx, ok := s.ioStreams[streamId]
	if !ok {
		return 0, false
	}
	return ctx.targetServerID, true
}

func (s *NezhaHandler) GetStream(streamId string) (*ioStreamContext, error) {
	s.ioStreamMutex.RLock()
	defer s.ioStreamMutex.RUnlock()
	if ctx, ok := s.ioStreams[streamId]; ok {
		return ctx, nil
	}
	return nil, errors.New("stream not found")
}

func (s *NezhaHandler) UserConnected(streamId string, userIo io.ReadWriteCloser) error {
	s.ioStreamMutex.Lock()
	stream, ok := s.ioStreams[streamId]
	if !ok {
		s.ioStreamMutex.Unlock()
		return errors.New("stream not found")
	}
	// 一条流只能附着一个用户连接。覆盖已有连接的话，同一个会话 ID 再连一次就会
	// 让 StartStream 再起一对中继协程，两对协程并发读同一个缓冲区，切片越界后
	// 整个面板进程退出。
	if stream.userIo != nil {
		s.ioStreamMutex.Unlock()
		return ErrUserStreamAlreadyConnected
	}
	stream.userIo = userIo
	s.ioStreamMutex.Unlock()
	stream.userIoChOnce.Do(func() { close(stream.userIoConnectCh) })
	return nil
}

func (s *NezhaHandler) AgentConnected(streamId string, agentIo io.ReadWriteCloser) error {
	s.ioStreamMutex.Lock()
	stream, ok := s.ioStreams[streamId]
	if !ok {
		s.ioStreamMutex.Unlock()
		return errors.Join(errors.New("stream not found"), agentIo.Close())
	}
	if stream.agentIo != nil {
		s.ioStreamMutex.Unlock()
		return errors.Join(ErrAgentStreamAlreadyConnected, agentIo.Close())
	}
	stream.agentIo = agentIo
	s.ioStreamMutex.Unlock()
	stream.agentIoChOnce.Do(func() { close(stream.agentIoConnectCh) })
	return nil
}

func (s *NezhaHandler) streamEndpoints(stream *ioStreamContext) (io.ReadWriteCloser, io.ReadWriteCloser) {
	s.ioStreamMutex.RLock()
	defer s.ioStreamMutex.RUnlock()
	return stream.userIo, stream.agentIo
}
