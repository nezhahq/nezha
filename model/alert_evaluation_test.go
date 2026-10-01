package model

import (
	"math"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestAlertEvaluationDistinguishesUnknownFromBreach(t *testing.T) {
	server := &Server{Common: Common{ID: 1}}
	rule := &Rule{Type: "cpu", Max: 80, Cover: RuleCoverAll}
	if passed, known := rule.Evaluate(nil, server, nil); !passed || known {
		t.Fatalf("missing state = (%v, %v), want unknown", passed, known)
	}
	lease := server.AttachStateStream(runtimeOwnershipStream{})
	if !lease.UpdateState(&HostState{CPU: 99}, time.Now()) {
		t.Fatal("state update rejected")
	}
	if passed, known := rule.Evaluate(nil, server, nil); passed || !known {
		t.Fatalf("fresh high CPU = (%v, %v), want known breach", passed, known)
	}
	if !lease.Clear() {
		t.Fatal("stream clear rejected")
	}
	// The raw metric remains available for historical display. The sentinel
	// enforces freshness and must not count this value after disconnect.
	if !server.RuntimeSnapshot().LastActive.IsZero() {
		t.Fatal("disconnect did not clear online visibility")
	}
}

func newCycleTestDB(t *testing.T, migrate bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if migrate {
		if err := db.AutoMigrate(&Transfer{}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func newCycleTestStats() *CycleTransferStats {
	return &CycleTransferStats{
		ServerName: make(map[uint64]string),
		Transfer:   make(map[uint64]uint64),
		NextUpdate: make(map[uint64]time.Time),
	}
}

func TestCycleRuleRejectsInvalidConfiguration(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	rule := &Rule{Type: "transfer_out_cycle", CycleStart: &start, CycleInterval: 1, CycleUnit: "hour", Max: 100}
	if !rule.HasSafeCycleConfiguration() {
		t.Fatal("normal hourly cycle rejected")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Rule)
	}{
		{"unknown unit", func(r *Rule) { r.CycleUnit = "fortnight" }},
		{"negative limit", func(r *Rule) { r.Max = -1 }},
		{"NaN limit", func(r *Rule) { r.Max = math.NaN() }},
		{"reversed limits", func(r *Rule) { r.Min, r.Max = 200, 100 }},
		{"future start", func(r *Rule) { future := time.Now().Add(time.Hour); r.CycleStart = &future }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *rule
			tc.mutate(&copy)
			if copy.HasSafeCycleConfiguration() {
				t.Fatal("unsafe cycle accepted")
			}
		})
	}
}

func TestCycleBoundsUseOneClockAtBoundary(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rule := &Rule{Type: "transfer_out_cycle", CycleStart: &start, CycleInterval: 1, CycleUnit: "hour", Max: 100}
	before := start.Add(59*time.Minute + 59*time.Second)
	from, to := rule.transferDurationBounds(before)
	if !from.Equal(start) || !to.Equal(start.Add(time.Hour)) {
		t.Fatalf("before boundary = %v..%v", from, to)
	}
	from, to = rule.transferDurationBounds(start.Add(time.Hour))
	if !from.Equal(start.Add(time.Hour)) || !to.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("at boundary = %v..%v", from, to)
	}
}

func TestCycleQueryFailureIsUnknownAndDoesNotCache(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	rule := &Rule{Type: "transfer_out_cycle", CycleStart: &start, CycleInterval: 1, CycleUnit: "hour", Max: 50}
	server := &Server{Common: Common{ID: 1}, State: &HostState{}}
	passed, known := rule.Evaluate(newCycleTestStats(), server, newCycleTestDB(t, false))
	if !passed || known || len(rule.NextTransferAt) != 0 {
		t.Fatalf("SQL error yielded passed=%v known=%v cache=%v", passed, known, rule.NextTransferAt)
	}
}

func TestCycleQueryExcludesFutureRowsAndHandlesMinOnly(t *testing.T) {
	start := time.Now().Add(-90 * time.Minute)
	db := newCycleTestDB(t, true)
	server := &Server{Common: Common{ID: 1}, State: &HostState{}}
	for _, row := range []Transfer{
		{Common: Common{CreatedAt: time.Now().Add(-time.Minute)}, ServerID: 1, Out: 10},
		{Common: Common{CreatedAt: time.Now().Add(10 * time.Minute)}, ServerID: 1, Out: 100},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	rule := &Rule{Type: "transfer_out_cycle", CycleStart: &start, CycleInterval: 1, CycleUnit: "hour", Max: 50}
	stats := newCycleTestStats()
	passed, known := rule.Evaluate(stats, server, db)
	if !passed || !known || stats.Transfer[1] != 10 {
		t.Fatalf("future row included: passed=%v known=%v amount=%d", passed, known, stats.Transfer[1])
	}
	minOnly := &Rule{Type: "transfer_out_cycle", CycleStart: &start, CycleInterval: 1, CycleUnit: "hour", Min: 20}
	passed, known = minOnly.Evaluate(newCycleTestStats(), server, db)
	if passed || !known {
		t.Fatalf("min-only cycle = (%v,%v), want known breach", passed, known)
	}
}

func TestAlertEvaluationAllNetworkSpeedUsesBothDirections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in, out uint64
		breach  bool
	}{
		{"inbound only", 100, 0, true},
		{"outbound below combined limit", 0, 30, false},
		{"both directions", 25, 30, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &Server{Common: Common{ID: 1}, State: &HostState{NetInSpeed: tc.in, NetOutSpeed: tc.out}}
			rule := &Rule{Type: "net_all_speed", Max: 50, Cover: RuleCoverAll}
			passed, known := rule.Evaluate(nil, server, nil)
			if !known || passed == tc.breach {
				t.Fatalf("got passed=%v known=%v, want breach=%v", passed, known, tc.breach)
			}
		})
	}
}

func TestAlertEvaluationUnavailableSensorsAreUnknown(t *testing.T) {
	tests := []struct {
		name  string
		rule  string
		state *HostState
		host  *Host
	}{
		{"missing memory host", "memory", &HostState{MemUsed: 1}, nil},
		{"zero memory total", "memory", &HostState{MemUsed: 1}, &Host{}},
		{"missing GPU", "gpu_max", &HostState{}, &Host{}},
		{"missing temperature", "temperature_max", &HostState{}, &Host{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := &Server{Common: Common{ID: 1}, State: tc.state, Host: tc.host}
			passed, known := (&Rule{Type: tc.rule, Max: 50, Cover: RuleCoverAll}).Evaluate(nil, server, nil)
			if !passed || known {
				t.Fatalf("got passed=%v known=%v, want unknown", passed, known)
			}
		})
	}
}

func TestOfflineRuleDoesNotRequireMetricState(t *testing.T) {
	server := &Server{Common: Common{ID: 1}}
	InitServer(server)
	passed, known := (&Rule{Type: "offline", Cover: RuleCoverAll}).Evaluate(nil, server, nil)
	if passed || !known {
		t.Fatalf("offline without state = (%v, %v), want known breach", passed, known)
	}
}

func TestCompoundAlertUsesOneImmutableAgentReport(t *testing.T) {
	server := &Server{Common: Common{ID: 1}}
	alert := &AlertRule{Rules: []*Rule{
		{Type: "cpu", Max: 80, Cover: RuleCoverAll},
		{Type: "memory", Max: 80, Cover: RuleCoverAll},
	}}
	oldReport := RuntimeSnapshot{
		State: &HostState{CPU: 99, MemUsed: 0},
		Host:  &Host{MemTotal: 100},
	}
	newReport := RuntimeSnapshot{
		State: &HostState{CPU: 0, MemUsed: 99},
		Host:  &Host{MemTotal: 100},
	}
	for _, report := range []RuntimeSnapshot{oldReport, newReport} {
		point, known := alert.SnapshotStatusWithRuntime(nil, server, report, nil)
		if !known || len(point) != 2 || point[0] == point[1] {
			t.Fatalf("compound point combined incompatible reports: known=%v point=%v", known, point)
		}
	}
}
