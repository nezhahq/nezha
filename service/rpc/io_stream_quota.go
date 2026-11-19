package rpc

import (
	"errors"
	"time"
)

const (
	maxStreamsPerUser   = 20
	maxStreamsPerServer = 40
	// A closed stream no longer counts toward the caps above, so the agent
	// RPCs of closed streams that have not ended yet are bounded separately:
	// each gets ioStreamCloseGrace to end by itself, and no more than the
	// maxClosingStreams most recently closed ones are left waiting for it.
	maxClosingStreams  = maxStreamsPerUser
	ioStreamCloseGrace = 2 * time.Second
)

var (
	ErrTooManyStreamsForUser   = errors.New("too many concurrent streams for this user")
	ErrTooManyStreamsForServer = errors.New("too many concurrent streams for this server")
	ErrStreamAlreadyExists     = errors.New("stream already exists")
)

func (s *NezhaHandler) CreateStream(streamId string, creatorUserID uint64, targetServerID uint64) error {
	return s.CreateStreamWithPurpose(streamId, creatorUserID, targetServerID, PurposeLegacy)
}

func (s *NezhaHandler) CreateStreamWithPurpose(streamId string, creatorUserID uint64, targetServerID uint64, purpose StreamPurpose) error {
	s.ioStreamMutex.Lock()
	defer s.ioStreamMutex.Unlock()
	return s.createStreamLocked(streamId, creatorUserID, targetServerID, purpose)
}

func (s *NezhaHandler) createStreamLocked(streamId string, creatorUserID uint64, targetServerID uint64, purpose StreamPurpose) error {
	if _, exists := s.ioStreams[streamId]; exists {
		// Stream IDs identify live relay ownership; never overwrite one or orphan its endpoint.
		return ErrStreamAlreadyExists
	}

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

	s.ioStreams[streamId] = newIOStreamContext(creatorUserID, targetServerID, purpose)
	s.publishIOStreamStateChangeLocked()
	return nil
}

// StreamCount reports the registry size under the same lock used by lifecycle mutations.
func (s *NezhaHandler) StreamCount() int {
	s.ioStreamMutex.RLock()
	defer s.ioStreamMutex.RUnlock()
	return len(s.ioStreams)
}
