package controller

import (
	"context"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type websocketPingWriter interface {
	WriteMessage(messageType int, data []byte) error
}

type websocketPingConnection interface {
	websocketPingWriter
	Close() error
}

type websocketPingTransport struct {
	websocketPingWriter
	closeOnce sync.Once
	closeErr  error
	close     func() error
}

func newWebsocketPingTransport(writer websocketPingWriter, close func() error) *websocketPingTransport {
	return &websocketPingTransport{websocketPingWriter: writer, close: close}
}

func (transport *websocketPingTransport) Close() error {
	transport.closeOnce.Do(func() { transport.closeErr = transport.close() })
	return transport.closeErr
}

func websocketPingLoop(ctx context.Context, ticks <-chan time.Time, writer websocketPingWriter) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticks:
			if !ok {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if err := writer.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
			return err
		}
	}
}

func startWebsocketPing(ctx context.Context, ticks <-chan time.Time, connection websocketPingConnection) func() {
	workerContext, cancel := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	closeConnection := sync.OnceFunc(func() { _ = connection.Close() })
	go func() {
		defer close(workerDone)
		if err := websocketPingLoop(workerContext, ticks, connection); err != nil {
			// The peer is gone, and nothing else is guaranteed to notice: a
			// relay blocked on the agent never touches the WebSocket again.
			closeConnection()
		}
	}()
	return func() {
		closeConnection()
		cancel()
		<-workerDone
	}
}

func startWebsocketPingTicker(ctx context.Context, interval time.Duration, connection websocketPingConnection) func() {
	ticker := time.NewTicker(interval)
	stop := startWebsocketPing(ctx, ticker.C, connection)
	return func() {
		ticker.Stop()
		stop()
	}
}
