package rpc

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/nezhahq/nezha/pkg/grpcx"
	pb "github.com/nezhahq/nezha/proto"
)

func (s *NezhaHandler) IOStream(stream pb.NezhaService_IOStreamServer) error {
	clientID, err := s.Auth.Check(stream.Context())
	if err != nil {
		return err
	}
	id, err := stream.Recv()
	if err != nil {
		return err
	}
	if id == nil || !isValidIOStreamMagic(id.Data) {
		return fmt.Errorf("invalid stream id")
	}
	streamID := string(id.Data[4:])
	if !s.IsStreamAuthorizedForAgent(streamID, clientID) {
		return fmt.Errorf("stream not authorized for agent")
	}
	if _, err := s.GetStream(streamID); err != nil {
		return err
	}
	wrapper := grpcx.NewIOStreamWrapper(stream)
	keepaliveDone := make(chan struct{})
	go func() {
		defer close(keepaliveDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-wrapper.Context().Done():
				return
			case <-wrapper.Done():
				return
			case <-ticker.C:
				if err := wrapper.SendKeepalive(); err != nil {
					log.Printf("NEZHA>> IOStream keepAlive error: %v\n", err)
					return
				}
			}
		}
	}()
	if err := s.AgentConnected(streamID, wrapper); err != nil {
		_ = wrapper.Close()
		return err
	}
	wrapper.Wait()
	s.endClosedIOStream(stream.Context())
	<-keepaliveDone
	return nil
}

// endClosedIOStream makes sure the RPC of a closed session ends with it. The
// session's quota slot is already free, but an agent that stopped reading keeps
// the relay's write, its buffer and the status IOStream is about to return
// queued behind a response write parked on flow control. The stream is reset
// unless the RPC finishes by itself within ioStreamCloseGrace, and only the
// maxClosingStreams most recently closed sessions get that grace, so what
// closed sessions pin stays bounded however fast they are opened and closed.
func (s *NezhaHandler) endClosedIOStream(ctx context.Context) {
	abort := grpcx.StreamAbort(ctx)
	if abort == nil {
		return
	}
	time.AfterFunc(ioStreamCloseGrace, abort)
	for {
		select {
		case s.ioStreamAborts <- abort:
			return
		default:
		}
		// Full: the oldest one has waited longest. Resetting it does nothing
		// if its RPC has ended, which a healthy one does right away.
		select {
		case oldest := <-s.ioStreamAborts:
			oldest()
		default:
		}
	}
}
