package singleton

import (
	"sync"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
)

func cycleTransferRuleFixture(t *testing.T, stats *model.CycleTransferStats) (*model.AlertRule, *model.Server) {
	t.Helper()
	t.Cleanup(setupTestDB(t))
	if err := DB.AutoMigrate(model.Transfer{}); err != nil {
		t.Fatalf("migrate transfer table: %v", err)
	}
	previousStore, previousPrev, previousLast, previousCycle := alertsStore, alertsPrevState, alertsLastMetricSeq, AlertsCycleTransferStatsStore
	t.Cleanup(func() {
		alertsStore, alertsPrevState, alertsLastMetricSeq, AlertsCycleTransferStatsStore = previousStore, previousPrev, previousLast, previousCycle
	})
	alertsStore = map[uint64]map[uint64][]model.TimedAlertPoint{1: {1: nil}}
	alertsPrevState = map[uint64]map[uint64]uint8{1: {}}
	alertsLastMetricSeq = map[uint64]map[uint64]uint64{1: {}}
	AlertsCycleTransferStatsStore = map[uint64]*model.CycleTransferStats{1: stats}

	cycleStart := time.Now().Add(-time.Hour)
	alert := &model.AlertRule{
		Common: model.Common{ID: 1},
		Rules: []*model.Rule{{
			Type: "transfer_all_cycle", Max: 1 << 40, CycleStart: &cycleStart, CycleInterval: 1, CycleUnit: "day", Cover: model.RuleCoverAll,
		}},
	}
	server := &model.Server{Common: model.Common{ID: 1}, Name: "server"}
	model.InitServer(server)
	return alert, server
}

// The alert evaluator rewrites the per-server maps of a cycle transfer rule
// while the public /service endpoint reads them. Run with -race: an unguarded
// read is reported here, and in production it aborts the dashboard process.
// The writer is the real evaluator, so removing its lock fails this test.
func TestCopyCycleTransferStatsIsSafeAgainstTheAlertEvaluator(t *testing.T) {
	stats := &model.CycleTransferStats{Name: "monthly", ServerName: map[uint64]string{}, Transfer: map[uint64]uint64{}, NextUpdate: map[uint64]time.Time{}}
	alert, server := cycleTransferRuleFixture(t, stats)
	lease := server.AttachStateStream(alertTestStateStream{})

	const rounds = 300
	var evaluator sync.WaitGroup
	evaluator.Add(1)
	go func() {
		defer evaluator.Done()
		for round := uint64(1); round <= rounds; round++ {
			if !lease.UpdateState(&model.HostState{NetInTransfer: round, NetOutTransfer: round}, time.Now()) {
				t.Error("state update rejected")
				return
			}
			// The rule skips evaluation until its next check time; clear it so
			// every round writes the statistics.
			alert.Rules[0].NextTransferAt = nil
			AlertsLock.RLock()
			checkStatusForServer(alert, server)
			AlertsLock.RUnlock()
		}
	}()
	for round := 0; round < rounds; round++ {
		for _, copied := range CopyCycleTransferStats() {
			for serverID := range copied.Transfer {
				_ = copied.ServerName[serverID]
			}
		}
	}
	evaluator.Wait()

	// The first report is the baseline the cycle counts from.
	if got, want := stats.Transfer[1], uint64(2*(rounds-1)); got != want {
		t.Fatalf("the evaluator recorded %d transferred bytes, want %d: it did not write the statistics every round", got, want)
	}
	// The copy must not share maps with the store: callers filter it per viewer.
	copied := CopyCycleTransferStats()
	copied[1].Transfer[999] = 1
	if _, leaked := stats.Transfer[999]; leaked {
		t.Fatal("the returned statistics share a map with the store")
	}
	if copied[1].Name != "monthly" || copied[1].ServerName[1] != "server" {
		t.Fatalf("copy = name %q, server name %q; want the rule name and the evaluated server", copied[1].Name, copied[1].ServerName[1])
	}
}

// checkStatusForServer recovers panics. A panic while it held the statistics
// lock used to leave the lock taken for good, hanging every later evaluation
// and the public /service endpoint.
func TestCycleTransferStatsLockIsReleasedWhenEvaluationPanics(t *testing.T) {
	// Statistics without maps make the evaluator panic when it records the result.
	alert, server := cycleTransferRuleFixture(t, &model.CycleTransferStats{Name: "monthly"})
	lease := server.AttachStateStream(alertTestStateStream{})
	if !lease.UpdateState(&model.HostState{NetInTransfer: 1, NetOutTransfer: 1}, time.Now()) {
		t.Fatal("state update rejected")
	}

	checkStatusForServer(alert, server)

	if alertsLastMetricSeq[1][1] == 0 {
		t.Fatal("test setup: the rule was not evaluated")
	}
	if len(alertsStore[1][1]) != 0 {
		t.Fatal("test setup: evaluation was expected to panic before it stored a sample")
	}
	copied := make(chan struct{})
	go func() {
		CopyCycleTransferStats()
		close(copied)
	}()
	select {
	case <-copied:
	case <-time.After(5 * time.Second):
		t.Fatal("the statistics lock is still held after the evaluator panicked")
	}
}
