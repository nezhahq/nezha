package singleton

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"github.com/robfig/cron/v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"google.golang.org/grpc/metadata"
)

type capturedTaskStream struct {
	tasks chan *pb.Task
}

func newCapturedTaskStream() *capturedTaskStream {
	return &capturedTaskStream{tasks: make(chan *pb.Task, 4)}
}

func (s *capturedTaskStream) Send(task *pb.Task) error {
	s.tasks <- task
	return nil
}

func (s *capturedTaskStream) Recv() (*pb.TaskResult, error) { return nil, context.Canceled }
func (s *capturedTaskStream) SetHeader(metadata.MD) error   { return nil }
func (s *capturedTaskStream) SendHeader(metadata.MD) error  { return nil }
func (s *capturedTaskStream) SetTrailer(metadata.MD)        {}
func (s *capturedTaskStream) Context() context.Context      { return context.Background() }
func (s *capturedTaskStream) SendMsg(any) error             { return nil }
func (s *capturedTaskStream) RecvMsg(any) error             { return context.Canceled }

// withTaskStream attaches a TaskStream to a freshly constructed Server using the
// new atomic accessor. The field itself is unexported (see Fix #12) precisely
// because direct struct-literal access invited torn interface reads on hot
// paths — tests use this helper rather than reaching in, mirroring production
// callsites.
func withTaskStream(s *model.Server, stream pb.NezhaService_RequestTaskServer) *model.Server {
	s.SetTaskStream(stream)
	return s
}

func replaceServerSharedForSecurityTest(t *testing.T, servers ...*model.Server) {
	t.Helper()

	original := ServerShared
	serverClass := &ServerClass{
		class: class[uint64, *model.Server]{
			list: make(map[uint64]*model.Server),
		},
		uuidToID: make(map[string]uint64),
	}
	for _, server := range servers {
		serverClass.list[server.ID] = server
	}
	ServerShared = serverClass
	t.Cleanup(func() { ServerShared = original })
}

func replaceUserInfoMapForSecurityTest(t *testing.T, users map[uint64]model.UserInfo) {
	t.Helper()

	UserLock.Lock()
	original := UserInfoMap
	UserInfoMap = users
	UserLock.Unlock()

	t.Cleanup(func() {
		UserLock.Lock()
		UserInfoMap = original
		UserLock.Unlock()
	})
}

func TestCronTriggerSkipsServersOwnedByOtherUsers(t *testing.T) {
	firstStream := newCapturedTaskStream()
	secondStream := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "member-server"}, firstStream),
		withTaskStream(&model.Server{Common: model.Common{ID: 2, UserID: 200}, Name: "admin-server"}, secondStream),
	)

	cronTask := &model.Cron{
		Common:  model.Common{ID: 99, UserID: 100},
		Command: "id",
		Cover:   model.CronCoverAll,
		Servers: []uint64{},
	}

	CronTrigger(cronTask)()

	assertTaskCommand(t, firstStream, "id")
	assertNoTask(t, secondStream)
}

func TestSendTriggerTasksSkipsCronOwnedByAnotherUser(t *testing.T) {
	attackerStream := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 7, UserID: 200}, Name: "attacker-server"}, attackerStream),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		200: {Role: model.RoleMember},
	})

	adminCron := &model.Cron{
		Common:  model.Common{ID: 42, UserID: 1},
		Command: "admin-maintenance",
		Cover:   model.CronCoverAlertTrigger,
	}
	cronClass := &CronClass{
		class: class[uint64, *model.Cron]{
			list: map[uint64]*model.Cron{adminCron.ID: adminCron},
		},
	}

	cronClass.SendTriggerTasks([]uint64{adminCron.ID}, 7, 200)

	assertNoTask(t, attackerStream)
}

func assertTaskCommand(t *testing.T, stream *capturedTaskStream, expectedCommand string) {
	t.Helper()

	select {
	case task := <-stream.tasks:
		if task.GetType() != model.TaskTypeCommand {
			t.Fatalf("expected command task type, got %v", task.GetType())
		}
		if task.GetData() != expectedCommand {
			t.Fatalf("expected command %q, got %q", expectedCommand, task.GetData())
		}
	case <-time.After(time.Second):
		t.Fatalf("expected command %q to be sent", expectedCommand)
	}
}

func assertNoTask(t *testing.T, stream *capturedTaskStream) {
	t.Helper()

	select {
	case task := <-stream.tasks:
		t.Fatalf("expected no task to be sent, got command %q", task.GetData())
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCronTriggerSendsToMemberOwnedServer(t *testing.T) {
	memberStream := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "member-server"}, memberStream),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember},
	})

	cronTask := &model.Cron{
		Common:  model.Common{ID: 99, UserID: 100},
		Command: "id",
		Cover:   model.CronCoverAll,
	}

	CronTrigger(cronTask)()

	assertTaskCommand(t, memberStream, "id")
}

func TestCronTriggerRejectsOversizedCommand(t *testing.T) {
	memberStream := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "member-server"}, memberStream),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember},
	})

	// A row that reached memory without passing Cron.BeforeSave must not be
	// encoded once per covered Agent; the largest allowed command still is.
	cronTask := &model.Cron{
		Common:  model.Common{ID: 99, UserID: 100},
		Command: strings.Repeat("#", model.MaxCronCommandBytes+1),
		Cover:   model.CronCoverAll,
	}
	CronTrigger(cronTask)()
	select {
	case task := <-memberStream.tasks:
		t.Fatalf("expected no task to be sent, got a %d-byte command", len(task.GetData()))
	case <-time.After(50 * time.Millisecond):
	}

	cronTask.Command = cronTask.Command[1:]
	CronTrigger(cronTask)()
	assertTaskCommand(t, memberStream, cronTask.Command)
}

func TestCronTriggerAdminCronFansOutAcrossOwners(t *testing.T) {
	first := newCapturedTaskStream()
	second := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "member-server"}, first),
		withTaskStream(&model.Server{Common: model.Common{ID: 2, UserID: 200}, Name: "admin-server"}, second),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		100: {Role: model.RoleMember},
		200: {Role: model.RoleAdmin},
	})

	cronTask := &model.Cron{
		Common:  model.Common{ID: 99, UserID: 1},
		Command: "maintenance",
		Cover:   model.CronCoverAll,
	}

	CronTrigger(cronTask)()

	assertTaskCommand(t, first, "maintenance")
	assertTaskCommand(t, second, "maintenance")
}

func TestCronTriggerLegacyZeroOwnerFansOut(t *testing.T) {
	first := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "member-server"}, first),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember},
	})

	cronTask := &model.Cron{
		Common:  model.Common{ID: 99, UserID: 0},
		Command: "legacy",
		Cover:   model.CronCoverAll,
	}

	CronTrigger(cronTask)()

	assertTaskCommand(t, first, "legacy")
}

func TestCronTriggerSkipsServersWhenOwnerNotKnown(t *testing.T) {
	stream := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "member-server"}, stream),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember},
	})

	cronTask := &model.Cron{
		Common:  model.Common{ID: 99, UserID: 999},
		Command: "ghost",
		Cover:   model.CronCoverAll,
	}

	CronTrigger(cronTask)()

	assertNoTask(t, stream)
}

func TestSendTriggerTasksAllowsSelfOwnedCron(t *testing.T) {
	stream := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 7, UserID: 200}, Name: "member-server"}, stream),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	})

	memberCron := &model.Cron{
		Common:  model.Common{ID: 42, UserID: 200},
		Command: "member-task",
		Cover:   model.CronCoverAlertTrigger,
	}
	cronClass := &CronClass{
		class: class[uint64, *model.Cron]{
			list: map[uint64]*model.Cron{memberCron.ID: memberCron},
		},
	}

	cronClass.SendTriggerTasks([]uint64{memberCron.ID}, 7, 200)

	assertTaskCommand(t, stream, "member-task")
}

func TestSendTriggerTasksAllowsAdminCallerToTriggerAny(t *testing.T) {
	stream := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 9, UserID: 100}, Name: "any-server"}, stream),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		100: {Role: model.RoleMember},
	})

	memberCron := &model.Cron{
		Common:  model.Common{ID: 42, UserID: 100},
		Command: "member-task",
		Cover:   model.CronCoverAlertTrigger,
	}
	cronClass := &CronClass{
		class: class[uint64, *model.Cron]{
			list: map[uint64]*model.Cron{memberCron.ID: memberCron},
		},
	}

	cronClass.SendTriggerTasks([]uint64{memberCron.ID}, 9, 1)

	assertTaskCommand(t, stream, "member-task")
}

func TestSendTriggerTasksIgnoresUnknownTaskIDs(t *testing.T) {
	stream := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 7, UserID: 200}, Name: "member-server"}, stream),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	})

	cronClass := &CronClass{
		class: class[uint64, *model.Cron]{
			list: map[uint64]*model.Cron{},
		},
	}

	cronClass.SendTriggerTasks([]uint64{12345}, 7, 200)
	cronClass.SendTriggerTasks(nil, 7, 200)

	assertNoTask(t, stream)
}

func TestSendTriggerTasksMixedCronIDsOnlyFiresAllowed(t *testing.T) {
	stream := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 7, UserID: 200}, Name: "member-server"}, stream),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		200: {Role: model.RoleMember},
	})

	memberCron := &model.Cron{
		Common:  model.Common{ID: 7, UserID: 200},
		Command: "member-task",
		Cover:   model.CronCoverAlertTrigger,
	}
	adminCron := &model.Cron{
		Common:  model.Common{ID: 8, UserID: 1},
		Command: "admin-task",
		Cover:   model.CronCoverAlertTrigger,
	}
	cronClass := &CronClass{
		class: class[uint64, *model.Cron]{
			list: map[uint64]*model.Cron{memberCron.ID: memberCron, adminCron.ID: adminCron},
		},
	}

	cronClass.SendTriggerTasks([]uint64{memberCron.ID, adminCron.ID}, 7, 200)

	select {
	case task := <-stream.tasks:
		if task.GetData() != "member-task" {
			t.Fatalf("expected member-task, got %q", task.GetData())
		}
	case <-time.After(time.Second):
		t.Fatalf("expected member-task to be sent")
	}
	assertNoTask(t, stream)
}

func TestAlertTriggerCronResultAuthorizationConsumesOneDispatch(t *testing.T) {
	cronClass := &CronClass{}
	cronClass.reserveAlertTriggerCronResult(42, 7)
	cronClass.reserveAlertTriggerCronResult(42, 7)

	if !cronClass.consumeAlertTriggerCronResult(42, 7) {
		t.Fatal("expected first alert-trigger authorization to be consumed")
	}
	if !cronClass.consumeAlertTriggerCronResult(42, 7) {
		t.Fatal("expected second alert-trigger authorization to be consumed")
	}
	if cronClass.consumeAlertTriggerCronResult(42, 7) {
		t.Fatal("expected alert-trigger authorization to be consumed only once per dispatch")
	}
}

func TestAlertTriggerCronResultAuthorizationExpires(t *testing.T) {
	cronClass := &CronClass{
		pendingAlertTriggerTasks: map[uint64]map[uint64][]time.Time{
			42: {7: {time.Now().Add(-time.Second)}},
		},
	}

	if cronClass.consumeAlertTriggerCronResult(42, 7) {
		t.Fatal("expired alert-trigger authorization must not be accepted")
	}
	if len(cronClass.pendingAlertTriggerTasks) != 0 {
		t.Fatal("expired alert-trigger authorization must be pruned")
	}
}

func TestAlertTriggerCronResultAuthorizationRevokeRemovesLatestDispatch(t *testing.T) {
	existingAuthorizationExpiresAt := time.Now().Add(time.Hour)
	cronClass := &CronClass{
		pendingAlertTriggerTasks: map[uint64]map[uint64][]time.Time{
			42: {7: {existingAuthorizationExpiresAt}},
		},
	}
	cronClass.reserveAlertTriggerCronResult(42, 7)

	cronClass.revokeAlertTriggerCronResult(42, 7)

	authorizations := cronClass.pendingAlertTriggerTasks[42][7]
	if len(authorizations) != 1 {
		t.Fatalf("expected one previous alert-trigger authorization to remain, got %d", len(authorizations))
	}
	if !authorizations[0].Equal(existingAuthorizationExpiresAt) {
		t.Fatal("send failure rollback must remove the newest reserved authorization")
	}
}

func TestCronClassUpdatePrunesAlertTriggerCronResultAuthorization(t *testing.T) {
	cronClass := &CronClass{
		Cron: cron.New(cron.WithSeconds()),
		class: class[uint64, *model.Cron]{
			list: map[uint64]*model.Cron{42: {Common: model.Common{ID: 42}}},
		},
		pendingAlertTriggerTasks: map[uint64]map[uint64][]time.Time{
			42: {7: {time.Now().Add(time.Hour)}},
		},
	}

	cronClass.Update(&model.Cron{Common: model.Common{ID: 42}})

	if len(cronClass.pendingAlertTriggerTasks) != 0 {
		t.Fatal("cron update must prune old alert-trigger result authorizations")
	}
}

func TestCronClassDeletePrunesAlertTriggerCronResultAuthorization(t *testing.T) {
	cronClass := &CronClass{
		Cron: cron.New(cron.WithSeconds()),
		class: class[uint64, *model.Cron]{
			list: map[uint64]*model.Cron{42: {Common: model.Common{ID: 42}}},
		},
		pendingAlertTriggerTasks: map[uint64]map[uint64][]time.Time{
			42: {7: {time.Now().Add(time.Hour)}},
		},
	}

	cronClass.Delete([]uint64{42})

	if len(cronClass.pendingAlertTriggerTasks) != 0 {
		t.Fatal("cron delete must prune alert-trigger result authorizations")
	}
}

// CanReportCronResult is the cron-side dual of canReportServiceResult: it gates
// agent-reported TaskTypeCommand results to only the cron/server pairs the
// dashboard actually fanned the task out to. Without these inbound checks any
// authenticated agent could fabricate a TaskResult for an arbitrary cron ID and
// poison LastResult / fire success/failure notifications belonging to another
// tenant. The tests below pin each Cover branch end-to-end against the dispatch
// logic in CronTrigger so the two sides stay symmetric.

func TestCanReportCronResultRejectsNilCronOrReporter(t *testing.T) {
	cr := &model.Cron{Common: model.Common{ID: 7, UserID: 100}, Cover: model.CronCoverAll}
	reporter := &model.Server{Common: model.Common{ID: 1, UserID: 100}}

	if CanReportCronResult(nil, reporter) {
		t.Fatal("nil cron must be rejected — would dereference inside cover branches")
	}
	if CanReportCronResult(cr, nil) {
		t.Fatal("nil reporter must be rejected")
	}
}

func TestCanReportCronResultRejectsForeignReporter(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember},
		200: {Role: model.RoleMember},
	})

	cr := &model.Cron{
		Common: model.Common{ID: 7, UserID: 100},
		Cover:  model.CronCoverAll,
	}
	foreign := &model.Server{Common: model.Common{ID: 1, UserID: 200}}

	if CanReportCronResult(cr, foreign) {
		t.Fatal("foreign-user reporter must be rejected: CronTrigger never dispatched to it")
	}
}

func TestCanReportCronResultCronCoverAllRejectsReporterInDenyList(t *testing.T) {
	cr := &model.Cron{
		Common:  model.Common{ID: 7, UserID: 100},
		Cover:   model.CronCoverAll,
		Servers: []uint64{1},
	}
	reporter := &model.Server{Common: model.Common{ID: 1, UserID: 100}}

	if CanReportCronResult(cr, reporter) {
		t.Fatal("CronCoverAll treats Servers as deny-list; reporter in the list must be rejected")
	}
}

// dispatchCronForSecurityTest runs cr through CronTrigger with reporter online
// and waits for the task to reach its stream, leaving behind exactly the result
// authorization a real run leaves.
func dispatchCronForSecurityTest(t *testing.T, cr *model.Cron, reporter *model.Server) {
	t.Helper()

	stream := newCapturedTaskStream()
	replaceServerSharedForSecurityTest(t, withTaskStream(reporter, stream))
	originalCronShared := CronShared
	t.Cleanup(func() { CronShared = originalCronShared })
	CronShared = &CronClass{
		class: class[uint64, *model.Cron]{list: map[uint64]*model.Cron{cr.ID: cr}},
	}

	CronTrigger(cr)()

	select {
	case <-stream.tasks:
	case <-time.After(time.Second):
		t.Fatal("expected CronTrigger to dispatch the cron task to the reporter")
	}
}

func TestCanReportCronResultCronCoverAllAcceptsReporterNotInDenyList(t *testing.T) {
	cr := &model.Cron{
		Common:  model.Common{ID: 7, UserID: 100},
		Cover:   model.CronCoverAll,
		Servers: []uint64{99},
	}
	reporter := &model.Server{Common: model.Common{ID: 1, UserID: 100}}
	dispatchCronForSecurityTest(t, cr, reporter)

	if !CanReportCronResult(cr, reporter) {
		t.Fatal("CronCoverAll with reporter NOT in Servers must accept — CronTrigger dispatches to it")
	}
}

func TestCanReportCronResultCronCoverIgnoreAllAcceptsReporterInAllowList(t *testing.T) {
	cr := &model.Cron{
		Common:  model.Common{ID: 7, UserID: 100},
		Cover:   model.CronCoverIgnoreAll,
		Servers: []uint64{1},
	}
	reporter := &model.Server{Common: model.Common{ID: 1, UserID: 100}}
	dispatchCronForSecurityTest(t, cr, reporter)

	if !CanReportCronResult(cr, reporter) {
		t.Fatal("CronCoverIgnoreAll treats Servers as allow-list; reporter in the list must be accepted")
	}
}

// Being inside a scheduled cron's cover is not enough: an admin's cover-all
// cron covers every member's server, whose agent could otherwise report runs
// that never happened, at any time and any number of times.
func TestCanReportCronResultRequiresDispatchForScheduledCovers(t *testing.T) {
	for name, cr := range map[string]*model.Cron{
		"CronCoverAll":       {Common: model.Common{ID: 42, UserID: 1}, Command: "backup", Cover: model.CronCoverAll},
		"CronCoverIgnoreAll": {Common: model.Common{ID: 42, UserID: 1}, Command: "backup", Cover: model.CronCoverIgnoreAll, Servers: []uint64{7}},
	} {
		t.Run(name, func(t *testing.T) {
			replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
				1:   {Role: model.RoleAdmin},
				200: {Role: model.RoleMember},
			})
			reporter := &model.Server{Common: model.Common{ID: 7, UserID: 200}, Name: "member-server"}
			originalCronShared := CronShared
			t.Cleanup(func() { CronShared = originalCronShared })
			CronShared = &CronClass{}

			if CanReportCronResult(cr, reporter) {
				t.Fatal("result must be rejected while the cron was never dispatched to the reporter")
			}

			dispatchCronForSecurityTest(t, cr, reporter)

			if !CanReportCronResult(cr, reporter) {
				t.Fatal("result for a dispatched task must be accepted")
			}
			if CanReportCronResult(cr, reporter) {
				t.Fatal("one dispatched task must authorize only one result")
			}
		})
	}
}

// blockedTaskStream keeps the first Send in flight so later tasks pile up in
// the per-stream queue until QueueTaskGuarded refuses them.
type blockedTaskStream struct {
	capturedTaskStream
	release chan struct{}
}

func (s *blockedTaskStream) Send(task *pb.Task) error {
	<-s.release
	s.tasks <- task
	return nil
}

func TestCronTriggerRevokesScheduledAuthorizationWhenTaskIsNotQueued(t *testing.T) {
	const runs = 32
	blocked := &blockedTaskStream{
		capturedTaskStream: capturedTaskStream{tasks: make(chan *pb.Task, runs)},
		release:            make(chan struct{}),
	}
	reporter := withTaskStream(&model.Server{Common: model.Common{ID: 7, UserID: 100}, Name: "stalled-server"}, blocked)
	replaceServerSharedForSecurityTest(t, reporter)

	originalCronShared := CronShared
	t.Cleanup(func() { CronShared = originalCronShared })
	CronShared = &CronClass{}

	cr := &model.Cron{Common: model.Common{ID: 42, UserID: 100}, Cover: model.CronCoverAll}
	for range runs {
		CronTrigger(cr)()
	}

	accepted := 0
	for accepted < runs && CanReportCronResult(cr, reporter) {
		accepted++
	}
	if accepted == 0 {
		t.Fatal("queued tasks must leave a result authorization")
	}
	if accepted >= runs {
		t.Fatalf("tasks refused by the full queue must not leave a result authorization, got %d for %d runs", accepted, runs)
	}

	// Every authorization belongs to a task that is still delivered once the stream moves again.
	close(blocked.release)
	for range accepted {
		select {
		case <-blocked.tasks:
		case <-time.After(2 * time.Second):
			t.Fatal("expected every queued cron task to be sent")
		}
	}
}

func TestCronResultAuthorizationIsBoundedPerServer(t *testing.T) {
	cronClass := &CronClass{}
	for range cronResultAuthorizationLimit + 8 {
		cronClass.reserveAlertTriggerCronResult(42, 7)
	}

	if pending := len(cronClass.pendingAlertTriggerTasks[42][7]); pending != cronResultAuthorizationLimit {
		t.Fatalf("expected at most %d pending authorizations for one cron/server pair, got %d", cronResultAuthorizationLimit, pending)
	}
}

func TestCronResultAuthorizationExpiresBetweenSweeps(t *testing.T) {
	now := time.Now()
	cronClass := &CronClass{
		pendingAlertTriggerTasks: map[uint64]map[uint64][]time.Time{
			42: {7: {now.Add(-time.Second), now.Add(time.Hour)}},
		},
		pendingAlertTriggerSweptAt: now,
	}

	if !cronClass.consumeAlertTriggerCronResult(42, 7) {
		t.Fatal("expected the unexpired authorization to be consumed")
	}
	if cronClass.consumeAlertTriggerCronResult(42, 7) {
		t.Fatal("an expired authorization must not be accepted before the next sweep")
	}
	if len(cronClass.pendingAlertTriggerTasks) != 0 {
		t.Fatal("expired authorization must be dropped when its pair is consumed")
	}
}

func TestCanReportCronResultCronCoverIgnoreAllRejectsReporterOutsideAllowList(t *testing.T) {
	cr := &model.Cron{
		Common:  model.Common{ID: 7, UserID: 100},
		Cover:   model.CronCoverIgnoreAll,
		Servers: []uint64{99},
	}
	reporter := &model.Server{Common: model.Common{ID: 1, UserID: 100}}

	if CanReportCronResult(cr, reporter) {
		t.Fatal("CronCoverIgnoreAll with reporter NOT in Servers must reject — CronTrigger never dispatched to it")
	}
}

// failingTaskStream simulates a TaskStream whose Send always errors. CronTrigger
// uses this signal to revoke a reserved alert-trigger authorization, so the
// agent can't later attach to the cron via CanReportCronResult based on a
// dispatch that never actually reached the wire.
type failingTaskStream struct {
	capturedTaskStream
	sendErr error
}

func newFailingTaskStream(err error) *failingTaskStream {
	return &failingTaskStream{
		capturedTaskStream: capturedTaskStream{tasks: make(chan *pb.Task, 4)},
		sendErr:            err,
	}
}

func (s *failingTaskStream) Send(task *pb.Task) error {
	s.tasks <- task
	return s.sendErr
}

func TestCronTriggerRevokesAlertTriggerAuthorizationOnSendFailure(t *testing.T) {
	failing := newFailingTaskStream(context.Canceled)
	replaceServerSharedForSecurityTest(t,
		withTaskStream(&model.Server{Common: model.Common{ID: 7, UserID: 100}, Name: "broken-server"}, failing),
	)
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember},
	})

	originalCronShared := CronShared
	t.Cleanup(func() { CronShared = originalCronShared })
	CronShared = &CronClass{
		class:                    class[uint64, *model.Cron]{list: map[uint64]*model.Cron{}},
		pendingAlertTriggerTasks: map[uint64]map[uint64][]time.Time{},
	}

	cr := &model.Cron{
		Common: model.Common{ID: 42, UserID: 100},
		Cover:  model.CronCoverAlertTrigger,
	}

	CronTrigger(cr, 7)()

	// drain the dispatched task — Send error is what we care about, not the payload
	select {
	case <-failing.tasks:
	case <-time.After(time.Second):
		t.Fatal("expected CronTrigger to call Send before reacting to the error")
	}

	if CronShared.consumeAlertTriggerCronResult(42, 7) {
		t.Fatal("Send failure must revoke the reserved alert-trigger authorization; otherwise a foreign agent could later report a result for a dispatch that never reached the wire")
	}
	if len(CronShared.pendingAlertTriggerTasks) != 0 {
		t.Fatalf("expected pendingAlertTriggerTasks to be empty after revoke, got %d entries", len(CronShared.pendingAlertTriggerTasks))
	}
}

func TestClassCheckPermission(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		200: {Role: model.RoleMember},
	})
	sharedClass := &ServerClass{
		class: class[uint64, *model.Server]{
			list: map[uint64]*model.Server{
				1: {Common: model.Common{ID: 1, UserID: 200}},
				2: {Common: model.Common{ID: 2, UserID: 1}},
			},
		},
		uuidToID: map[string]uint64{},
	}

	memberCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	memberCtx.Set(model.CtxKeyAuthorizedUser, &model.User{
		Common: model.Common{ID: 200},
		Role:   model.RoleMember,
	})
	adminCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	adminCtx.Set(model.CtxKeyAuthorizedUser, &model.User{
		Common: model.Common{ID: 1},
		Role:   model.RoleAdmin,
	})

	if !sharedClass.CheckPermission(memberCtx, slices.Values([]uint64{1})) {
		t.Fatal("expected member to access own resource")
	}
	if sharedClass.CheckPermission(memberCtx, slices.Values([]uint64{2})) {
		t.Fatal("expected member to be denied foreign resource")
	}
	if !sharedClass.CheckPermission(memberCtx, slices.Values([]uint64{})) {
		t.Fatal("expected empty iterator to be allowed")
	}
	if !sharedClass.CheckPermission(memberCtx, slices.Values([]uint64{999})) {
		t.Fatal("expected unknown id to be ignored (vacuous true)")
	}
	if !sharedClass.CheckPermission(adminCtx, slices.Values([]uint64{1, 2})) {
		t.Fatal("expected admin to access any resource")
	}
}

func TestServiceMonitorResultSkipsReporterOutsideServiceCover(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "covered-server"},
		&model.Server{Common: model.Common{ID: 2, UserID: 100}, Name: "uncovered-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 100},
		Name:        "selected-only-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	})

	ss.Dispatch(serviceMonitorResult(2, 10, model.TaskTypeTCPPing, true))
	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 1)
	assertNoServiceHistory(t, 10, 2)
}

func TestServiceMonitorResultSkipsCoveredReporterOwnedByAnotherUser(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "owner-server"},
		&model.Server{Common: model.Common{ID: 2, UserID: 200}, Name: "foreign-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 100},
		Name:        "owner-only-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true, 2: true},
	})

	ss.Dispatch(serviceMonitorResult(2, 10, model.TaskTypeTCPPing, true))
	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 1)
	assertNoServiceHistory(t, 10, 2)
}

func TestServiceMonitorResultSkipsMismatchedTaskType(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "owner-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 100},
		Name:        "http-service",
		Type:        model.TaskTypeHTTPGet,
		Target:      "https://example.invalid",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	})

	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, false))
	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeHTTPGet, true))

	waitForTodayStats(t, ss, 10, 1, 0)
}

func TestServiceMonitorResultSkipsUnknownReporter(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "owner-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 100},
		Name:        "known-reporter-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	})

	ss.Dispatch(serviceMonitorResult(999, 10, model.TaskTypeTCPPing, true))
	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 1)
	assertNoServiceHistory(t, 10, 999)
}

func TestServiceMonitorResultAllowsCoveredReporterOwnedByServiceOwner(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 100}, Name: "owner-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 100},
		Name:        "owner-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	})

	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 1)
}

func TestServiceMonitorResultAllowsCoveredReporterForAdminOwnedService(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		200: {Role: model.RoleMember},
	})
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 2, UserID: 200}, Name: "member-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:      model.Common{ID: 10, UserID: 1},
		Name:        "admin-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{2: true},
	})

	ss.Dispatch(serviceMonitorResult(2, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 2)
}

func TestServiceMonitorResultFromMemberServerDoesNotDecideAdminOwnedService(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		2:   {Role: model.RoleAdmin},
		200: {Role: model.RoleMember},
	})
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "owner-server"},
		&model.Server{Common: model.Common{ID: 2, UserID: 200}, Name: "member-server"},
		&model.Server{Common: model.Common{ID: 3, UserID: 2}, Name: "other-admin-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:   model.Common{ID: 10, UserID: 1},
		Name:     "admin-service",
		Type:     model.TaskTypeTCPPing,
		Target:   "example.invalid:443",
		Duration: 3600,
		Cover:    model.ServiceCoverAll,
	})

	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))
	ss.Dispatch(serviceMonitorResult(2, 10, model.TaskTypeTCPPing, false))
	ss.Dispatch(serviceMonitorResult(3, 10, model.TaskTypeTCPPing, true))

	// The member server keeps its per-server history, but only the servers of
	// the service owner and of the other admin decide the shared state.
	waitForServiceHistory(t, 10, 2)
	waitForTodayStats(t, ss, 10, 2, 0)

	ss.serviceResponseDataStoreLock.RLock()
	defer ss.serviceResponseDataStoreLock.RUnlock()
	if current := ss.serviceResponseDataStore[10]; current.Up != 2 || current.Down != 0 {
		t.Fatalf("member server moved the admin service window: up=%d down=%d", current.Up, current.Down)
	}
	if lastStatus := ss.serviceCurrentStatusData[10].lastStatus; lastStatus != StatusGood {
		t.Fatalf("expected admin service status %d, got %d", StatusGood, lastStatus)
	}
}

func newServiceMonitorSecurityHarness(t *testing.T, servers ...*model.Server) *ServiceSentinel {
	t.Helper()

	originalDB := DB
	originalConf := Conf
	originalCache := Cache
	originalCronShared := CronShared
	originalServerShared := ServerShared
	originalServiceSentinelShared := ServiceSentinelShared
	originalNotificationShared := NotificationShared
	originalTSDBShared := TSDBShared
	originalLoc := Loc
	var sqlDBClose func() error

	t.Cleanup(func() {
		DB = originalDB
		Conf = originalConf
		Cache = originalCache
		CronShared = originalCronShared
		ServerShared = originalServerShared
		ServiceSentinelShared = originalServiceSentinelShared
		NotificationShared = originalNotificationShared
		TSDBShared = originalTSDBShared
		Loc = originalLoc
		if sqlDBClose != nil {
			_ = sqlDBClose()
		}
	})

	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDBClose = sqlDB.Close
	DB = db
	if err := DB.AutoMigrate(
		model.Server{},
		model.Service{},
		model.ServiceHistory{},
		model.Notification{},
		model.NotificationGroup{},
		model.NotificationGroupNotification{},
	); err != nil {
		t.Fatal(err)
	}

	Conf = &ConfigClass{Config: &model.Config{AvgPingCount: 1}}
	Cache = cache.New(time.Minute, time.Minute)
	CronShared = &CronClass{
		Cron:  cron.New(cron.WithSeconds()),
		class: class[uint64, *model.Cron]{list: map[uint64]*model.Cron{}},
	}
	NotificationShared = &NotificationClass{
		class:         class[uint64, *model.Notification]{list: map[uint64]*model.Notification{}},
		groupToIDList: map[uint64]map[uint64]*model.Notification{},
		idToGroupList: map[uint64]map[uint64]struct{}{},
		groupList:     map[uint64]string{},
	}
	TSDBShared = nil
	Loc = time.UTC

	serverClass := &ServerClass{
		class: class[uint64, *model.Server]{
			list: make(map[uint64]*model.Server),
		},
		uuidToID: make(map[string]uint64),
	}
	for _, server := range servers {
		serverClass.list[server.ID] = server
	}
	ServerShared = serverClass

	bus := make(chan *model.Service, 1)
	ss, err := NewServiceSentinel(bus)
	if err != nil {
		t.Fatal(err)
	}
	ServiceSentinelShared = ss
	// LIFO Cleanup ordering: this Close() runs BEFORE the earlier t.Cleanup that
	// restores Conf/Cache/CronShared/NotificationShared/TSDBShared, so the
	// worker has fully exited before we swap those globals out. Skipping this
	// step causes `go test -race` to flag the write-vs-read between the
	// teardown and the still-running worker.
	t.Cleanup(func() { ss.Close() })
	return ss
}

func addServiceMonitorSecurityService(t *testing.T, ss *ServiceSentinel, service *model.Service) {
	t.Helper()

	if err := DB.Create(service).Error; err != nil {
		t.Fatal(err)
	}
	if err := ss.Update(service); err != nil {
		t.Fatal(err)
	}
}

func serviceMonitorResult(reporter, serviceID uint64, taskType uint8, successful bool) ReportData {
	return ReportData{
		Reporter: reporter,
		Data: &pb.TaskResult{
			Id:         serviceID,
			Type:       uint64(taskType),
			Delay:      12,
			Data:       "service monitor result",
			Successful: successful,
		},
	}
}

func waitForServiceHistory(t *testing.T, serviceID, serverID uint64) {
	t.Helper()

	deadline := time.After(time.Second)
	for {
		var count int64
		if err := DB.Model(&model.ServiceHistory{}).
			Where("service_id = ? AND server_id = ?", serviceID, serverID).
			Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("expected service history for service %d from server %d", serviceID, serverID)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func assertNoServiceHistory(t *testing.T, serviceID, serverID uint64) {
	t.Helper()

	var count int64
	if err := DB.Model(&model.ServiceHistory{}).
		Where("service_id = ? AND server_id = ?", serviceID, serverID).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected no service history for service %d from server %d, got %d", serviceID, serverID, count)
	}
}

func waitForTodayStats(t *testing.T, ss *ServiceSentinel, serviceID uint64, wantUp, wantDown uint64) {
	t.Helper()

	deadline := time.After(time.Second)
	for {
		ss.serviceResponseDataStoreLock.RLock()
		stats := ss.serviceStatusToday[serviceID]
		var up, down uint64
		if stats != nil {
			up = stats.Up
			down = stats.Down
		}
		ss.serviceResponseDataStoreLock.RUnlock()

		if up == wantUp && down == wantDown {
			return
		}
		if down > wantDown {
			t.Fatalf("expected service %d down count %d, got %d", serviceID, wantDown, down)
		}

		select {
		case <-deadline:
			t.Fatalf("expected service %d stats up=%d down=%d", serviceID, wantUp, wantDown)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
