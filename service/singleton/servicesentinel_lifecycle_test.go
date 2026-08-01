package singleton

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
)

// Regression markers for Finding 1 and Finding 2 of GHSA-jx78-55p5-rwv5
// (incomplete fix of GHSA-qjpp-gffx-2wm9).
const (
	concurrentServerDeleteSuccessMarker = "ghsa-jx78-55p5-rwv5-finding1-no-crash"
	deleteUnknownIDSuccessMarker        = "ghsa-jx78-55p5-rwv5-finding2-no-zombie"
)

const serviceSentinelLifecycleSuccessMarker = "service-sentinel-stale-report-lifecycle-success"

func TestServiceSentinelReporterDeleteWaitsForSynchronousReportProcessing(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "reporter"},
	)
	service := &model.Service{
		Common:      model.Common{ID: 10, UserID: 1},
		Name:        "lifecycle-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "lifecycle.example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	}
	addServiceMonitorSecurityService(t, ss, service)

	reportValidated := make(chan struct{})
	releaseReport := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseReport) }) }
	ss.serviceReportValidatedHook = func(serviceID uint64) {
		if serviceID == service.ID {
			close(reportValidated)
			<-releaseReport
		}
	}
	t.Cleanup(func() {
		release()
		ss.Close()
	})

	ss.Dispatch(serviceMonitorResult(1, service.ID, model.TaskTypeTCPPing, true))
	select {
	case <-reportValidated:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	deleteDone := make(chan struct{})
	go func() {
		ServerShared.Delete([]uint64{1})
		close(deleteDone)
	}()
	select {
	case <-deleteDone:
		t.Fatal("server deletion returned before the accepted report completed")
	case <-time.After(25 * time.Millisecond):
	}

	release()
	select {
	case <-deleteDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	var historyCount int64
	if err := DB.Model(&model.ServiceHistory{}).
		Where("service_id = ? AND server_id = ?", service.ID, 1).
		Count(&historyCount).Error; err != nil {
		t.Fatal(err)
	}
	if historyCount != 1 {
		t.Fatalf("expected report side effects before deletion returned, got %d history rows", historyCount)
	}
	if _, ok := ServerShared.Get(1); ok {
		t.Fatal("expected reporter to be deleted after the report completed")
	}
}

func TestServiceSentinelWorkerRejectsReportAfterReporterDeletion(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "reporter"},
	)
	service := &model.Service{
		Common:      model.Common{ID: 10, UserID: 1},
		Name:        "deleted-reporter-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "deleted-reporter.example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	}
	addServiceMonitorSecurityService(t, ss, service)

	ServerShared.Delete([]uint64{1})
	ss.Dispatch(serviceMonitorResult(1, service.ID, model.TaskTypeTCPPing, true))
	ss.Close()

	var historyCount int64
	if err := DB.Model(&model.ServiceHistory{}).
		Where("service_id = ? AND server_id = ?", service.ID, 1).
		Count(&historyCount).Error; err != nil {
		t.Fatal(err)
	}
	if historyCount != 0 {
		t.Fatalf("expected no history after reporter deletion, got %d rows", historyCount)
	}
	ss.serviceResponseDataStoreLock.RLock()
	_, pingCached := ss.serviceResponsePing[service.ID]
	_, responseCached := ss.serviceResponseDataStore[service.ID]
	stats := ss.serviceStatusToday[service.ID]
	ss.serviceResponseDataStoreLock.RUnlock()
	if pingCached {
		t.Fatal("expected no ping cache side effect after reporter deletion")
	}
	if responseCached {
		t.Fatal("expected no response cache side effect after reporter deletion")
	}
	if stats == nil || stats.Up != 0 || stats.Down != 0 {
		t.Fatalf("expected no stats side effect after reporter deletion, got %+v", stats)
	}
}

func TestServiceSentinelWorkerRecoversPerReportPanic(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "reporter"},
	)
	panicService := &model.Service{
		Common:      model.Common{ID: 10, UserID: 1},
		Name:        "panic-service",
		Type:        model.TaskTypeHTTPGet,
		Target:      "https://panic.example.invalid",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	}
	validService := &model.Service{
		Common:      model.Common{ID: 20, UserID: 1},
		Name:        "valid-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "valid.example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	}
	addServiceMonitorSecurityService(t, ss, panicService)
	addServiceMonitorSecurityService(t, ss, validService)
	ss.serviceReportBeforeTLSSideEffectsHook = func(serviceID uint64) {
		if serviceID == panicService.ID {
			panic("test service report panic")
		}
	}

	ss.Dispatch(serviceMonitorResult(1, panicService.ID, model.TaskTypeHTTPGet, true))
	ss.Dispatch(serviceMonitorResult(1, validService.ID, model.TaskTypeTCPPing, true))
	waitForServiceHistory(t, validService.ID, 1)
	ss.Close()
	if !ss.serviceResponseDataStoreLock.TryLock() {
		t.Fatal("panic leaked the service response lock")
	}
	ss.serviceResponseDataStoreLock.Unlock()

	deleteDone := make(chan struct{})
	go func() {
		ServerShared.Delete([]uint64{1})
		close(deleteDone)
	}()
	select {
	case <-deleteDone:
	case <-ctx.Done():
		t.Fatal("panic leaked a lifecycle lock: " + ctx.Err().Error())
	}
}

func TestServiceSentinelWorkerIgnoresStaleReportAfterDeletion(t *testing.T) {
	if os.Getenv("NEZHA_SERVICE_SENTINEL_LIFECYCLE_CHILD") == "1" {
		testServiceSentinelWorkerIgnoresStaleReportAfterDeletionChild(t)
		return
	}

	// Given
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServiceSentinelWorkerIgnoresStaleReportAfterDeletion$")
	child.Env = append(os.Environ(), "NEZHA_SERVICE_SENTINEL_LIFECYCLE_CHILD=1")

	// When
	output, err := child.CombinedOutput()

	// Then
	if ctx.Err() != nil {
		t.Fatalf("service sentinel lifecycle child timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("service sentinel lifecycle child failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), serviceSentinelLifecycleSuccessMarker) {
		t.Fatalf("service sentinel lifecycle child did not report success:\n%s", output)
	}
}

func testServiceSentinelWorkerIgnoresStaleReportAfterDeletionChild(t *testing.T) {
	// Given
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "reporter"},
	)
	for _, service := range []*model.Service{
		{
			Common:      model.Common{ID: 10, UserID: 1},
			Name:        "stale-service",
			Type:        model.TaskTypeTCPPing,
			Target:      "stale.example.invalid:443",
			Duration:    3600,
			Cover:       model.ServiceCoverIgnoreAll,
			SkipServers: map[uint64]bool{1: true},
		},
		{
			Common:      model.Common{ID: 20, UserID: 1},
			Name:        "valid-service",
			Type:        model.TaskTypeTCPPing,
			Target:      "valid.example.invalid:443",
			Duration:    3600,
			Cover:       model.ServiceCoverIgnoreAll,
			SkipServers: map[uint64]bool{1: true},
		},
	} {
		addServiceMonitorSecurityService(t, ss, service)
	}
	acceptedStaleReport := make(chan struct{})
	releaseWorker := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorkerHook := func() {
		releaseOnce.Do(func() { close(releaseWorker) })
	}
	ss.serviceReportValidatedHook = func(serviceID uint64) {
		if serviceID == 10 {
			close(acceptedStaleReport)
			<-releaseWorker
		}
	}
	t.Cleanup(func() {
		releaseWorkerHook()
		ss.Close()
	})

	// When
	ss.Dispatch(serviceMonitorResult(1, 10, model.TaskTypeTCPPing, true))
	select {
	case <-acceptedStaleReport:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	ss.Delete([]uint64{10})
	releaseWorkerHook()
	ss.Dispatch(serviceMonitorResult(1, 20, model.TaskTypeTCPPing, true))
	ss.Close()

	// Then
	var staleHistoryCount int64
	if err := DB.Model(&model.ServiceHistory{}).
		Where("service_id = ? AND server_id = ?", 10, 1).
		Count(&staleHistoryCount).Error; err != nil {
		t.Fatal(err)
	}
	if staleHistoryCount != 0 {
		t.Fatalf("expected stale service to write zero per-reporter history rows, got %d", staleHistoryCount)
	}
	var validHistoryCount int64
	if err := DB.Model(&model.ServiceHistory{}).
		Where("service_id = ? AND server_id = ?", 20, 1).
		Count(&validHistoryCount).Error; err != nil {
		t.Fatal(err)
	}
	if validHistoryCount != 1 {
		t.Fatalf("expected exactly one valid service history row, got %d", validHistoryCount)
	}
	ss.serviceResponseDataStoreLock.RLock()
	_, stalePingCached := ss.serviceResponsePing[10]
	validStats := ss.serviceStatusToday[20]
	ss.serviceResponseDataStoreLock.RUnlock()
	if stalePingCached {
		t.Fatal("expected stale service ping cache to be deleted")
	}
	if validStats == nil || validStats.Up != 1 || validStats.Down != 0 {
		t.Fatalf("expected valid service stats up=1 down=0, got %+v", validStats)
	}
	if _, err := fmt.Fprintln(os.Stdout, serviceSentinelLifecycleSuccessMarker); err != nil {
		t.Fatal(err)
	}
}

func TestServiceSentinelWorkerRevalidatesReportAfterUpdate(t *testing.T) {
	// Given
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "reporter"},
	)
	service := &model.Service{
		Common:      model.Common{ID: 10, UserID: 1},
		Name:        "updatable-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "updatable.example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	}
	addServiceMonitorSecurityService(t, ss, service)
	ss.serviceResponseDataStoreLock.Lock()
	ss.serviceStatusToday[service.ID] = &_TodayStatsOfService{Up: 7, Down: 3, Delay: 12.5}
	ss.serviceResponseDataStoreLock.Unlock()
	acceptedReport := make(chan struct{})
	releaseWorker := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorkerHook := func() {
		releaseOnce.Do(func() { close(releaseWorker) })
	}
	ss.serviceReportValidatedHook = func(serviceID uint64) {
		if serviceID == service.ID {
			close(acceptedReport)
			<-releaseWorker
		}
	}
	t.Cleanup(func() {
		releaseWorkerHook()
		ss.Close()
	})

	// When
	ss.Dispatch(serviceMonitorResult(1, service.ID, model.TaskTypeTCPPing, true))
	select {
	case <-acceptedReport:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	updatedService := *service
	updatedService.Name = "updated-service"
	updatedService.SkipServers = map[uint64]bool{}
	if err := ss.Update(&updatedService); err != nil {
		t.Fatal(err)
	}
	releaseWorkerHook()
	ss.Close()

	// Then
	var historyCount int64
	if err := DB.Model(&model.ServiceHistory{}).
		Where("service_id = ? AND server_id = ?", service.ID, 1).
		Count(&historyCount).Error; err != nil {
		t.Fatal(err)
	}
	if historyCount != 0 {
		t.Fatalf("expected updated service to write zero per-reporter history rows, got %d", historyCount)
	}
	ss.serviceResponseDataStoreLock.RLock()
	_, pingCached := ss.serviceResponsePing[service.ID]
	stats := ss.serviceStatusToday[service.ID]
	ss.serviceResponseDataStoreLock.RUnlock()
	if pingCached {
		t.Fatal("expected updated service report to leave no ping cache entry")
	}
	if stats == nil || stats.Up != 7 || stats.Down != 3 || stats.Delay != 12.5 {
		t.Fatalf("expected existing service stats to remain unchanged, got %+v", stats)
	}
	currentService, ok := ss.Get(service.ID)
	if !ok || currentService.Name != updatedService.Name || currentService.SkipServers[1] {
		t.Fatalf("expected updated service configuration, got %+v", currentService)
	}
}

func TestServiceSentinelLoadStatsFollowsLifecycleLockOrder(t *testing.T) {
	// Given
	ss := &ServiceSentinel{
		serviceStatusToday:       make(map[uint64]*_TodayStatsOfService),
		serviceResponseDataStore: make(map[uint64]serviceResponseData),
		services:                 make(map[uint64]*model.Service),
		monthlyStatus:            make(map[uint64]*serviceResponseItem),
	}
	ss.loadStatsResponseLockedHook = func() {
		if ss.serviceResponseDataStoreLock.TryLock() {
			ss.serviceResponseDataStoreLock.Unlock()
			t.Fatal("LoadStats invoked the hook before acquiring the response read lock")
		}
		if !ss.monthlyStatusLock.TryLock() {
			t.Fatal("LoadStats acquired monthlyStatusLock before the response lock hook")
		}
		ss.monthlyStatusLock.Unlock()
		if !ss.servicesLock.TryLock() {
			t.Fatal("LoadStats acquired servicesLock before the response lock hook")
		}
		ss.servicesLock.Unlock()
	}

	// When / Then
	ss.LoadStats()
}

func TestServiceSentinelWorkerHoldsResponseLockDuringTLSSideEffects(t *testing.T) {
	// Given
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "reporter"},
	)
	service := &model.Service{
		Common:      model.Common{ID: 10, UserID: 1},
		Name:        "tls-service",
		Type:        model.TaskTypeHTTPGet,
		Target:      "https://tls.example.invalid",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	}
	addServiceMonitorSecurityService(t, ss, service)
	tlsSideEffectsReady := make(chan struct{})
	releaseTLSSideEffects := make(chan struct{})
	var releaseOnce sync.Once
	releaseTLSSideEffectsHook := func() {
		releaseOnce.Do(func() { close(releaseTLSSideEffects) })
	}
	ss.serviceReportBeforeTLSSideEffectsHook = func(serviceID uint64) {
		if serviceID == service.ID {
			close(tlsSideEffectsReady)
			<-releaseTLSSideEffects
		}
	}
	t.Cleanup(func() {
		releaseTLSSideEffectsHook()
		ss.Close()
	})
	report := serviceMonitorResult(1, service.ID, model.TaskTypeHTTPGet, true)
	report.Data.Data = "issuer|2030-01-02 15:04:05 +0000 UTC"

	// When
	ss.Dispatch(report)
	select {
	case <-tlsSideEffectsReady:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	responseLockAcquired := ss.serviceResponseDataStoreLock.TryLock()
	if responseLockAcquired {
		ss.serviceResponseDataStoreLock.Unlock()
		t.Fatal("worker released the response lock before TLS side effects")
	}
	releaseTLSSideEffectsHook()
	ss.Close()

	// Then
	ss.serviceResponseDataStoreLock.RLock()
	cachedCertificate := ss.tlsCertCache[service.ID]
	ss.serviceResponseDataStoreLock.RUnlock()
	if cachedCertificate != report.Data.Data {
		t.Fatalf("expected TLS cache %q, got %q", report.Data.Data, cachedCertificate)
	}
}

// TestServiceSentinelWorkerSurvivesConcurrentReporterServerDelete is a
// regression test for GHSA-jx78-55p5-rwv5 Finding 1 (incomplete fix of
// GHSA-qjpp-gffx-2wm9).
//
// The vulnerability: after the 2026-07-21 fix, the worker re-validates the
// service under serviceResponseDataStoreLock, but then takes a fresh snapshot
// m := ServerShared.GetList() with no guard. A concurrent batch-delete of the
// reporter's own server removes it between the pre-lock validation and the
// GetList call, so m[r.Reporter] is nil.  delayCheck and notifyCheck then
// dereference m[r.Reporter].Name unconditionally — SIGSEGV.
//
// The subprocess-isolation pattern is used because the pre-fix code path
// panicked (nil pointer dereference in an unrecovered goroutine), which would
// crash the whole test binary rather than simply failing a single test.
func TestServiceSentinelWorkerSurvivesConcurrentReporterServerDelete(t *testing.T) {
	if os.Getenv("NEZHA_SENTINEL_CONCURRENT_DELETE_CHILD") == "1" {
		testServiceSentinelWorkerSurvivesConcurrentReporterServerDeleteChild(t)
		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestServiceSentinelWorkerSurvivesConcurrentReporterServerDelete$",
		"-test.v",
	)
	child.Env = append(os.Environ(), "NEZHA_SENTINEL_CONCURRENT_DELETE_CHILD=1")

	output, err := child.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("child process timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("child process crashed (likely nil deref in delayCheck/notifyCheck): %v\n%s", err, output)
	}
	if !strings.Contains(string(output), concurrentServerDeleteSuccessMarker) {
		t.Fatalf("child did not print success marker:\n%s", output)
	}
}

func testServiceSentinelWorkerSurvivesConcurrentReporterServerDeleteChild(t *testing.T) {
	// Given: a reporter server and a service with latency-alerting enabled so
	// that delayCheck (the vulnerable sink at line 785) is exercised on every
	// dispatch.  MaxLatency=1 ensures delay=12 always exceeds the threshold and
	// the notification branch (not just the mute-clear branch) is taken.
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "reporter"},
	)
	service := &model.Service{
		Common:        model.Common{ID: 10, UserID: 1},
		Name:          "latency-service",
		Type:          model.TaskTypeTCPPing,
		Target:        "example.invalid:443",
		Duration:      3600,
		Cover:         model.ServiceCoverIgnoreAll,
		SkipServers:   map[uint64]bool{1: true},
		LatencyNotify: true,
		MaxLatency:    1,
	}
	addServiceMonitorSecurityService(t, ss, service)

	reportProcessing := make(chan struct{})
	releaseWorker := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorkerFn := func() { releaseOnce.Do(func() { close(releaseWorker) }) }

	// serviceReportValidatedHook runs while the report holds the lifecycle read
	// lock. Deletion must therefore run in another goroutine and wait until this
	// hook releases; attempting Delete here would try to upgrade the RWMutex.
	ss.serviceReportValidatedHook = func(serviceID uint64) {
		if serviceID == service.ID {
			close(reportProcessing)
			<-releaseWorker
		}
	}
	t.Cleanup(func() {
		releaseWorkerFn()
		ss.Close()
	})

	// When
	ss.Dispatch(serviceMonitorResult(1, service.ID, model.TaskTypeTCPPing, true))
	select {
	case <-reportProcessing:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	deleteDone := make(chan struct{})
	go func() {
		ServerShared.Delete([]uint64{1})
		close(deleteDone)
	}()
	releaseWorkerFn()
	select {
	case <-deleteDone:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	ss.Close()

	// Then: no crash; the worker handled the nil reporter gracefully.
	if _, err := fmt.Fprintln(os.Stdout, concurrentServerDeleteSuccessMarker); err != nil {
		t.Fatal(err)
	}
}

// TestServiceSentinelDeleteWithUnknownIDDoesNotLeaveZombies is a regression
// test for GHSA-jx78-55p5-rwv5 Finding 2 (low severity).
//
// The vulnerability: ServiceSentinel.Delete iterates the caller-supplied id
// slice and does CronShared.Remove(ss.services[id].CronJobID) without checking
// whether id is present in ss.services.  CheckPermission returns vacuously
// true for unknown ids, so the controller layer cannot block this path.
// ss.services[unknownID] returns nil, and .CronJobID panics.  Because the
// panic aborts the loop, every id ordered AFTER the bogus one is never removed
// from the in-memory registry even though its database row was already deleted,
// producing zombie services that keep dispatching cron probes.
func TestServiceSentinelDeleteWithUnknownIDDoesNotLeaveZombies(t *testing.T) {
	// Given: one legitimate service (ID 10) registered in the sentinel.
	ss := newServiceMonitorSecurityHarness(t,
		&model.Server{Common: model.Common{ID: 1, UserID: 1}, Name: "reporter"},
	)
	service := &model.Service{
		Common:      model.Common{ID: 10, UserID: 1},
		Name:        "real-service",
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Duration:    3600,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	}
	addServiceMonitorSecurityService(t, ss, service)

	// When: Delete is called with a bogus ID first, then the real service ID.
	// Before the fix this panicked on ss.services[99999].CronJobID and left
	// service 10 as a zombie.
	ss.Delete([]uint64{99999, service.ID})

	// Then: the real service must be fully removed from every in-memory map.
	ss.serviceResponseDataStoreLock.RLock()
	_, todayPresent := ss.serviceStatusToday[service.ID]
	_, pingPresent := ss.serviceResponsePing[service.ID]
	ss.serviceResponseDataStoreLock.RUnlock()

	ss.servicesLock.RLock()
	_, servicePresent := ss.services[service.ID]
	ss.servicesLock.RUnlock()

	ss.monthlyStatusLock.Lock()
	_, monthlyPresent := ss.monthlyStatus[service.ID]
	ss.monthlyStatusLock.Unlock()

	if todayPresent {
		t.Error("zombie: serviceStatusToday still contains the deleted service")
	}
	if pingPresent {
		t.Error("zombie: serviceResponsePing still contains the deleted service")
	}
	if servicePresent {
		t.Error("zombie: services map still contains the deleted service")
	}
	if monthlyPresent {
		t.Error("zombie: monthlyStatus still contains the deleted service")
	}

	if _, err := fmt.Fprintln(os.Stdout, deleteUnknownIDSuccessMarker); err != nil {
		t.Fatal(err)
	}
}
