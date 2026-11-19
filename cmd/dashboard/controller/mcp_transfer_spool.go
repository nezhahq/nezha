package controller

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"sync"
)

// transferSpoolBuffer sizes the write buffer in front of the spool tmpfile.
const transferSpoolBuffer = 64 << 10

type transferSpool struct {
	f *os.File
	// The agent chooses how the payload is cut into Write calls (one per NZTC
	// frame), so writes are buffered: the number of write(2) calls must follow
	// the payload size, not the agent's framing. Rewind flushes.
	w         *bufio.Writer
	closeOnce sync.Once
}

func newTransferSpool() (*transferSpool, error) {
	f, err := os.CreateTemp("", "nz-mcp-xfer-*")
	if err != nil {
		return nil, err
	}
	// 提前 unlink，文件句柄关掉就回收磁盘；Windows 不支持就留到 Close 兜底。
	if runtime.GOOS != "windows" {
		_ = os.Remove(f.Name())
	}
	return &transferSpool{f: f, w: bufio.NewWriterSize(f, transferSpoolBuffer)}, nil
}

func (s *transferSpool) Write(p []byte) (int, error) { return s.w.Write(p) }

func (s *transferSpool) Read(p []byte) (int, error) { return s.f.Read(p) }

func (s *transferSpool) Rewind() error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	_, err := s.f.Seek(0, io.SeekStart)
	return err
}

// Close may run while a Read or Write is in flight (relayDownloadFrames closes
// the spool when the transfer context ends under a relay that is still
// blocked), so it leaves s.f in place; later calls fail with os.ErrClosed.
func (s *transferSpool) Close() {
	s.closeOnce.Do(func() {
		_ = s.f.Close()
		if runtime.GOOS == "windows" {
			_ = os.Remove(s.f.Name())
		}
	})
}
