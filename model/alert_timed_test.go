package model

import (
	"testing"
	"time"
)

func timedRule(kind string, duration uint64) *AlertRule {
	return &AlertRule{Rules: []*Rule{{Type: kind, Duration: duration, Max: 80, Cover: RuleCoverAll}}}
}

func timedPoints(start time.Time, values []bool, spacing time.Duration) []TimedAlertPoint {
	points := make([]TimedAlertPoint, len(values))
	for i, value := range values {
		points[i] = TimedAlertPoint{At: start.Add(time.Duration(i) * spacing), Values: []bool{value}}
	}
	return points
}

func TestTimedAlertNeedsElapsedWindowAndDistinctReports(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	rule := timedRule("cpu", 5)
	points := timedPoints(start, []bool{false, false, false, false}, AlertSampleInterval)
	if known, _ := rule.CheckTimed(points, points[len(points)-1].At); known {
		t.Fatal("four reports decided a five-tick window")
	}
	points = append(points, TimedAlertPoint{At: start.Add(12 * time.Second), Values: []bool{false}})
	if known, passed := rule.CheckTimed(points, points[len(points)-1].At); !known || passed {
		t.Fatalf("complete high-CPU window: known=%v passed=%v", known, passed)
	}
	// Repeating the same observation does not advance wall-clock coverage.
	if known, _ := rule.CheckTimed(points, start.Add(30*time.Second)); known {
		t.Fatal("stale sample created a new complete window")
	}
}

func TestTimedAlertGapIsUnknownAndCannotResolve(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	rule := timedRule("cpu", 5)
	points := []TimedAlertPoint{
		{At: start, Values: []bool{false}},
		{At: start.Add(30 * time.Second), Values: []bool{true}},
	}
	if known, _ := rule.CheckTimed(points, start.Add(30*time.Second)); known {
		t.Fatal("telemetry gap must not be interpreted as healthy")
	}
}

func TestTimedAlertIrregularReportsUseElapsedTime(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	rule := timedRule("cpu", 6)
	points := timedPoints(start, []bool{false, false, false, false}, 5*time.Second)
	if known, passed := rule.CheckTimed(points, start.Add(15*time.Second)); !known || passed {
		t.Fatalf("irregular reports cover the wall-clock window: known=%v passed=%v", known, passed)
	}
}

func TestTimedAlertExactSeventyPercentIsNotIncident(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	rule := timedRule("cpu", 10)
	values := []bool{false, false, false, false, false, false, false, true, true, true}
	points := timedPoints(start, values, AlertSampleInterval)
	if known, passed := rule.CheckTimed(points, points[len(points)-1].At); !known || !passed {
		t.Fatalf("70%% failed is below the >70%% trigger: known=%v passed=%v", known, passed)
	}
}

func TestTimedAlertTenTickWindowDoesNotDecideAtNine(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	rule := timedRule("cpu", 10)
	points := timedPoints(start, []bool{false, false, false, false, false, false, false, false, false}, AlertSampleInterval)
	if known, _ := rule.CheckTimed(points, points[len(points)-1].At); known {
		t.Fatal("nine reports decided a ten-tick window one tick early")
	}
	points = append(points, TimedAlertPoint{At: start.Add(27 * time.Second), Values: []bool{false}})
	if known, passed := rule.CheckTimed(points, points[len(points)-1].At); !known || passed {
		t.Fatalf("full ten-tick breach: known=%v passed=%v", known, passed)
	}
}

func TestTimedOfflineRequiresSustainedAbsence(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	rule := timedRule("offline", 5)
	points := timedPoints(start, []bool{false, false, false, false, false}, AlertSampleInterval)
	if known, passed := rule.CheckTimed(points, points[len(points)-1].At); !known || passed {
		t.Fatalf("continuous offline interval: known=%v passed=%v", known, passed)
	}
	points = append(points, TimedAlertPoint{At: start.Add(15 * time.Second), Values: []bool{true}})
	if known, passed := rule.CheckTimed(points, points[len(points)-1].At); !known || !passed {
		t.Fatalf("online observation should resolve offline: known=%v passed=%v", known, passed)
	}
}

func TestTimedCompoundRuleWaitsForAllKnown(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	rule := &AlertRule{Rules: []*Rule{
		{Type: "cpu", Duration: 5, Max: 80, Cover: RuleCoverAll},
		{Type: "memory", Duration: 5, Max: 80, Cover: RuleCoverAll},
	}}
	points := timedPoints(start, []bool{false, false, false, false, false}, AlertSampleInterval)
	if known, _ := rule.CheckTimed(points, points[len(points)-1].At); known {
		t.Fatal("missing second condition treated as decisive")
	}
}
