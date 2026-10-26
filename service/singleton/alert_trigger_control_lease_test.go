package singleton

import (
	"context"
	"testing"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/stretchr/testify/require"
)

type stalledAlertCommandStream struct {
	pb.NezhaService_RequestTaskServer
	entered chan struct{}
	resume  chan struct{}
}

func (s *stalledAlertCommandStream) Context() context.Context { return context.Background() }
func (s *stalledAlertCommandStream) Send(*pb.Task) error      { close(s.entered); <-s.resume; return nil }

func TestAlertCommandKeepsControlAfterSendTimeout(t *testing.T) {
	stream := &stalledAlertCommandStream{entered: make(chan struct{}), resume: make(chan struct{})}
	server := &model.Server{Common: model.Common{ID: 7, UserID: 100}, Name: "alert-command-subject"}
	server.SetTaskStream(stream)
	replaceServerSharedForSecurityTest(t, server)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{100: {Role: model.RoleMember}})
	previousCron, previousTransfer := CronShared, ServerTransferShared
	CronShared, ServerTransferShared = nil, nil
	t.Cleanup(func() { CronShared, ServerTransferShared = previousCron, previousTransfer })
	CronTrigger(&model.Cron{Common: model.Common{ID: 42, UserID: 100}, Cover: model.CronCoverAlertTrigger}, server.ID)()
	<-stream.entered
	lock := &serverControlLocks[server.ID%uint64(len(serverControlLocks))]
	admittedTransition := lock.TryLock()
	if admittedTransition {
		lock.Unlock()
	}
	close(stream.resume)
	// Wait for the actual sender's lease cleanup before restoring global fixtures.
	lock.Lock()
	lock.Unlock()
	require.False(t, admittedTransition, "ownership transition was admitted while the alert command was still sending")
}
