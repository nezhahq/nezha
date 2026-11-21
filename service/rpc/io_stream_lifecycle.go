package rpc

import (
	"context"
	"errors"
	"io"
	"time"
)

type ioStreamDetach struct {
	stream    *ioStreamContext
	endpoints []io.ReadWriteCloser
}

func detachStreamLocked(streamID string, retainedStream *ioStreamContext, streams map[string]*ioStreamContext) (ioStreamDetach, bool) {
	current, live := streams[streamID]
	if streamID == "" || !live || current != retainedStream {
		return ioStreamDetach{}, false
	}
	retainedStream.revoke()
	endpoints := make([]io.ReadWriteCloser, 0, 2)
	if retainedStream.userIo != nil {
		endpoints = append(endpoints, retainedStream.userIo)
	}
	if retainedStream.agentIo != nil {
		endpoints = append(endpoints, retainedStream.agentIo)
	}
	delete(streams, streamID)
	return ioStreamDetach{stream: retainedStream, endpoints: endpoints}, true
}

func (s *NezhaHandler) detachExactStream(streamID string, retainedStream *ioStreamContext) error {
	s.ioStreamMutex.Lock()
	detached, ok := detachStreamLocked(streamID, retainedStream, s.ioStreams)
	if !ok {
		s.ioStreamMutex.Unlock()
		return nil
	}
	s.publishIOStreamStateChangeLocked()
	s.ioStreamMutex.Unlock()

	var closeErrors []error
	for _, endpoint := range detached.endpoints {
		if err := endpoint.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

func (s *NezhaHandler) detachStreams(shouldDetach func(*ioStreamContext) bool) (int, error) {
	s.ioStreamMutex.Lock()
	detached := make([]ioStreamDetach, 0)
	for streamID, stream := range s.ioStreams {
		if !shouldDetach(stream) {
			continue
		}
		item, ok := detachStreamLocked(streamID, stream, s.ioStreams)
		if ok {
			detached = append(detached, item)
		}
	}
	if len(detached) > 0 {
		s.publishIOStreamStateChangeLocked()
	}
	s.ioStreamMutex.Unlock()

	var closeErrors []error
	for _, item := range detached {
		// Registry publication must precede endpoint Close so Close implementations may reenter safely.
		for _, endpoint := range item.endpoints {
			if err := endpoint.Close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
	}
	return len(detached), errors.Join(closeErrors...)
}

// CloseStreamUnlessUserConnects 在 timeout 内没有用户连接附着时关闭这条流。
// 创建接口成功之后流就占着配额，agent 也已经为它起了 shell 或文件会话；
// 浏览器的 WebSocket 没连上来（反向代理没放行、标签页提前关闭、脚本只调了创建
// 接口）的话，没有任何东西会回收它，攒够数量后这个用户乃至整台服务器都开不了新会话。
func (s *NezhaHandler) CloseStreamUnlessUserConnects(streamID string, timeout time.Duration) {
	stream, err := s.GetStream(streamID)
	if err != nil {
		return
	}
	go func() {
		unattached := time.NewTimer(timeout)
		defer unattached.Stop()
		select {
		case <-stream.userIoConnectCh:
		case <-stream.revokedCh:
		case <-unattached.C:
			// 只关闭仍然是这一条的流，ID 被复用时不能误关新的。
			_, _ = s.detachStreams(func(candidate *ioStreamContext) bool { return candidate == stream })
		}
	}()
}

func (s *NezhaHandler) CloseStream(streamID string) error {
	_, err := s.detachStreams(func(stream *ioStreamContext) bool {
		return stream != nil && streamID != "" && stream == s.ioStreams[streamID]
	})
	return err
}

func (s *NezhaHandler) WaitForAgent(ctx context.Context, streamID string, timeout time.Duration) (io.ReadWriteCloser, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.ioStreamMutex.RLock()
		stream, ok := s.ioStreams[streamID]
		if ok && stream.agentIo != nil {
			agentIo := stream.agentIo
			s.ioStreamMutex.RUnlock()
			return agentIo, true
		}
		if !ok {
			s.ioStreamMutex.RUnlock()
			return nil, false
		}
		revokedCh := stream.revokedCh
		agentIoConnectCh := stream.agentIoConnectCh
		stream.waitStartedOnce.Do(func() { close(stream.waitStartedCh) })
		s.ioStreamMutex.RUnlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-deadline.C:
			return nil, false
		case <-revokedCh:
			return nil, false
		case <-agentIoConnectCh:
		}
	}
}

func (s *NezhaHandler) RevokeStreamsForServer(serverID uint64) {
	if serverID == 0 {
		return
	}
	_, _ = s.detachStreams(func(stream *ioStreamContext) bool { return stream.targetServerID == serverID })
}

func (s *NezhaHandler) RevokeStreamsForPurpose(purpose StreamPurpose) int {
	revoked, _ := s.detachStreams(func(stream *ioStreamContext) bool { return stream.purpose == purpose })
	return revoked
}
