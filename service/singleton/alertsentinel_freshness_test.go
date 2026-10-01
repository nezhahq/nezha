package singleton

import (
	"context"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"google.golang.org/grpc/metadata"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
)

type alertTestStateStream struct{}

func (alertTestStateStream) Send(*pb.Receipt) error       { return nil }
func (alertTestStateStream) Recv() (*pb.State, error)     { return nil, context.Canceled }
func (alertTestStateStream) SetHeader(metadata.MD) error  { return nil }
func (alertTestStateStream) SendHeader(metadata.MD) error { return nil }
func (alertTestStateStream) SetTrailer(metadata.MD)       {}
func (alertTestStateStream) Context() context.Context     { return context.Background() }
func (alertTestStateStream) SendMsg(any) error            { return nil }
func (alertTestStateStream) RecvMsg(any) error            { return nil }

func TestAlertSentinelCountsOnlyFreshDistinctMetricReports(t *testing.T) {
	oldStore, oldPrev, oldLast, oldCycle := alertsStore, alertsPrevState, alertsLastMetricAt, AlertsCycleTransferStatsStore
	alertsStore = map[uint64]map[uint64][]model.TimedAlertPoint{7: {1: nil}}
	alertsPrevState = map[uint64]map[uint64]uint8{7: {}}
	alertsLastMetricAt = map[uint64]map[uint64]time.Time{7: {}}
	AlertsCycleTransferStatsStore = map[uint64]*model.CycleTransferStats{}
	t.Cleanup(func() {
		alertsStore, alertsPrevState, alertsLastMetricAt, AlertsCycleTransferStatsStore = oldStore, oldPrev, oldLast, oldCycle
	})

	alert := &model.AlertRule{
		Common: model.Common{ID: 7},
		Rules:  []*model.Rule{{Type: "cpu", Max: 80, Duration: 100, Cover: model.RuleCoverAll}},
	}
	server := &model.Server{Common: model.Common{ID: 1}}
	model.InitServer(server)
	lease := server.AttachStateStream(alertTestStateStream{})
	if !lease.UpdateState(&model.HostState{CPU: 99}, time.Now()) {
		t.Fatal("state update rejected")
	}
	checkStatusForServer(alert, server)
	if got := len(alertsStore[7][1]); got != 1 {
		t.Fatalf("first report produced %d samples, want 1", got)
	}
	for i := 0; i < 100; i++ {
		checkStatusForServer(alert, server)
	}
	if got := len(alertsStore[7][1]); got != 1 {
		t.Fatalf("same report was counted %d times, want 1", got)
	}
	if !lease.Clear() {
		t.Fatal("stream clear rejected")
	}
	checkStatusForServer(alert, server)
	if got := len(alertsStore[7][1]); got != 0 {
		t.Fatalf("disconnect retained %d high samples, want 0", got)
	}
	alertsPrevState[7][1] = _RuleCheckFail // incident was active before disconnect
	newLease := server.AttachStateStream(alertTestStateStream{})
	if !newLease.UpdateState(&model.HostState{CPU: 99}, time.Now()) {
		t.Fatal("reconnect state update rejected")
	}
	checkStatusForServer(alert, server)
	if got := len(alertsStore[7][1]); got != 1 || alertsStore[7][1][0].Values[0] {
		t.Fatalf("reconnect window = %v, want one high sample", alertsStore[7][1])
	}
	if alertsPrevState[7][1] != _RuleCheckFail {
		t.Fatal("incomplete warm-up falsely resolved active incident")
	}
}

func TestAlertNewIncidentNotMutedByEarlierInflightIncident(t *testing.T) {
	oldCache, oldNotifications := Cache, NotificationShared
	Cache = cache.New(time.Minute, time.Minute)
	n := &model.Notification{Common: model.Common{ID: 601}, Name: "hook"}
	nc := newNotificationClassWithItems(n)
	nc.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 603}, Name: "alert-test"}, []uint64{n.ID})
	NotificationShared = nc
	t.Cleanup(func() {
		cancelAlertDeliveries(602)
		NotificationShared, Cache = oldNotifications, oldCache
	})
	started := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan string, 3)
	nc.sendForTest = func(_ *model.Notification, desc string, _ *model.Server) error {
		if desc == "first incident" {
			close(started)
			<-release
		}
		delivered <- desc
		return nil
	}
	alert := &model.AlertRule{
		Common:              model.Common{ID: 602},
		NotificationGroupID: 603,
		TriggerMode:         model.ModeOnetimeTrigger,
	}
	server := &model.Server{Common: model.Common{ID: 604}}
	label := NotificationMuteLabel.ServerIncident(server.ID, alert.ID)
	startAlertDelivery(alert, server, "first incident", label)
	<-started
	startAlertDelivery(alert, server, "recovered", NotificationMuteLabel.ServerIncidentResolved(server.ID, alert.ID))
	startAlertDelivery(alert, server, "second incident", label)
	close(release)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case message := <-delivered:
			seen[message] = true
		case <-timer.C:
			t.Fatalf("new incident was suppressed after old send completed: %v", seen)
		}
	}
	if !seen["first incident"] || !seen["second incident"] || seen["recovered"] {
		t.Fatalf("unexpected transition deliveries: %v", seen)
	}
	alertDeliveryMu.Lock()
	done := alertDeliveries[alert.ID][server.ID].done
	alertDeliveryMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("new incident delivery did not finish")
	}
}

func TestAlertDeliveryRetriesFailureAndStopsAfterSuccess(t *testing.T) {
	cancel := make(chan struct{})
	done := make(chan struct{})
	attempts := 0
	go func() {
		runAlertDelivery(cancel, func() bool {
			attempts++
			return attempts == 2
		}, false, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(cancel)
		t.Fatal("delivery did not retry and finish")
	}
	if attempts != 2 {
		t.Fatalf("got %d attempts, want 2", attempts)
	}
}

func TestAlertDeliveryCancelsAfterRecovery(t *testing.T) {
	cancel := make(chan struct{})
	done := make(chan struct{})
	started := make(chan struct{})
	go func() {
		runAlertDelivery(cancel, func() bool {
			close(started)
			return true
		}, true, time.Millisecond)
		close(done)
	}()
	<-started
	close(cancel)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovered incident delivery was not cancelled")
	}
}
