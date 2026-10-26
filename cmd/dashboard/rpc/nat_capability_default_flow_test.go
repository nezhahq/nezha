//go:build !agentcompat

package rpc

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/agentcompatcontract"
	"github.com/nezhahq/nezha/proto"
)

func TestServeNATDefaultForwardsCapabilityHeaderAsOrdinaryData(t *testing.T) {
	fixture := newServeNATFixture(t)
	installNATCredentialTestGate(t)
	connection := newServeNATConn()
	agent := &serveNATAgent{readErr: io.EOF, writeDone: make(chan struct{})}
	request := &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Scheme: "http", Host: "example.test", Path: "/nat"},
		Header: make(http.Header),
		Body:   &serveNATBody{},
	}
	request.Header.Set(agentcompatcontract.IOStreamCapabilityHeader, "ordinary-data")
	request.Header.Set("Authorization", natTestForeignRandom)
	fixture.taskStream.onSend = func(task *proto.Task) error {
		var nat model.TaskNAT
		if err := json.Unmarshal([]byte(task.Data), &nat); err != nil {
			return err
		}
		return fixture.handler.AgentConnected(nat.StreamID, agent)
	}
	done := make(chan struct{})
	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		ServeNAT(&serveNATResponseWriter{conn: connection}, request, &model.NAT{Common: model.Common{UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})
		close(done)
	}()
	select {
	case <-agent.writeDone:
	case <-deadline.Done():
		t.Fatal("agent did not receive ordinary NAT request")
	}
	_ = connection.Close()
	select {
	case <-done:
	case <-deadline.Done():
		t.Fatal("ServeNAT did not finish")
	}
	forwarded := strings.ToLower(string(agent.writtenBytes()))
	if !strings.Contains(forwarded, strings.ToLower(agentcompatcontract.IOStreamCapabilityHeader)+": ordinary-data") {
		t.Fatal("default NAT flow did not forward capability header as ordinary request data")
	}
	if !strings.Contains(forwarded, strings.ToLower("Authorization: "+natTestForeignRandom)) {
		t.Fatal("default NAT flow stripped a foreign credential that does not belong to the dashboard")
	}
}

func TestServeNATDefaultDoesNotForwardDashboardCredentials(t *testing.T) {
	fixture := newServeNATFixture(t)
	installNATCredentialTestGate(t)
	connection := newServeNATConn()
	agent := &serveNATAgent{readErr: io.EOF, writeDone: make(chan struct{})}
	request := &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Scheme: "http", Host: "example.test", Path: "/nat"},
		Header: make(http.Header),
		Body:   &serveNATBody{},
	}
	request.Header.Set("Authorization", natTestDashboardJWT)
	fixture.taskStream.onSend = func(task *proto.Task) error {
		var nat model.TaskNAT
		if err := json.Unmarshal([]byte(task.Data), &nat); err != nil {
			return err
		}
		return fixture.handler.AgentConnected(nat.StreamID, agent)
	}
	done := make(chan struct{})
	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		ServeNAT(&serveNATResponseWriter{conn: connection}, request, &model.NAT{Common: model.Common{UserID: 1}, ServerID: fixture.server.ID, Host: "target.example"})
		close(done)
	}()
	select {
	case <-agent.writeDone:
	case <-deadline.Done():
		t.Fatal("agent did not receive ordinary NAT request")
	}
	_ = connection.Close()
	select {
	case <-done:
	case <-deadline.Done():
		t.Fatal("ServeNAT did not finish")
	}
	forwarded := strings.ToLower(string(agent.writtenBytes()))
	if strings.Contains(forwarded, "authorization:") || strings.Contains(forwarded, "dashboard-signed-jwt") {
		t.Fatal("default NAT flow forwarded dashboard credentials")
	}
}
