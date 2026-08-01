package rpc

import (
	"context"
	"errors"
	"testing"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// When a server is edited mid-session, updateServer swaps a new *Server into
// ServerShared that adopts the live stream holder. The agent's RequestTask
// cleanup must detach the stream from whichever *Server is currently published,
// not the stale object captured when the stream attached — otherwise the new
// object keeps reporting the agent as online on a dead stream.
func TestRequestTaskCleanupDetachesStreamFromCurrentServerAfterEdit(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "ffffffff-ffff-ffff-ffff-ffffffffffff")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, nil, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	old, ok := singleton.ServerShared.Get(reporter.ID)
	if !ok {
		t.Fatalf("server %d not found", reporter.ID)
	}

	stream := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	stream.onRecv = func() {
		edited := &model.Server{Common: model.Common{ID: old.ID, UserID: old.UserID}, UUID: old.UUID, Name: "edited"}
		edited.CopyFromRunningServer(old)
		singleton.ServerShared.Update(edited, "")
	}

	if err := NewNezhaHandler().RequestTask(stream); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected RequestTask to finish after Recv error, got %v", err)
	}

	current, ok := singleton.ServerShared.Get(reporter.ID)
	if !ok {
		t.Fatalf("server %d not found after edit", reporter.ID)
	}
	if got := current.GetTaskStream(); got != nil {
		t.Fatalf("edited server must report offline after the agent stream dropped, got %T", got)
	}
}

func TestRequestTaskRejectsResultWhenServerDeletedAfterRecv(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "10101010-1010-1010-1010-101010101010")
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverAll, nil)
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	stream := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	stream.results = []*pb.TaskResult{cronTaskResult(cronTask.ID, true)}
	stream.onResult = func() {
		singleton.ServerShared.Delete([]uint64{reporter.ID})
	}

	err := NewNezhaHandler().RequestTask(stream)
	if !errors.Is(err, ErrRequestTaskStreamSuperseded) {
		t.Fatalf("expected stale RequestTask stream error, got %v", err)
	}
	assertCronResultNotUpdated(t, cronTask.ID)
}

func TestRequestTaskRejectsResultWhenNewerStreamSupersedesOld(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "20202020-2020-2020-2020-202020202020")
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverAll, nil)
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	current, ok := singleton.ServerShared.Get(reporter.ID)
	if !ok {
		t.Fatalf("server %d not found", reporter.ID)
	}
	newer := &requestTaskSecurityStream{ctx: context.Background()}
	stream := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	stream.results = []*pb.TaskResult{cronTaskResult(cronTask.ID, true)}
	stream.onResult = func() {
		current.SetTaskStream(newer)
	}

	err := NewNezhaHandler().RequestTask(stream)
	if !errors.Is(err, ErrRequestTaskStreamSuperseded) {
		t.Fatalf("expected superseded RequestTask stream error, got %v", err)
	}
	if got := current.GetTaskStream(); got != newer {
		t.Fatalf("old stream cleanup must preserve newer stream, got %T", got)
	}
	assertCronResultNotUpdated(t, cronTask.ID)
}

func TestRequestTaskAcceptsResultAfterServerPointerReplacementWithSameStream(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "30303030-3030-3030-3030-303030303030")
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverAll, nil)
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	old, ok := singleton.ServerShared.Get(reporter.ID)
	if !ok {
		t.Fatalf("server %d not found", reporter.ID)
	}
	stream := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	stream.results = []*pb.TaskResult{cronTaskResult(cronTask.ID, true)}
	stream.onResult = func() {
		replacement := &model.Server{Common: model.Common{ID: old.ID, UserID: old.UserID}, UUID: old.UUID, Name: "replacement"}
		replacement.CopyFromRunningServer(old)
		singleton.ServerShared.Update(replacement, "")
	}

	err := NewNezhaHandler().RequestTask(stream)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected RequestTask to finish after accepted result, got %v", err)
	}
	if !cronLastResult(t, cronTask.ID) {
		t.Fatal("result on a replacement server that inherited the stream must be accepted")
	}
}

func assertCronResultNotUpdated(t *testing.T, cronID uint64) {
	t.Helper()

	var cronTask model.Cron
	if err := singleton.DB.First(&cronTask, cronID).Error; err != nil {
		t.Fatal(err)
	}
	if cronTask.LastResult || !cronTask.LastExecutedAt.IsZero() {
		t.Fatalf("stale RequestTask result must not mutate cron, got last_result=%t last_executed_at=%s", cronTask.LastResult, cronTask.LastExecutedAt)
	}
}
