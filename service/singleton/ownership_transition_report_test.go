package singleton

import (
	"testing"

	"github.com/nezhahq/nezha/model"
)

// replaceServerTransferSharedForSecurityTest publishes the given transfers as
// Pending without a DB. Call it before newServiceMonitorSecurityHarness so the
// sentinel worker has exited before the global is restored.
func replaceServerTransferSharedForSecurityTest(t *testing.T, pending ...*model.ServerTransfer) *ServerTransferClass {
	t.Helper()

	original := ServerTransferShared
	t.Cleanup(func() {
		ServerTransferShared = original
	})
	transfer := &ServerTransferClass{pending: make(map[uint64]*model.ServerTransfer)}
	for _, p := range pending {
		transfer.pending[p.ServerID] = p
	}
	ServerTransferShared = transfer
	return transfer
}

// While a transfer is Pending, Server.UserID already names the destination user
// but the Agent stream may still authenticate with the previous owner's secret,
// and ServerTaskAdmission dispatches none of the destination user's tasks to the
// server. The inbound gate must refuse what the outbound side would not send.
func TestCanReportCronResultRejectsReporterInOwnershipTransition(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		100: {Role: model.RoleMember},
		200: {Role: model.RoleMember},
	})
	replaceServerTransferSharedForSecurityTest(t, &model.ServerTransfer{ServerID: 7, FromUserID: 100, ToUserID: 200})

	moved := &model.Server{Common: model.Common{ID: 7, UserID: 200}}
	settled := &model.Server{Common: model.Common{ID: 8, UserID: 200}}
	cr := &model.Cron{Common: model.Common{ID: 42, UserID: 200}, Cover: model.CronCoverAll}
	adminCron := &model.Cron{Common: model.Common{ID: 43, UserID: 1}, Cover: model.CronCoverAll}

	// Results are only accepted for dispatched tasks, so reserve one per check
	// to isolate the ownership-transition rule.
	originalCronShared := CronShared
	t.Cleanup(func() { CronShared = originalCronShared })
	CronShared = &CronClass{}
	CronShared.reserveAlertTriggerCronResult(cr.ID, moved.ID)
	CronShared.reserveAlertTriggerCronResult(cr.ID, settled.ID)
	CronShared.reserveAlertTriggerCronResult(adminCron.ID, moved.ID)

	if CanReportCronResult(cr, moved) {
		t.Fatal("member cron result must be rejected from a server whose transfer is pending")
	}
	if !CanReportCronResult(cr, settled) {
		t.Fatal("member cron result must still be accepted from the owner's settled server")
	}
	if !CanReportCronResult(adminCron, moved) {
		t.Fatal("admin cron is still dispatched to a server in transition; its result must be accepted")
	}
}

// Service-monitor dual of TestCanReportCronResultRejectsReporterInOwnershipTransition:
// probes are not dispatched to a server in transition, so a result arriving on
// its stream (still the previous owner's) must not count for the new owner.
func TestServiceMonitorResultSkipsReporterInOwnershipTransition(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember},
		200: {Role: model.RoleMember},
	})
	transfer := replaceServerTransferSharedForSecurityTest(t, &model.ServerTransfer{ServerID: 2, FromUserID: 100, ToUserID: 200})
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 200}, Name: "owner-server"},
		&model.Server{Common: model.Common{ID: 2, UserID: 200}, Name: "moved-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:   model.Common{ID: 10, UserID: 200},
		Name:     "new-owner-service",
		Type:     model.TaskTypeTCPPing,
		Target:   "example.invalid:443",
		Duration: 3600,
		Cover:    model.ServiceCoverAll,
	})

	ss.Dispatch(serviceMonitorResult(2, 10, model.TaskTypeTCPPing, true))
	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 1)
	assertNoServiceHistory(t, 10, 2)

	// Once the Agent has rotated and the transfer left Pending, the server is
	// an ordinary server of the new owner again.
	transfer.mu.Lock()
	delete(transfer.pending, 2)
	transfer.mu.Unlock()
	ss.Dispatch(serviceMonitorResult(2, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 2)
}

func TestServiceMonitorResultAllowsTransitioningReporterForAdminOwnedService(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin},
		100: {Role: model.RoleMember},
		200: {Role: model.RoleMember},
	})
	replaceServerTransferSharedForSecurityTest(t, &model.ServerTransfer{ServerID: 2, FromUserID: 100, ToUserID: 200})
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 2, UserID: 200}, Name: "moved-server"},
	)
	addServiceMonitorSecurityService(t, ss, &model.Service{
		Common:   model.Common{ID: 10, UserID: 1},
		Name:     "admin-service",
		Type:     model.TaskTypeTCPPing,
		Target:   "example.invalid:443",
		Duration: 3600,
		Cover:    model.ServiceCoverAll,
	})

	ss.Dispatch(serviceMonitorResult(2, 10, model.TaskTypeTCPPing, true))

	waitForServiceHistory(t, 10, 2)
}
