package rpc

import (
	"github.com/nezhahq/nezha/proto"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func TestDispatchTaskSendsOnlyProbeTypes(t *testing.T) {
	originalServerShared := singleton.ServerShared
	originalUserInfo := singleton.UserInfoMap
	t.Cleanup(func() {
		singleton.ServerShared = originalServerShared
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfo
		singleton.UserLock.Unlock()
	})

	server := &model.Server{Common: model.Common{ID: 1, UserID: 100}}
	sent := make(chan struct{})
	stream := &serveNATTaskStream{onSend: func(*proto.Task) error { close(sent); return nil }}
	server.SetTaskStream(stream)
	serverShared := singleton.NewEmptyServerClassForTest()
	serverShared.InsertForTest(server)
	singleton.ServerShared = serverShared
	singleton.UserLock.Lock()
	singleton.UserInfoMap = map[uint64]model.UserInfo{100: {Role: model.RoleMember}}
	singleton.UserLock.Unlock()

	bus := make(chan *model.Service, 8)
	done := make(chan struct{})
	go func() {
		DispatchTask(bus)
		close(done)
	}()
	for _, taskType := range []uint8{model.TaskTypeCommand, model.TaskTypeApplyConfig, model.TaskTypeExec, 255} {
		bus <- &model.Service{
			Common:      model.Common{ID: uint64(taskType), UserID: 100},
			Type:        taskType,
			Cover:       model.ServiceCoverIgnoreAll,
			SkipServers: map[uint64]bool{1: true},
		}
	}
	bus <- &model.Service{
		Common:      model.Common{ID: 1000, UserID: 100},
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	}
	close(bus)
	<-done
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("probe was not delivered")
	}

	require.Len(t, stream.sent, 1)
	require.Equal(t, uint64(model.TaskTypeTCPPing), stream.sent[0].GetType())
	require.Equal(t, uint64(1000), stream.sent[0].GetId())
}
