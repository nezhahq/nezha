package rpc

import (
	"io"
	"reflect"
	"testing"
	"time"
)

func TestIOStream(t *testing.T) {
	handler := NewNezhaHandler()

	const testStreamID = "ffffffff-ffff-ffff-ffff-ffffffffffff"

	handler.CreateStream(testStreamID)
	userIo, agentIo := newPipeReadWriter(), newPipeReadWriter()
	defer func() {
		userIo.Close()
		agentIo.Close()
	}()

	handler.AgentConnected(testStreamID, agentIo)
	handler.UserConnected(testStreamID, userIo)

	go handler.StartStream(testStreamID, time.Second*10)

	cases := [][]byte{
		{0, 9, 1, 3, 2, 9, 1, 4, 8},
		{3, 1, 3, 5, 2, 9, 5, 13, 53, 23},
		make([]byte, 1024),
		make([]byte, 1024*1024),
	}

	t.Run("WriteUserIO", func(t *testing.T) {
		for i, c := range cases {
			_, err := userIo.Write(c)
			if err != nil {
				t.Fatalf("write to userIo failed at case %d: %v", i, err)
			}

			b := make([]byte, len(c))
			n, err := agentIo.Read(b)
			if err != nil {
				t.Fatalf("read agentIo failed at case %d: %v", i, err)
			}

			if !reflect.DeepEqual(c, b[:n]) {
				t.Fatalf("expected %v, but got %v", c, b[:n])
			}
		}
	})

	t.Run("WriteAgentIO", func(t *testing.T) {
		for i, c := range cases {
			_, err := agentIo.Write(c)
			if err != nil {
				t.Fatalf("write to agentIo failed at case %d: %v", i, err)
			}

			b := make([]byte, len(c))
			n, err := userIo.Read(b)
			if err != nil {
				t.Fatalf("read userIo failed at case %d: %v", i, err)
			}

			if !reflect.DeepEqual(c, b[:n]) {
				t.Fatalf("Expected %v, but got %v", c, b[:n])
			}
		}
	})

	t.Run("WriteUserIOReadTwice", func(t *testing.T) {
		data := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		_, err := agentIo.Write(data)
		if err != nil {
			t.Fatalf("write to agentIo failed: %v", err)
		}

		b := make([]byte, len(data)/2)
		n, err := userIo.Read(b)
		if err != nil {
			t.Fatalf("read userIo failed: %v", err)
		}

		b2 := make([]byte, len(data)-n)
		_, err = userIo.Read(b2)
		if err != nil {
			t.Fatalf("read userIo failed: %v", err)
		}

		if !reflect.DeepEqual(data[:len(data)/2], b) {
			t.Fatalf("expected %v, but got %v", data[:len(data)/2], b)
		}

		if !reflect.DeepEqual(data[len(data)/2:], b2) {
			t.Fatalf("expected %v, but got %v", data[len(data)/2:], b2)
		}
	})
}

func newPipeReadWriter() io.ReadWriteCloser {
	r, w := io.Pipe()
	return struct {
		io.Reader
		io.WriteCloser
	}{r, w}
}
