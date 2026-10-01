package singleton

import (
	"testing"

	"github.com/nezhahq/nezha/model"
)

// notifyDecision replays the exact send-gate from checkStatus (lines 164-186)
// for a single (alert, server) pair: given the current Check verdict and the
// previous stored state, it reports whether an incident or recovery notification
// would be dispatched and what the next stored state becomes. Kept in lockstep
// with checkStatus so the end-to-end "does it actually notify" path is testable
// without the DB/global singletons checkStatus pulls in.
func notifyDecision(triggerMode uint8, passed bool, prev uint8) (incident, recover bool, next uint8) {
	if !passed {
		if triggerMode == model.ModeAlwaysTrigger || prev != _RuleCheckFail {
			return true, false, _RuleCheckFail
		}
		return false, false, _RuleCheckFail
	}
	if prev == _RuleCheckFail {
		return false, true, _RuleCheckPass
	}
	return false, false, _RuleCheckPass
}

// driveCheckStatus simulates the checkStatus tick loop end-to-end: each tick
// appends one sample, runs the real Check, applies the real RetentionWindow
// trim, then runs the send-gate. It returns how many incident notifications
// would have been dispatched across all ticks and the final sample window size.
func driveCheckStatus(rule *model.AlertRule, triggerMode uint8, ticks int, sample []bool) (incidents int, finalWindow int) {
	incidents, finalWindow, _ = driveCheckStatusCap(rule, triggerMode, ticks, sample)
	return incidents, finalWindow
}

// driveCheckStatusCap additionally reports the peak length and capacity the
// sample slice ever reached, so tests can assert memory stays bounded.
func driveCheckStatusCap(rule *model.AlertRule, triggerMode uint8, ticks int, sample []bool) (incidents, finalLen, peakCap int) {
	var samples [][]bool
	prev := uint8(_RuleCheckNoData)
	for i := 0; i < ticks; i++ {
		samples = append(samples, append([]bool(nil), sample...))
		_, passed := rule.Check(samples)
		w := rule.RetentionWindow()
		if w <= 0 {
			samples = samples[:0]
		} else if w < len(samples) {
			samples = samples[len(samples)-w:]
		}
		if cap(samples) > peakCap {
			peakCap = cap(samples)
		}
		incident, _, next := notifyDecision(triggerMode, passed, prev)
		if incident {
			incidents++
		}
		prev = next
	}
	return incidents, len(samples), peakCap
}

// TestCheckStatus_GeneralRuleFiresIncident is the end-to-end guard for the
// regression: a Duration:10 rule on a server that fails every tick must,
// after the window fills, reach passed=false and actually dispatch an incident
// notification. Before the fix the window was wiped each tick, so passed never
// became false and zero notifications were sent.
func TestCheckStatus_GeneralRuleFiresIncident(t *testing.T) {
	rule := &model.AlertRule{Rules: []*model.Rule{{Type: "cpu", Duration: 10}}}

	t.Run("AlwaysTrigger fires repeatedly once window fills", func(t *testing.T) {
		incidents, window := driveCheckStatus(rule, model.ModeAlwaysTrigger, 30, []bool{false})
		if window < 10 {
			t.Fatalf("window never filled: got %d want >= 10", window)
		}
		if incidents == 0 {
			t.Fatalf("AlwaysTrigger rule never dispatched an incident notification")
		}
	})

	t.Run("OnetimeTrigger fires exactly once", func(t *testing.T) {
		incidents, _ := driveCheckStatus(rule, model.ModeOnetimeTrigger, 30, []bool{false})
		if incidents != 1 {
			t.Fatalf("OnetimeTrigger must dispatch exactly one incident, got %d", incidents)
		}
	})
}

// TestCheckStatus_HealthyServerStaysSilent guards the other direction: a server
// passing every tick must never dispatch an incident.
func TestCheckStatus_HealthyServerStaysSilent(t *testing.T) {
	rule := &model.AlertRule{Rules: []*model.Rule{{Type: "cpu", Duration: 10}}}
	incidents, _ := driveCheckStatus(rule, model.ModeAlwaysTrigger, 30, []bool{true})
	if incidents != 0 {
		t.Fatalf("a healthy server must never trigger an incident, got %d", incidents)
	}
}

// TestCheckStatus_SampleMemoryBounded pins the no-memory-leak invariant: no
// matter how many ticks run, the per-(alert,server) sample slice length and
// capacity stay bounded by the rule's retention window, never growing with
// elapsed time. Runs far more ticks than the window to expose any unbounded
// growth.
func TestCheckStatus_SampleMemoryBounded(t *testing.T) {
	const duration = 10
	rule := &model.AlertRule{Rules: []*model.Rule{{Type: "cpu", Duration: duration}}}

	_, finalLen, peakCap := driveCheckStatusCap(rule, model.ModeAlwaysTrigger, 100000, []bool{false})

	if finalLen > duration {
		t.Fatalf("sample length exceeded retention window after many ticks: got %d want <= %d", finalLen, duration)
	}
	// append grows capacity geometrically; with length pinned at window+1 the
	// backing array stabilises at a small constant. A generous 4x window bound
	// catches any reintroduced unbounded growth without being flaky.
	if peakCap > duration*4 {
		t.Fatalf("sample capacity grew unbounded: peak cap %d exceeds 4x window %d", peakCap, duration*4)
	}
}

func TestCheckStatusForServerRecoversUnexpectedEvaluatorPanic(t *testing.T) {
	originalStore := alertsStore
	originalPrevState := alertsPrevState
	originalCycleStore := AlertsCycleTransferStatsStore
	alertsStore = map[uint64]map[uint64][]model.TimedAlertPoint{}
	alertsPrevState = map[uint64]map[uint64]uint8{}
	AlertsCycleTransferStatsStore = map[uint64]*model.CycleTransferStats{}
	t.Cleanup(func() {
		alertsStore = originalStore
		alertsPrevState = originalPrevState
		AlertsCycleTransferStatsStore = originalCycleStore
	})

	enabled := true
	alert := &model.AlertRule{
		Common: model.Common{ID: 77, UserID: 2},
		Enable: &enabled,
		Rules:  []*model.Rule{{Type: "cpu", Duration: 3, Cover: model.RuleCoverAll}},
	}
	server := &model.Server{Common: model.Common{ID: 1, UserID: 2}}
	model.InitServer(server)

	// The deliberately missing alertsStore[alert.ID] map panics at assignment
	// after evaluation. The per-alert/server recovery boundary must contain it.
	checkStatusForServer(alert, server)
}

func TestAddCycleTransferStatsSkipsPoisonedPersistedRule(t *testing.T) {
	original := AlertsCycleTransferStatsStore
	AlertsCycleTransferStatsStore = map[uint64]*model.CycleTransferStats{}
	t.Cleanup(func() { AlertsCycleTransferStatsStore = original })

	enabled := true
	alert := &model.AlertRule{
		Common: model.Common{ID: 88},
		Enable: &enabled,
		Rules: []*model.Rule{{
			Type:          "transfer_in_cycle",
			CycleInterval: 1,
			Cover:         model.RuleCoverAll,
			// CycleStart intentionally nil, as it may be in a poisoned legacy DB row.
		}},
	}

	addCycleTransferStatsInfo(alert)
	if _, exists := AlertsCycleTransferStatsStore[alert.ID]; exists {
		t.Fatal("invalid persisted cycle rule must not be scheduled")
	}
}
