package websocketx

import (
	"io"
	"sync"

	"github.com/gorilla/websocket"
)

var _ io.ReadWriteCloser = (*Conn)(nil)

type Conn struct {
	*websocket.Conn
	writeLock *sync.Mutex
	dataBuf   []byte
}

func NewConn(conn *websocket.Conn) *Conn {
	return &Conn{Conn: conn, writeLock: new(sync.Mutex)}
}

func (conn *Conn) Write(data []byte) (int, error) {
	conn.writeLock.Lock()
	defer conn.writeLock.Unlock()
	if err := conn.Conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (conn *Conn) WriteMessage(messageType int, data []byte) error {
	conn.writeLock.Lock()
	defer conn.writeLock.Unlock()
	return conn.Conn.WriteMessage(messageType, data)
}

func (conn *Conn) Read(data []byte) (int, error) {
	if len(conn.dataBuf) > 0 {
		n := copy(data, conn.dataBuf)
		conn.dataBuf = conn.dataBuf[n:]
		return n, nil
	}
	mType, innerData, err := conn.Conn.ReadMessage()
	if err != nil {
		return 0, err
	}
	// 将文本消息转换为命令输入
	if mType == websocket.TextMessage {
		innerData = append([]byte{0}, innerData...)
	}
	n := copy(data, innerData)
	if n < len(innerData) {
		conn.dataBuf = innerData[n:]
	}
	return n, nil
}
