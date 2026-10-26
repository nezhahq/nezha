package rpc

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/proto"
	rpcService "github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type serveNATFixture struct {
	handler    *rpcService.NezhaHandler
	server     *model.Server
	taskStream *serveNATTaskStream
}

func newServeNATFixture(t *testing.T) serveNATFixture {
	t.Helper()
	originalUsers := singleton.UserInfoMap
	originalConf := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{DashboardHost: "panel.example.test"}}}
	singleton.UserInfoMap = map[uint64]model.UserInfo{1: {Role: model.RoleAdmin}, 100: {Role: model.RoleMember}}
	originalDB, originalServerShared, originalHandler := singleton.DB, singleton.ServerShared, rpcService.NezhaHandlerSingleton
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(model.Server{}))
	server := &model.Server{Common: model.Common{ID: 7, UserID: 1}, UUID: "serve-nat-test", Name: "serve-nat-test"}
	require.NoError(t, db.Create(server).Error)
	singleton.DB = db
	singleton.ServerShared = singleton.NewServerClass()
	handler := rpcService.NewNezhaHandler()
	rpcService.NezhaHandlerSingleton = handler
	taskStream := &serveNATTaskStream{}
	server, ok := singleton.ServerShared.Get(server.ID)
	require.True(t, ok)
	server.SetTaskStream(taskStream)
	taskStream.server = server
	t.Cleanup(func() {
		singleton.UserInfoMap = originalUsers
		singleton.Conf = originalConf
		rpcService.NezhaHandlerSingleton, singleton.ServerShared, singleton.DB = originalHandler, originalServerShared, originalDB
		if dbSQL, dbErr := db.DB(); dbErr == nil {
			_ = dbSQL.Close()
		}
	})
	return serveNATFixture{handler: handler, server: server, taskStream: taskStream}
}

type serveNATTaskStream struct {
	server  *model.Server
	onSend  func(*proto.Task) error
	sendErr error
	sent    []*proto.Task
}

func (stream *serveNATTaskStream) Send(task *proto.Task) error {
	stream.sent = append(stream.sent, task)
	if stream.sendErr != nil {
		return stream.sendErr
	}
	if stream.onSend != nil {
		return stream.onSend(task)
	}
	return nil
}
func (*serveNATTaskStream) Recv() (*proto.TaskResult, error) { return nil, io.EOF }
func (*serveNATTaskStream) SetHeader(metadata.MD) error      { return nil }
func (*serveNATTaskStream) SendHeader(metadata.MD) error     { return nil }
func (*serveNATTaskStream) SetTrailer(metadata.MD)           {}
func (*serveNATTaskStream) Context() context.Context         { return context.Background() }
func (*serveNATTaskStream) SendMsg(any) error                { return nil }
func (*serveNATTaskStream) RecvMsg(any) error                { return io.EOF }

type serveNATResponseWriter struct {
	conn           *serveNATConn
	prefetched     []byte
	header         http.Header
	status, writes int
	body           string
}

func (writer *serveNATResponseWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}
func (writer *serveNATResponseWriter) Write(data []byte) (int, error) {
	writer.writes += len(data)
	writer.body += string(data)
	return len(data), nil
}
func (writer *serveNATResponseWriter) WriteHeader(status int) { writer.status = status }
func (writer *serveNATResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	go func() { _, _ = writer.conn.Read(make([]byte, 1)) }()
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(writer.prefetched), writer.conn))
	if len(writer.prefetched) > 0 {
		_, _ = reader.Peek(len(writer.prefetched))
	}
	return writer.conn, bufio.NewReadWriter(reader, bufio.NewWriter(writer.conn)), nil
}

type serveNATBody struct{ closeCount atomic.Int32 }

func (*serveNATBody) Read([]byte) (int, error) { return 0, io.EOF }
func (body *serveNATBody) Close() error        { body.closeCount.Add(1); return nil }

type serveNATConn struct {
	deadlineErr             error
	closed, readDone        chan struct{}
	readDoneOnce, closeOnce sync.Once
	closeCount              atomic.Int32
}

func newServeNATConn() *serveNATConn {
	return &serveNATConn{closed: make(chan struct{}), readDone: make(chan struct{})}
}
func (conn *serveNATConn) Read([]byte) (int, error) {
	<-conn.closed
	conn.readDoneOnce.Do(func() { close(conn.readDone) })
	return 0, io.EOF
}
func (*serveNATConn) Write(data []byte) (int, error) { return len(data), nil }
func (conn *serveNATConn) Close() error {
	conn.closeCount.Add(1)
	conn.closeOnce.Do(func() { close(conn.closed) })
	return nil
}
func (*serveNATConn) LocalAddr() net.Addr              { return serveNATAddr("local") }
func (*serveNATConn) RemoteAddr() net.Addr             { return serveNATAddr("remote") }
func (conn *serveNATConn) SetDeadline(time.Time) error { return conn.deadlineErr }
func (*serveNATConn) SetReadDeadline(time.Time) error  { return nil }
func (*serveNATConn) SetWriteDeadline(time.Time) error { return nil }

type serveNATAddr string

func (addr serveNATAddr) Network() string { return "test" }
func (addr serveNATAddr) String() string  { return string(addr) }

type serveNATAgent struct {
	mu         sync.Mutex
	written    bytes.Buffer
	readErr    error
	writeDone  chan struct{}
	writeOnce  sync.Once
	closeCount atomic.Int32
}

func (agent *serveNATAgent) Read([]byte) (int, error) {
	if agent.writeDone != nil {
		<-agent.writeDone
	}
	return 0, agent.readErr
}
func (agent *serveNATAgent) Write(data []byte) (int, error) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	count, err := agent.written.Write(data)
	if agent.writeDone != nil {
		agent.writeOnce.Do(func() { close(agent.writeDone) })
	}
	return count, err
}
func (agent *serveNATAgent) Close() error { agent.closeCount.Add(1); return nil }
func (agent *serveNATAgent) writtenBytes() []byte {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return append([]byte(nil), agent.written.Bytes()...)
}

var _ proto.NezhaService_RequestTaskServer = (*serveNATTaskStream)(nil)
var _ http.Hijacker = (*serveNATResponseWriter)(nil)
var _ net.Conn = (*serveNATConn)(nil)
var _ io.ReadWriteCloser = (*serveNATAgent)(nil)
