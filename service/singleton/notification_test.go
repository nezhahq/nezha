package singleton

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"

	"github.com/nezhahq/nezha/model"
)

func TestNotificationFailureDoesNotMuteRetryOrResendSuccessfulRecipient(t *testing.T) {
	oldCache := Cache
	Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() { Cache = oldCache })
	first := &model.Notification{Common: model.Common{ID: 301}, Name: "first"}
	second := &model.Notification{Common: model.Common{ID: 302}, Name: "second"}
	nc := newNotificationClassWithItems(first, second)
	nc.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 303}, Name: "retry-test"}, []uint64{first.ID, second.ID})
	counts := map[uint64]int{}
	nc.sendForTest = func(n *model.Notification, _ string, _ *model.Server) error {
		counts[n.ID]++
		if n.ID == second.ID && counts[n.ID] == 1 {
			return errors.New("temporary failure")
		}
		return nil
	}
	label := "alert-retry-test"
	nc.UnMuteNotification(303, label)
	if nc.SendNotification(303, "incident", label) {
		t.Fatal("partial delivery must report failure")
	}
	if !nc.SendNotification(303, "incident", label) {
		t.Fatal("failed recipient should be retried successfully")
	}
	if counts[first.ID] != 1 || counts[second.ID] != 2 {
		t.Fatalf("delivery attempts = %v, want first=1 second=2", counts)
	}
	nc.UnMuteNotification(303, label)
}

func TestNotificationConcurrentSameRecipientSendsOnce(t *testing.T) {
	oldCache := Cache
	Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() { Cache = oldCache })
	n := &model.Notification{Common: model.Common{ID: 401}, Name: "hook"}
	nc := newNotificationClassWithItems(n)
	nc.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 402}, Name: "concurrent-test"}, []uint64{n.ID})
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	nc.sendForTest = func(*model.Notification, string, *model.Server) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan bool, 2)
	go func() { done <- nc.SendNotification(402, "incident", "same-event") }()
	<-entered
	go func() { done <- nc.SendNotification(402, "incident", "same-event") }()
	close(release)
	if !<-done || !<-done {
		t.Fatal("concurrent delivery returned failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("same event delivered %d times, want 1", calls.Load())
	}
}

func TestNotificationAsyncCoalescesBlockedServiceReports(t *testing.T) {
	oldCache := Cache
	Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() { Cache = oldCache })
	n := &model.Notification{Common: model.Common{ID: 501}, Name: "hook"}
	nc := newNotificationClassWithItems(n)
	nc.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 502}, Name: "service-test"}, []uint64{n.ID})
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	nc.sendForTest = func(*model.Notification, string, *model.Server) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	nc.SendNotificationAsync(502, "down", "service-down")
	<-entered
	for i := 0; i < 1000; i++ {
		nc.SendNotificationAsync(502, "down", "service-down")
	}
	nc.deliveryMu.Lock()
	inFlight := len(nc.asyncInFlight)
	nc.deliveryMu.Unlock()
	if inFlight != 1 || calls.Load() != 1 {
		t.Fatalf("in-flight sends=%d attempts=%d, want one each", inFlight, calls.Load())
	}
	close(release)
	deadline := time.After(time.Second)
	for {
		nc.deliveryMu.Lock()
		remaining := len(nc.asyncInFlight)
		nc.deliveryMu.Unlock()
		if remaining == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("async notification did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestNotificationFailureSummaryOmitsCredentialURL(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://example.test/bot-secret-token/send?key=secret-query", Err: errors.New("timeout")}
	if summary := notificationFailureSummary(err); strings.Contains(summary, "secret") || strings.Contains(summary, "example.test") {
		t.Fatalf("credential-bearing URL leaked: %s", summary)
	}
}

func TestNotificationAsyncBacksOffFastFailures(t *testing.T) {
	oldCache := Cache
	Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() { Cache = oldCache })
	n := &model.Notification{Common: model.Common{ID: 511}, Name: "hook"}
	nc := newNotificationClassWithItems(n)
	nc.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 512}, Name: "backoff-test"}, []uint64{n.ID})
	var calls atomic.Int32
	nc.sendForTest = func(*model.Notification, string, *model.Server) error {
		calls.Add(1)
		return errors.New("fast failure")
	}
	key := "512:service-down"
	nc.SendNotificationAsync(512, "down", "service-down")
	deadline := time.After(time.Second)
	for {
		nc.deliveryMu.Lock()
		busy := len(nc.asyncInFlight) != 0
		after := nc.asyncRetryAfter[key]
		nc.deliveryMu.Unlock()
		if !busy && !after.IsZero() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first failed send did not record backoff")
		case <-time.After(time.Millisecond):
		}
	}
	for i := 0; i < 1000; i++ {
		nc.SendNotificationAsync(512, "down", "service-down")
	}
	if calls.Load() != 1 {
		t.Fatalf("fast failures caused %d attempts before retry-after", calls.Load())
	}
}

func TestUnMuteDuringInflightSendCannotRemuteRecovery(t *testing.T) {
	oldCache := Cache
	Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() { Cache = oldCache })
	n := &model.Notification{Common: model.Common{ID: 521}, Name: "hook"}
	nc := newNotificationClassWithItems(n)
	nc.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 522}, Name: "generation-test"}, []uint64{n.ID})
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	nc.sendForTest = func(*model.Notification, string, *model.Server) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan bool, 1)
	go func() { done <- nc.SendNotification(522, "first failure", "static-label") }()
	<-entered
	nc.UnMuteNotification(522, "static-label")
	close(release)
	if !<-done {
		t.Fatal("first send failed unexpectedly")
	}
	if !nc.SendNotification(522, "next failure", "static-label") || calls.Load() != 2 {
		t.Fatalf("old in-flight send re-muted next failure: calls=%d", calls.Load())
	}
}

func TestAsyncBackoffMemoryIsBounded(t *testing.T) {
	nc := &NotificationClass{
		asyncRetryAfter: make(map[string]time.Time),
		asyncRetryDelay: make(map[string]time.Duration),
	}
	for i := 0; i < maxAsyncBackoffKeys+100; i++ {
		key := fmt.Sprintf("old-event-%d", i)
		nc.asyncRetryAfter[key] = time.Now().Add(time.Minute)
		nc.asyncRetryDelay[key] = time.Minute
	}
	nc.pruneAsyncBackoffLocked()
	if len(nc.asyncRetryAfter) > maxAsyncBackoffKeys || len(nc.asyncRetryDelay) != len(nc.asyncRetryAfter) {
		t.Fatalf("unbounded backoff maps: after=%d delay=%d", len(nc.asyncRetryAfter), len(nc.asyncRetryDelay))
	}
}

func TestServiceStateNotificationsKeepIncidentRecoveryOrder(t *testing.T) {
	oldCache := Cache
	Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() { Cache = oldCache })
	n := &model.Notification{Common: model.Common{ID: 531}, Name: "hook"}
	nc := newNotificationClassWithItems(n)
	nc.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 532}, Name: "ordered-test"}, []uint64{n.ID})
	entered := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan string, 3)
	nc.sendForTest = func(_ *model.Notification, desc string, _ *model.Server) error {
		if desc == "down" {
			close(entered)
			<-release
		}
		delivered <- desc
		return nil
	}
	nc.SendServiceState(533, 532, "down", "state-down-1")
	<-entered
	nc.SendServiceState(533, 532, "good", "state-good-2")
	select {
	case message := <-delivered:
		t.Fatalf("service transition overtook blocked incident: %s", message)
	default:
	}
	close(release)
	for _, want := range []string{"down", "good"} {
		select {
		case got := <-delivered:
			if got != want {
				t.Fatalf("delivery order got %s, want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing %s delivery", want)
		}
	}
	deadline := time.After(time.Second)
	for {
		nc.deliveryMu.Lock()
		remaining := len(nc.serviceDelivery)
		nc.deliveryMu.Unlock()
		if remaining == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("ordered delivery worker did not exit")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestServiceStateRetriesFailedIncidentBeforePendingRecovery(t *testing.T) {
	oldCache := Cache
	Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() { Cache = oldCache })
	n := &model.Notification{Common: model.Common{ID: 535}, Name: "hook"}
	nc := newNotificationClassWithItems(n)
	nc.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 536}, Name: "retry-order-test"}, []uint64{n.ID})
	t.Cleanup(func() { nc.CancelServiceState(537) })

	attempts := make(chan string, 3)
	releaseFirst := make(chan struct{})
	var downAttempts atomic.Int32
	nc.sendForTest = func(_ *model.Notification, desc string, _ *model.Server) error {
		attempts <- desc
		if desc == "down" && downAttempts.Add(1) == 1 {
			<-releaseFirst
			return errors.New("temporary incident failure")
		}
		return nil
	}

	nc.SendServiceState(537, 536, "down", "state-down-1")
	nc.deliveryMu.Lock()
	state := nc.serviceDelivery[537]
	nc.deliveryMu.Unlock()
	select {
	case got := <-attempts:
		if got != "down" {
			t.Fatalf("first delivery = %q, want down", got)
		}
	case <-time.After(time.Second):
		t.Fatal("incident delivery did not start")
	}
	nc.SendServiceState(537, 536, "good", "state-good-2")
	close(releaseFirst)

	for _, want := range []string{"down", "good"} {
		select {
		case got := <-attempts:
			if got != want {
				t.Fatalf("delivery after failed incident = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing %s delivery", want)
		}
	}
	select {
	case <-state.done:
	case <-time.After(time.Second):
		t.Fatal("ordered delivery worker did not finish")
	}
}

func TestServiceStateCancelStopsOldConfiguration(t *testing.T) {
	oldCache := Cache
	Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() { Cache = oldCache })
	n := &model.Notification{Common: model.Common{ID: 541}, Name: "hook"}
	nc := newNotificationClassWithItems(n)
	nc.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 542}, Name: "old-group"}, []uint64{n.ID})
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	nc.sendForTest = func(*model.Notification, string, *model.Server) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return errors.New("old webhook failed")
	}
	nc.SendServiceState(543, 542, "down", "old-event")
	<-entered
	nc.deliveryMu.Lock()
	state := nc.serviceDelivery[543]
	nc.deliveryMu.Unlock()
	nc.CancelServiceState(543)
	nc.deliveryMu.Lock()
	remaining := len(nc.serviceDelivery)
	nc.deliveryMu.Unlock()
	if remaining != 0 {
		t.Fatal("old service delivery worker remained registered after cancellation")
	}
	close(release)
	select {
	case <-state.done:
	case <-time.After(time.Second):
		t.Fatal("canceled old delivery worker did not exit")
	}
}

func TestServiceUpdateCancelsOldRecipientRetry(t *testing.T) {
	ss := newServiceMonitorSecurityHarness(t)
	service := &model.Service{
		Common:              model.Common{ID: 551, UserID: 1},
		Name:                "generic-probe",
		Type:                model.TaskTypeTCPPing,
		Target:              "example.invalid:443",
		Duration:            3600,
		Notify:              true,
		NotificationGroupID: 552,
		Cover:               model.ServiceCoverIgnoreAll,
	}
	addServiceMonitorSecurityService(t, ss, service)
	n := &model.Notification{Common: model.Common{ID: 553}, Name: "old-hook"}
	NotificationShared.InsertForTest(n)
	NotificationShared.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 552}, Name: "old-group"}, []uint64{n.ID})
	entered := make(chan struct{})
	release := make(chan struct{})
	NotificationShared.sendForTest = func(*model.Notification, string, *model.Server) error {
		close(entered)
		<-release
		return errors.New("failed old recipient")
	}
	NotificationShared.SendServiceState(service.ID, 552, "old down", "old-state")
	<-entered
	NotificationShared.deliveryMu.Lock()
	state := NotificationShared.serviceDelivery[service.ID]
	NotificationShared.deliveryMu.Unlock()
	updated := *service
	updated.Notify = false
	updated.NotificationGroupID = 554
	if err := ss.Update(&updated); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-state.done:
	case <-time.After(time.Second):
		t.Fatal("old recipient retry did not stop after service update")
	}
	NotificationShared.deliveryMu.Lock()
	remaining := len(NotificationShared.serviceDelivery)
	NotificationShared.deliveryMu.Unlock()
	if remaining != 0 {
		t.Fatal("old recipient retained after service update")
	}
}

func newNotificationClassWithItems(items ...*model.Notification) *NotificationClass {
	nc := NewEmptyNotificationClassForTest()
	for _, item := range items {
		nc.InsertForTest(item)
	}
	return nc
}

func TestNotificationClassDeleteGroupCleansReverseIndex(t *testing.T) {
	n := &model.Notification{Common: model.Common{ID: 1, UserID: 2}, Name: "hook"}
	nc := newNotificationClassWithItems(n)
	group := &model.NotificationGroup{Common: model.Common{ID: 10, UserID: 2}, Name: "group"}
	nc.UpdateGroup(group, []uint64{n.ID})

	nc.DeleteGroup([]uint64{group.ID})

	if _, ok := nc.groupToIDList[group.ID]; ok {
		t.Fatalf("forward index still contains deleted group %d", group.ID)
	}
	if groups, ok := nc.idToGroupList[n.ID]; ok {
		if _, stale := groups[group.ID]; stale {
			t.Fatalf("reverse index for notification %d still contains deleted group %d", n.ID, group.ID)
		}
	}
}

func TestNotificationClassUpdateGroupCleansRemovedMembership(t *testing.T) {
	first := &model.Notification{Common: model.Common{ID: 1, UserID: 2}, Name: "first"}
	second := &model.Notification{Common: model.Common{ID: 2, UserID: 2}, Name: "second"}
	nc := newNotificationClassWithItems(first, second)
	group := &model.NotificationGroup{Common: model.Common{ID: 10, UserID: 2}, Name: "group"}
	nc.UpdateGroup(group, []uint64{first.ID, second.ID})

	nc.UpdateGroup(group, []uint64{second.ID})

	if groups, ok := nc.idToGroupList[first.ID]; ok {
		if _, stale := groups[group.ID]; stale {
			t.Fatalf("reverse index for notification %d still contains group %d", first.ID, group.ID)
		}
	}
	if _, ok := nc.idToGroupList[second.ID][group.ID]; !ok {
		t.Fatalf("reverse index lost retained membership of notification %d in group %d", second.ID, group.ID)
	}
}

func TestNotificationClassUpdateRepairsOrphanedReverseIndex(t *testing.T) {
	n := &model.Notification{Common: model.Common{ID: 1, UserID: 2}, Name: "hook"}
	nc := newNotificationClassWithItems(n)
	nc.idToGroupList[n.ID] = map[uint64]struct{}{99: {}}

	n.Name = "updated"
	nc.Update(n)

	if groups, ok := nc.idToGroupList[n.ID]; ok && len(groups) != 0 {
		t.Fatalf("orphaned reverse memberships were not removed: %v", groups)
	}
}

func TestNotificationClassGroupMutationsDoNotDeadlock(t *testing.T) {
	nc := NewEmptyNotificationClassForTest()
	nc.listMu.RLock()
	readLockHeld := true
	defer func() {
		if readLockHeld {
			nc.listMu.RUnlock()
		}
	}()

	deleteDone := make(chan struct{})
	go func() {
		nc.DeleteGroup([]uint64{10})
		close(deleteDone)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for nc.listMu.TryRLock() {
		nc.listMu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("DeleteGroup did not queue for listMu")
		}
		time.Sleep(time.Millisecond)
	}

	updateDone := make(chan struct{})
	go func() {
		nc.UpdateGroup(&model.NotificationGroup{
			Common: model.Common{ID: 10, UserID: 2},
			Name:   "group",
		}, nil)
		close(updateDone)
	}()

	deadline = time.Now().Add(2 * time.Second)
	for nc.groupMu.TryLock() {
		nc.groupMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("neither group mutation acquired groupMu")
		}
		time.Sleep(time.Millisecond)
	}

	nc.listMu.RUnlock()
	readLockHeld = false

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for deleteDone != nil || updateDone != nil {
		select {
		case <-deleteDone:
			deleteDone = nil
		case <-updateDone:
			updateDone = nil
		case <-timer.C:
			t.Fatal("concurrent UpdateGroup and DeleteGroup deadlocked")
		}
	}
}
