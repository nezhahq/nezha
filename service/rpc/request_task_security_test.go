package rpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

type requestTaskSecurityStream struct {
	ctx      context.Context
	results  []*pb.TaskResult
	onRecv   func()
	onResult func()
	onSend   func(*pb.Task)
	sendErr  error
}

func (s *requestTaskSecurityStream) Send(task *pb.Task) error {
	if s.onSend != nil {
		s.onSend(task)
	}
	return s.sendErr
}

func (s *requestTaskSecurityStream) Recv() (*pb.TaskResult, error) {
	if len(s.results) == 0 {
		if s.onRecv != nil {
			s.onRecv()
		}
		return nil, context.Canceled
	}
	result := s.results[0]
	s.results = s.results[1:]
	if s.onResult != nil {
		onResult := s.onResult
		s.onResult = nil
		onResult()
	}
	return result, nil
}

func (s *requestTaskSecurityStream) SetHeader(metadata.MD) error  { return nil }
func (s *requestTaskSecurityStream) SendHeader(metadata.MD) error { return nil }
func (s *requestTaskSecurityStream) SetTrailer(metadata.MD)       {}
func (s *requestTaskSecurityStream) Context() context.Context     { return s.ctx }
func (s *requestTaskSecurityStream) SendMsg(any) error            { return nil }
func (s *requestTaskSecurityStream) RecvMsg(any) error            { return context.Canceled }

func TestRequestTaskSkipsCronResultOwnedByAnotherUser(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "11111111-1111-1111-1111-111111111111")
	victimCron := requestTaskSecurityCron(42, 100, model.CronCoverAll, nil)
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{victimCron}, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember},
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID, cronTaskResult(victimCron.ID, true))

	if cronLastResult(t, victimCron.ID) {
		t.Fatal("foreign cron result must not update victim cron status")
	}
}

func TestRequestTaskSkipsCronResultOutsideReporterCover(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "22222222-2222-2222-2222-222222222222")
	coveredServerID := uint64(8)
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverIgnoreAll, []uint64{coveredServerID})
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID, cronTaskResult(cronTask.ID, true))

	if cronLastResult(t, cronTask.ID) {
		t.Fatal("cron result from a server outside cron cover must not update cron status")
	}
}

func TestRequestTaskSkipsCronCoverAllExcludedReporter(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "88888888-8888-8888-8888-888888888888")
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverAll, []uint64{reporter.ID})
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID, cronTaskResult(cronTask.ID, true))

	if cronLastResult(t, cronTask.ID) {
		t.Fatal("cron result from a server excluded by CronCoverAll must not update cron status")
	}
}

func TestRequestTaskAllowsCronCoverAllReporter(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "99999999-9999-9999-9999-999999999999")
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverAll, []uint64{8})
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID, cronTaskResult(cronTask.ID, true))

	if !cronLastResult(t, cronTask.ID) {
		t.Fatal("CronCoverAll reporter not in the exclusion list must update cron status")
	}
}

func TestRequestTaskAllowsCronResultForCoveredOwnerServer(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "33333333-3333-3333-3333-333333333333")
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverIgnoreAll, []uint64{reporter.ID})
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID, cronTaskResult(cronTask.ID, true))

	if !cronLastResult(t, cronTask.ID) {
		t.Fatal("covered owner cron result must update cron status")
	}
}

func TestRequestTaskAllowsCronResultForCoveredAdminOwnedCron(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "44444444-4444-4444-4444-444444444444")
	cronTask := requestTaskSecurityCron(42, 1, model.CronCoverIgnoreAll, []uint64{reporter.ID})
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID, cronTaskResult(cronTask.ID, true))

	if !cronLastResult(t, cronTask.ID) {
		t.Fatal("covered admin-owned cron result must update cron status")
	}
}

func TestRequestTaskSkipsAlertTriggerCronResultFromUntriggeredReporter(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "55555555-5555-5555-5555-555555555555")
	triggerServer := requestTaskSecurityServer(8, 200, "66666666-6666-6666-6666-666666666666")
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverAlertTrigger, nil)
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter, triggerServer}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200, "trigger-secret": 200})
	connectRequestTaskSecurityTaskStream(t, triggerServer.ID)
	singleton.CronTrigger(cronTask, triggerServer.ID)()

	runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID, cronTaskResult(cronTask.ID, true))

	if cronLastResult(t, cronTask.ID) {
		t.Fatal("alert-trigger cron result from a non-triggered server must not update cron status")
	}
}

func TestRequestTaskAllowsAlertTriggerCronResultForTriggeredReporter(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "77777777-7777-7777-7777-777777777777")
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverAlertTrigger, nil)
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})
	connectRequestTaskSecurityTaskStream(t, reporter.ID)
	singleton.CronTrigger(cronTask, reporter.ID)()

	runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID, cronTaskResult(cronTask.ID, true))

	if !cronLastResult(t, cronTask.ID) {
		t.Fatal("alert-trigger cron result from the triggered server must update cron status")
	}
}

func TestRequestTaskAllowsAlertTriggerCronResultReportedDuringSend(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverAlertTrigger, nil)
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})
	connectRequestTaskSecurityTaskStreamWithSendHook(t, reporter.ID, nil, func(task *pb.Task) {
		if task.GetId() != cronTask.ID {
			t.Fatalf("expected alert-trigger task %d, got %d", cronTask.ID, task.GetId())
		}
		runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID, cronTaskResult(cronTask.ID, true))
	})

	singleton.CronTrigger(cronTask, reporter.ID)()

	if !cronLastResult(t, cronTask.ID) {
		t.Fatal("alert-trigger cron result reported during Send must update cron status")
	}
}

func TestRequestTaskSkipsAlertTriggerCronResultAfterSendFailure(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	cronTask := requestTaskSecurityCron(42, 200, model.CronCoverAlertTrigger, nil)
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, []*model.Cron{cronTask}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})
	connectRequestTaskSecurityTaskStreamWithSendHook(t, reporter.ID, errors.New("send failed"), nil)
	singleton.CronTrigger(cronTask, reporter.ID)()

	runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID, cronTaskResult(cronTask.ID, true))

	if cronLastResult(t, cronTask.ID) {
		t.Fatal("alert-trigger cron result after failed dispatch must not update cron status")
	}
}

func TestRequestTaskClearsTaskStreamOnRecvError(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "cccccccc-cccc-cccc-cccc-cccccccccccc")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, nil, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	stream := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	err := NewNezhaHandler().RequestTask(stream)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected RequestTask to finish after Recv error, got %v", err)
	}

	server, ok := singleton.ServerShared.Get(reporter.ID)
	if !ok {
		t.Fatalf("server %d not found", reporter.ID)
	}
	if got := server.GetTaskStream(); got != nil {
		t.Fatalf("dead RequestTask stream must be cleared, got %T", got)
	}
}

func TestRequestTaskKeepsNewerTaskStreamOnOldRecvError(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "dddddddd-dddd-dddd-dddd-dddddddddddd")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, nil, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	server, ok := singleton.ServerShared.Get(reporter.ID)
	if !ok {
		t.Fatalf("server %d not found", reporter.ID)
	}
	newer := &requestTaskSecurityStream{ctx: context.Background()}
	old := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	old.onRecv = func() {
		server.SetTaskStream(newer)
	}

	err := NewNezhaHandler().RequestTask(old)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected RequestTask to finish after Recv error, got %v", err)
	}
	if got := server.GetTaskStream(); got != newer {
		t.Fatalf("old stream cleanup must keep newer stream, got %T", got)
	}
}

func setupRequestTaskSecurityFixture(t *testing.T, servers []*model.Server, crons []*model.Cron, users map[uint64]model.UserInfo, agentSecrets map[string]uint64) {
	t.Helper()

	originalDB := singleton.DB
	originalConf := singleton.Conf
	originalLoc := singleton.Loc
	originalLocalizer := singleton.Localizer
	originalNotification := singleton.NotificationShared
	originalServerShared := singleton.ServerShared
	originalServiceSentinel := singleton.ServiceSentinelShared
	originalCronShared := singleton.CronShared
	originalUserInfoMap := singleton.UserInfoMap
	originalAgentSecretToUserID := singleton.AgentSecretToUserId

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)

	singleton.DB = db
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{}}
	singleton.Loc = time.UTC
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	singleton.NotificationShared = singleton.NewEmptyNotificationClassForTest()
	if err := singleton.DB.AutoMigrate(model.Server{}, model.Cron{}); err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		if err := singleton.DB.Create(server).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, cronTask := range crons {
		if err := singleton.DB.Create(cronTask).Error; err != nil {
			t.Fatal(err)
		}
	}

	singleton.UserLock.Lock()
	singleton.UserInfoMap = users
	singleton.AgentSecretToUserId = agentSecrets
	singleton.UserLock.Unlock()
	singleton.ServerShared = singleton.NewServerClass()
	singleton.CronShared = singleton.NewCronClass()

	t.Cleanup(func() {
		singleton.CronShared.Close()
		_ = sqlDB.Close()
		singleton.DB = originalDB
		singleton.Conf = originalConf
		singleton.Loc = originalLoc
		singleton.Localizer = originalLocalizer
		singleton.NotificationShared = originalNotification
		singleton.ServiceSentinelShared = originalServiceSentinel
		singleton.ServerShared = originalServerShared
		singleton.CronShared = originalCronShared
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfoMap
		singleton.AgentSecretToUserId = originalAgentSecretToUserID
		singleton.UserLock.Unlock()
	})
}

func requestTaskSecurityServer(id, userID uint64, uuid string) *model.Server {
	return &model.Server{
		Common: model.Common{ID: id, UserID: userID},
		UUID:   uuid,
		Name:   "request-task-security-server",
	}
}

func requestTaskSecurityCron(id, userID uint64, cover uint8, servers []uint64) *model.Cron {
	return &model.Cron{
		Common:    model.Common{ID: id, UserID: userID},
		Name:      "request-task-security-cron",
		Command:   "id",
		Scheduler: "@every 1h",
		Cover:     cover,
		Servers:   servers,
	}
}

func cronTaskResult(cronID uint64, successful bool) *pb.TaskResult {
	return &pb.TaskResult{
		Id:         cronID,
		Type:       model.TaskTypeCommand,
		Delay:      1,
		Data:       "cron result",
		Successful: successful,
	}
}

func connectRequestTaskSecurityTaskStream(t *testing.T, serverID uint64) {
	t.Helper()

	connectRequestTaskSecurityTaskStreamWithSendHook(t, serverID, nil, nil)
}

func connectRequestTaskSecurityTaskStreamWithSendHook(t *testing.T, serverID uint64, sendErr error, onSend func(*pb.Task)) {
	t.Helper()

	server, ok := singleton.ServerShared.Get(serverID)
	if !ok {
		t.Fatalf("server %d not found", serverID)
	}
	server.SetTaskStream(&requestTaskSecurityStream{ctx: context.Background(), sendErr: sendErr, onSend: onSend})
}

func runRequestTaskSecurityResult(t *testing.T, secret string, uuid string, result *pb.TaskResult) {
	t.Helper()

	stream := requestTaskSecurityAuthedStream(secret, uuid)
	stream.results = []*pb.TaskResult{result}
	err := NewNezhaHandler().RequestTask(stream)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected RequestTask to finish after test result, got %v", err)
	}
}

func requestTaskSecurityAuthedStream(secret string, uuid string) *requestTaskSecurityStream {
	return &requestTaskSecurityStream{
		ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			"client_secret", secret,
			"client_uuid", uuid,
		)),
	}
}

func cronLastResult(t *testing.T, cronID uint64) bool {
	t.Helper()

	var cronTask model.Cron
	if err := singleton.DB.First(&cronTask, cronID).Error; err != nil {
		t.Fatal(err)
	}
	return cronTask.LastResult
}
