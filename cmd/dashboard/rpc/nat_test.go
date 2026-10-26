package rpc

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/proto"
)

func TestServeNATClosesHijackedResourcesWhenStreamDisappearsBeforeUserTransfer(t *testing.T) {
	fixture := newServeNATFixture(t)
	conn := newServeNATConn()
	body := &serveNATBody{}
	writer := &serveNATResponseWriter{conn: conn}
	request := &http.Request{Method: http.MethodPost, URL: &url.URL{Scheme: "http", Host: "example.test", Path: "/nat"}, Header: make(http.Header), Body: body, ContentLength: 0}
	request.Header.Set("X-NAT-Test", "failure")
	fixture.taskStream.onSend = func(task *proto.Task) error {
		var nat model.TaskNAT
		if err := json.Unmarshal([]byte(task.Data), &nat); err != nil {
			return err
		}
		return fixture.handler.CloseStream(nat.StreamID)
	}

	ServeNAT(writer, request, &model.NAT{Common: model.Common{UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})

	if got := body.closeCount.Load(); got != 0 {
		t.Fatal("original net/http body must not be used after hijack")
	}
	if got := conn.closeCount.Load(); got != 1 {
		t.Fatalf("hijacked connection close count = %d, want 1", got)
	}
	select {
	case <-conn.readDone:
	case <-time.After(time.Second):
		t.Fatal("hijacked connection read remained blocked after failed transfer")
	}
	if writer.status != 0 || writer.writes != 0 {
		t.Fatalf("hijacked failure wrote HTTP response: status=%d writes=%d", writer.status, writer.writes)
	}
}

func TestServeNATTransfersRequestAndRegistryOwnsSuccessfulCleanup(t *testing.T) {
	fixture := newServeNATFixture(t)
	conn := newServeNATConn()
	body := &serveNATBody{}
	agent := &serveNATAgent{readErr: io.EOF, writeDone: make(chan struct{})}
	writer := &serveNATResponseWriter{conn: conn}
	request := &http.Request{Method: http.MethodPost, URL: &url.URL{Scheme: "http", Host: "example.test", Path: "/nat"}, Header: make(http.Header), Body: body, ContentLength: 0}
	request.Header.Set("X-NAT-Test", "success")
	fixture.taskStream.onSend = func(task *proto.Task) error {
		var nat model.TaskNAT
		if err := json.Unmarshal([]byte(task.Data), &nat); err != nil {
			return err
		}
		return fixture.handler.AgentConnected(nat.StreamID, agent)
	}

	serveDone := make(chan struct{})
	go func() {
		ServeNAT(writer, request, &model.NAT{Common: model.Common{UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})
		close(serveDone)
	}()

	select {
	case <-agent.writeDone:
	case <-time.After(time.Second):
		t.Fatal("agent did not receive the transferred NAT request")
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("ServeNAT did not finish after the successful stream was closed")
	}
	if got := body.closeCount.Load(); got != 0 {
		t.Fatal("original net/http body must not be used after hijack")
	}
	if got := conn.closeCount.Load(); got != 1 {
		t.Fatalf("registry-owned hijacked connection close count = %d, want 1", got)
	}
	if got := agent.closeCount.Load(); got != 1 {
		t.Fatalf("registry-owned agent close count = %d, want 1", got)
	}
	if got := strings.ToLower(string(agent.writtenBytes())); !strings.Contains(got, "post /nat http/1.1") || !strings.Contains(got, "x-nat-test: success") {
		t.Fatalf("agent received incomplete NAT request: %q", got)
	}
}
