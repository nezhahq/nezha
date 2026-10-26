package controller

import (
	"context"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
)

type configurationLeaseTaskStream struct {
	pb.NezhaService_RequestTaskServer
	sent   chan *pb.Task
	resume chan struct{}
}

func (s *configurationLeaseTaskStream) Context() context.Context { return context.Background() }
func (s *configurationLeaseTaskStream) Send(task *pb.Task) error {
	if task.Type == model.TaskTypeServerTransferApply {
		return nil
	}
	s.sent <- task
	if s.resume != nil {
		<-s.resume
	}
	return nil
}

func setupConfigurationLeaseTransfer(t *testing.T) (*model.Server, uint64) {
	t.Helper()
	cleanup := setupRetryServerTransferFixture(t)
	previousConf, previousLoc := singleton.Conf, singleton.Loc
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{}}
	singleton.Loc = time.UTC
	t.Cleanup(func() { cleanup(); singleton.Conf, singleton.Loc = previousConf, previousLoc })
	db, err := singleton.DB.DB()
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	seedServer(t, 1, 100)
	transferID := seedPendingTransfer(t, 1, 100, 200, 100)
	server, ok := singleton.ServerShared.Get(1)
	require.True(t, ok)
	return server, transferID
}

func TestServerConfigReadReleasesControlBeforeWaitingForReply(t *testing.T) {
	server, transferID := setupConfigurationLeaseTransfer(t)
	stream := &configurationLeaseTaskStream{sent: make(chan *pb.Task, 1)}
	server.SetTaskStream(stream)
	c := ctxAs(1, model.RoleAdmin)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	type outcome struct {
		data string
		err  error
	}
	done := make(chan outcome, 1)
	go func() { data, err := getServerConfig(c); done <- outcome{data, err} }()
	require.Equal(t, uint64(model.TaskTypeReportConfig), (<-stream.sent).Type)
	rollback := make(chan error, 1)
	go func() {
		_, err := singleton.ServerTransferShared.MarkFailed(transferID, "agent rejected transfer")
		rollback <- err
		server.ConfigCache <- "agent configuration"
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Error("configuration read blocked rollback while waiting for its reply")
		server.ConfigCache <- "cleanup configuration"
		got = <-done
	}
	require.NoError(t, <-rollback)
	require.NoError(t, got.err)
	require.Equal(t, "agent configuration", got.data)
	require.Equal(t, uint64(100), server.GetUserID())
}

func TestServerConfigWriteKeepsControlUntilTimedOutSendReturns(t *testing.T) {
	server, transferID := setupConfigurationLeaseTransfer(t)
	stream := &configurationLeaseTaskStream{sent: make(chan *pb.Task, 1), resume: make(chan struct{})}
	server.SetTaskStream(stream)
	c := ctxAs(1, model.RoleAdmin)
	bindJSON(c, model.ServerConfigForm{Servers: []uint64{1}, Config: `{"debug":true}`})
	type outcome struct {
		response *model.ServerTaskResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() { response, err := setServerConfig(c); done <- outcome{response, err} }()
	task := <-stream.sent
	require.Equal(t, uint64(model.TaskTypeApplyConfig), task.Type)
	require.Equal(t, `{"debug":true}`, task.Data)
	got := <-done
	require.NoError(t, got.err)
	require.Equal(t, []uint64{1}, got.response.Failure)
	require.Empty(t, got.response.Success)
	rollback := make(chan error, 1)
	go func() {
		_, err := singleton.ServerTransferShared.MarkFailed(transferID, "agent rejected transfer")
		rollback <- err
	}()
	var early bool
	select {
	case err := <-rollback:
		require.NoError(t, err)
		early = true
	case <-time.After(100 * time.Millisecond):
	}
	close(stream.resume)
	if !early {
		require.NoError(t, <-rollback)
	}
	require.False(t, early, "ownership changed while the old configuration was still being sent")
	require.Equal(t, uint64(100), server.GetUserID())
}

func TestForceUpdateRejectsPendingTransferOwner(t *testing.T) {
	server, _ := setupConfigurationLeaseTransfer(t)
	stream := &configurationLeaseTaskStream{sent: make(chan *pb.Task, 1)}
	server.SetTaskStream(stream)
	c := ctxAs(200, model.RoleMember)
	bindJSON(c, []uint64{server.ID})
	response, err := forceUpdateServer(c)
	require.NoError(t, err)
	require.Empty(t, response.Success)
	require.Equal(t, []uint64{server.ID}, response.Failure)
	require.Empty(t, stream.sent, "the pending destination must not dispatch an Agent upgrade")
}
