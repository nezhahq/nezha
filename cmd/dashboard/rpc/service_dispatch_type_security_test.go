package rpc

import (
	"github.com/nezhahq/nezha/proto"
	"strings"
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

func TestDispatchTaskRejectsOversizedTarget(t *testing.T) {
	originalServerShared := singleton.ServerShared
	originalUserInfo := singleton.UserInfoMap
	t.Cleanup(func() {
		singleton.ServerShared = originalServerShared
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfo
		singleton.UserLock.Unlock()
	})

	server := &model.Server{Common: model.Common{ID: 1, UserID: 100}}
	sent := make(chan uint64, 2)
	stream := &serveNATTaskStream{onSend: func(task *proto.Task) error { sent <- task.GetId(); return nil }}
	server.SetTaskStream(stream)
	serverShared := singleton.NewEmptyServerClassForTest()
	serverShared.InsertForTest(server)
	singleton.ServerShared = serverShared
	singleton.UserLock.Lock()
	singleton.UserInfoMap = map[uint64]model.UserInfo{100: {Role: model.RoleMember}}
	singleton.UserLock.Unlock()

	// A row that reached memory without passing Service.BeforeSave must not be
	// encoded once per covered Agent; the largest allowed target still is.
	bus := make(chan *model.Service, 2)
	for id, size := range []int{model.MaxServiceTargetBytes + 1, model.MaxServiceTargetBytes} {
		bus <- &model.Service{
			Common: model.Common{ID: uint64(id + 1), UserID: 100},
			Type:   model.TaskTypeTCPPing,
			Target: strings.Repeat("4", size),
			Cover:  model.ServiceCoverAll,
		}
	}
	close(bus)
	DispatchTask(bus)

	// Tasks of one connection are delivered in order, so the first one seen
	// is the oversized probe if it was queued at all.
	select {
	case id := <-sent:
		require.Equal(t, uint64(2), id, "the oversized probe must not be sent")
	case <-time.After(time.Second):
		t.Fatal("probe with the largest allowed target was not delivered")
	}
}
