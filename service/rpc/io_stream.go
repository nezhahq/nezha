package rpc

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/nezhahq/nezha/service/singleton"
)

// StreamPurpose tags every IOStream with the feature that opened it so
// admin actions can drop only the relevant subset. Existing call sites
// (terminal / fm / NAT / server-transfer) keep PurposeLegacy and the
// previous semantics; only the new MCP fs.transfer path uses
// PurposeMCPTransfer, which is what EnableMCP=false revokes.
type StreamPurpose uint8

const (
	PurposeLegacy StreamPurpose = iota
	PurposeMCPTransfer
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

const (
	maxStreamsPerUser   = 20
	maxStreamsPerServer = 40
)

var (
	ErrTooManyStreamsForUser   = errors.New("too many concurrent streams for this user")
	ErrTooManyStreamsForServer = errors.New("too many concurrent streams for this server")
)

func (s *NezhaHandler) CreateStream(streamId string, creatorUserID uint64, targetServerID uint64) error {
	return s.CreateStreamWithPurpose(streamId, creatorUserID, targetServerID, PurposeLegacy)
}

func (s *NezhaHandler) CreateStreamWithPurpose(streamId string, creatorUserID uint64, targetServerID uint64, purpose StreamPurpose) error {
	s.ioStreamMutex.Lock()
	defer s.ioStreamMutex.Unlock()

	var perUser, perServer int
	for _, ctx := range s.ioStreams {
		if creatorUserID != 0 && ctx.creatorUserID == creatorUserID {
			perUser++
		}
		if ctx.targetServerID == targetServerID {
			perServer++
		}
	}
	// creatorUserID==0 is a dashboard-internal stream (NAT, server transfer,
	// MCP transfer); only end-user-initiated streams are capped per user, but
	// every stream counts toward the per-server cap so one server cannot be
	// flooded regardless of who opened the streams.
	if creatorUserID != 0 && perUser >= maxStreamsPerUser {
		return ErrTooManyStreamsForUser
	}
	if perServer >= maxStreamsPerServer {
		return ErrTooManyStreamsForServer
	}

	s.ioStreams[streamId] = &ioStreamContext{
		creatorUserID:    creatorUserID,
		targetServerID:   targetServerID,
		purpose:          purpose,
		userIoConnectCh:  make(chan struct{}),
		agentIoConnectCh: make(chan struct{}),
		revokedCh:        make(chan struct{}),
	}
	return nil
}

// IsStreamAuthorizedForAgent reports whether the connecting agent is the
// server the dashboard selected when CreateStream was called. Without this
// check any authenticated agent that learns an active streamId — via
// task-stream observation, leaked logs, or a shared global agent secret —
// can race in via IOStream() and serve a terminal / fm / NAT session that
// was addressed to a different server, turning the channel into a
// session-hijack RCE primitive. This is the agent-side dual of
// IsStreamAuthorizedForUser.
func (s *NezhaHandler) IsStreamAuthorizedForAgent(streamId string, agentServerID uint64) bool {
	s.ioStreamMutex.RLock()
	defer s.ioStreamMutex.RUnlock()

	ctx, ok := s.ioStreams[streamId]
	if !ok {
		return false
	}
	return ctx.targetServerID != 0 && ctx.targetServerID == agentServerID
}

// WaitForAgent 阻塞等待 agent 端通过 IOStream 接入并完成 AgentConnected。
// dashboard 把 MCP 大文件传输的 task 派给 agent 后，需要等 agent dial 回来
// 才能开始 Read/Write，这里以 timeout 内的轻量轮询暴露给 controller。
//
// 同时返回 agent 端流（io.ReadWriteCloser）以便 controller 调 io.CopyN 转发
// HTTP body；ok=false 表示超时或流已被关闭。
func (s *NezhaHandler) WaitForAgent(ctx context.Context, streamId string, timeout time.Duration) (io.ReadWriteCloser, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.ioStreamMutex.RLock()
		sc, ok := s.ioStreams[streamId]
		if ok && sc.agentIo != nil {
			s.ioStreamMutex.RUnlock()
			return sc.agentIo, true
		}
		s.ioStreamMutex.RUnlock()
		if !ok {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-deadline.C:
			return nil, false
		case <-sc.revokedCh:
			return nil, false
		case <-sc.agentIoConnectCh:
			s.ioStreamMutex.RLock()
			ag := sc.agentIo
			s.ioStreamMutex.RUnlock()
			return ag, ag != nil
		}
	}
}

// IsStreamAuthorizedForUser checks whether the requesting user may attach to
// the stream. A stream is reachable only by its creator or by an admin; any
// other authenticated user must be rejected. Unknown streams are always
// rejected.
func (s *NezhaHandler) IsStreamAuthorizedForUser(streamId string, userID uint64, isAdmin bool) bool {
	creator, found := s.StreamOwnership(streamId)
	if !found {
		return false
	}
	if isAdmin {
		return true
	}
	return creator == userID
}

// isValidIOStreamMagic reports whether the first four bytes of an IOStream
// init message carry the ff05ff05 marker. Previously this was inlined as
// `byte0 != 0xff && byte1 != 0x05 && byte2 != 0xff && byte3 == 0x05` to
// detect *invalid* payloads — but && short-circuited so any payload whose
// byte0 happened to be 0xff slipped through. Centralising the check here and
// stating the contract positively (all four bytes must match) eliminates the
// short-circuit class of mistakes.
func isValidIOStreamMagic(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	return data[0] == 0xff && data[1] == 0x05 && data[2] == 0xff && data[3] == 0x05
}

// StreamOwnership returns the user ID that created the stream and whether the
// stream is still tracked. Callers must compare the returned creator against
// the requesting user before attaching to the stream — without this the
// channel becomes a session-hijack primitive (terminal/file manager RCE).
func (s *NezhaHandler) StreamOwnership(streamId string) (uint64, bool) {
	s.ioStreamMutex.RLock()
	defer s.ioStreamMutex.RUnlock()

	ctx, ok := s.ioStreams[streamId]
	if !ok {
		return 0, false
	}
	return ctx.creatorUserID, true
}

// StreamTarget returns the server ID the stream was opened against and
// whether the stream is still tracked. Callers MUST pass this through the
// requesting PAT's CanAccessServer check before allowing attachment —
// IsStreamAuthorizedForUser only knows about creator/admin, so without this
// dual gate an admin's server-limited PAT can hijack any stream by knowing
// the streamId.
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

// RevokeStreamsForServer tears down every IOStream whose targetServerID
// matches serverID. Called by the singleton package via the
// ServerTransferStreamRevocationHook on every transfer ownership
// transition — a stream the previous owner had open against this server
// must not survive into the new tenant, otherwise terminal/file-manager/NAT
// sessions become post-transfer hijack channels (effectively RCE).
//
// Underlying IO pipes are closed inline so the dashboard websocket loop
// sees EOF immediately rather than at the next idle-timeout.
func (s *NezhaHandler) RevokeStreamsForServer(serverID uint64) {
	if serverID == 0 {
		return
	}
	s.ioStreamMutex.Lock()
	defer s.ioStreamMutex.Unlock()
	for streamId, ctx := range s.ioStreams {
		if ctx.targetServerID != serverID {
			continue
		}
		if ctx.userIo != nil {
			ctx.userIo.Close()
		}
		if ctx.agentIo != nil {
			ctx.agentIo.Close()
		}
		delete(s.ioStreams, streamId)
	}
}

// RevokeStreamsForPurpose tears down every IOStream tagged with the given
// purpose. Used as the IOStream half of the MCP kill switch: when the
// admin flips EnableMCP=false, any in-flight fs.transfer / fs.upload /
// fs.download must drop immediately rather than wait out the 5min IO
// timeout. Returns the number of streams revoked so the caller can log
// the blast radius.
func (s *NezhaHandler) RevokeStreamsForPurpose(purpose StreamPurpose) int {
	s.ioStreamMutex.Lock()
	defer s.ioStreamMutex.Unlock()
	revoked := 0
	for streamId, ctx := range s.ioStreams {
		if ctx.purpose != purpose {
			continue
		}
		ctx.revokedOnce.Do(func() {
			if ctx.revokedCh != nil {
				close(ctx.revokedCh)
			}
		})
		if ctx.userIo != nil {
			ctx.userIo.Close()
		}
		if ctx.agentIo != nil {
			ctx.agentIo.Close()
		}
		delete(s.ioStreams, streamId)
		revoked++
	}
	return revoked
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

// UserConnected publishes the user-side IO under ioStreamMutex so concurrent
// Revoke* / WaitForAgent / StartStream see a consistent stream view.
// Without the lock, the bare assignment to stream.userIo races with the
// revoker's lock-protected read and triggers go-race.
func (s *NezhaHandler) UserConnected(streamId string, userIo io.ReadWriteCloser) error {
	s.ioStreamMutex.Lock()
	stream, ok := s.ioStreams[streamId]
	if !ok {
		s.ioStreamMutex.Unlock()
		return errors.New("stream not found")
	}
	stream.userIo = userIo
	s.ioStreamMutex.Unlock()
	stream.userIoChOnce.Do(func() {
		close(stream.userIoConnectCh)
	})
	return nil
}

// AgentConnected is the agent-side dual of UserConnected. Same locking
// rationale.
func (s *NezhaHandler) AgentConnected(streamId string, agentIo io.ReadWriteCloser) error {
	s.ioStreamMutex.Lock()
	stream, ok := s.ioStreams[streamId]
	if !ok {
		s.ioStreamMutex.Unlock()
		return errors.New("stream not found")
	}
	stream.agentIo = agentIo
	s.ioStreamMutex.Unlock()
	stream.agentIoChOnce.Do(func() {
		close(stream.agentIoConnectCh)
	})
	return nil
}

// streamEndpoints returns the user/agent IO under ioStreamMutex so callers
// never read the interface fields while UserConnected/AgentConnected write them.
func (s *NezhaHandler) streamEndpoints(stream *ioStreamContext) (userIo, agentIo io.ReadWriteCloser) {
	s.ioStreamMutex.RLock()
	defer s.ioStreamMutex.RUnlock()
	return stream.userIo, stream.agentIo
}

func (s *NezhaHandler) StartStream(streamId string, timeout time.Duration) error {
	stream, err := s.GetStream(streamId)
	if err != nil {
		return err
	}

	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

LOOP:
	for {
		select {
		case <-stream.userIoConnectCh:
			if _, agentIo := s.streamEndpoints(stream); agentIo != nil {
				break LOOP
			}
		case <-stream.agentIoConnectCh:
			if userIo, _ := s.streamEndpoints(stream); userIo != nil {
				break LOOP
			}
		case <-timeoutTimer.C:
			break LOOP
		}
		time.Sleep(time.Millisecond * 500)
	}

	userIo, agentIo := s.streamEndpoints(stream)
	if userIo == nil && agentIo == nil {
		return singleton.Localizer.ErrorT("timeout: no connection established")
	}
	if userIo == nil {
		return singleton.Localizer.ErrorT("timeout: user connection not established")
	}
	if agentIo == nil {
		return singleton.Localizer.ErrorT("timeout: agent connection not established")
	}

	errCh := make(chan error, 2)

	go func() {
		bp := bufPool.Get().(*bp)
		defer bufPool.Put(bp)
		_, innerErr := io.CopyBuffer(userIo, agentIo, bp.buf)
		errCh <- innerErr
	}()
	go func() {
		bp := bufPool.Get().(*bp)
		defer bufPool.Put(bp)
		_, innerErr := io.CopyBuffer(agentIo, userIo, bp.buf)
		errCh <- innerErr
	}()

	return <-errCh
}
