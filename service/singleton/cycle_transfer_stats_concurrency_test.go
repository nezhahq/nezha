package singleton

import (
	"sync"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
)

// The alert evaluator rewrites the per-server maps of a cycle transfer rule
// while the public /service endpoint reads them. Run with -race: an unguarded
// read is reported here, and in production it aborts the dashboard process.
func TestCopyCycleTransferStatsIsSafeAgainstTheAlertEvaluator(t *testing.T) {
	previous := AlertsCycleTransferStatsStore
	t.Cleanup(func() { AlertsCycleTransferStatsStore = previous })
	AlertsCycleTransferStatsStore = map[uint64]*model.CycleTransferStats{
		1: {Name: "monthly", ServerName: map[uint64]string{}, Transfer: map[uint64]uint64{}, NextUpdate: map[uint64]time.Time{}},
	}

	var evaluator sync.WaitGroup
	evaluator.Add(1)
	go func() {
		defer evaluator.Done()
		for round := uint64(0); round < 2000; round++ {
			// What checkStatusForServer does while it evaluates a rule.
			AlertsLock.RLock()
			cycleTransferStatsMu.Lock()
			stats := AlertsCycleTransferStatsStore[1]
			stats.ServerName[round%50] = "server"
			stats.Transfer[round%50] = round
			stats.NextUpdate[round%50] = time.Now()
			cycleTransferStatsMu.Unlock()
			AlertsLock.RUnlock()
		}
	}()
	for round := 0; round < 2000; round++ {
		for _, stats := range CopyCycleTransferStats() {
			for serverID := range stats.Transfer {
				_ = stats.ServerName[serverID]
			}
		}
	}
	evaluator.Wait()

	// The copy must not share maps with the store: callers filter it per viewer.
	copied := CopyCycleTransferStats()
	copied[1].Transfer[999] = 1
	if _, leaked := AlertsCycleTransferStatsStore[1].Transfer[999]; leaked {
		t.Fatal("the returned statistics share a map with the store")
	}
	if len(copied[1].Transfer) != 51 || copied[1].Name != "monthly" {
		t.Fatalf("copy = %d transfer entries, name %q; want the 50 written entries plus the probe and the rule name", len(copied[1].Transfer), copied[1].Name)
	}
}
