package model

import (
	"slices"
	"testing"
	"time"
)

type arSt struct {
	msg    string
	rule   *AlertRule
	points [][]bool
	expD   int
	exp    bool
}

func TestAlertRules(t *testing.T) {
	t.Run("CycleRules", testCycleRules)
	t.Run("OfflineRules", testOfflineRules)
	t.Run("GeneralRules", testGeneralRules)
	t.Run("CombinedRules", testCombinedRules)
}

func testCycleRules(t *testing.T) {
	cases := []arSt{
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Type: "_cycle",
					},
				},
			},
			msg:    "CyclePass",
			points: [][]bool{{false}, {true}},
			expD:   1,
			exp:    true,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Type: "_cycle",
					},
				},
			},
			msg:    "CycleFail",
			points: [][]bool{{true}, {false}},
			expD:   1,
			exp:    false,
		},
	}

	for _, c := range cases {
		d, passed := c.rule.Check(c.points)
		assertEq(t, c.msg, c.expD, d)
		assertEq(t, c.msg, c.exp, passed)
	}
}

func testOfflineRules(t *testing.T) {
	cases := []arSt{
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Type:     "offline",
						Duration: 10,
					},
				},
			},
			msg:    "OfflineLast",
			points: append([][]bool{{true}}, repeat([]bool{false}, 9)...),
			expD:   10,
			exp:    true,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Type:     "offline",
						Duration: 10,
					},
				},
			},
			msg:    "OfflineMiddle",
			points: mod(repeat([]bool{false}, 10), true, 5),
			expD:   5,
			exp:    true,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Type:     "offline",
						Duration: 10,
					},
				},
			},
			msg:    "OfflineFirst",
			points: mod(repeat([]bool{false}, 10), true, 9),
			expD:   1,
			exp:    true,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Type:     "offline",
						Duration: 10,
					},
				},
			},
			msg:    "OfflineBoundCheck",
			points: repeat([]bool{false}, 9),
			expD:   0,
			exp:    true,
		},
	}

	for _, c := range cases {
		d, passed := c.rule.Check(c.points)
		assertEq(t, c.msg, c.expD, d)
		assertEq(t, c.msg, c.exp, passed)
	}
}

func testGeneralRules(t *testing.T) {
	cases := []arSt{
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Duration: 10,
					},
				},
			},
			msg:    "GeneralFail",
			points: repeat([]bool{false}, 10),
			expD:   10,
			exp:    false,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Duration: 10,
					},
				},
			},
			msg:    "GeneralFail80%",
			points: slices.Concat(repeat([]bool{false}, 8), repeat([]bool{true}, 2)),
			expD:   10,
			exp:    false,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Duration: 10,
					},
				},
			},
			msg:    "GeneralPass30%",
			points: slices.Concat(repeat([]bool{false}, 7), repeat([]bool{true}, 3)),
			expD:   10,
			exp:    true,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Duration: 10,
					},
				},
			},
			msg:    "GeneralPass",
			points: slices.Concat(repeat([]bool{false}, 4), repeat([]bool{true}, 6)),
			expD:   10,
			exp:    true,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Duration: 10,
					},
				},
			},
			msg:    "GeneralBoundCheck",
			points: repeat([]bool{false}, 9),
			expD:   0,
			exp:    true,
		},
	}

	for _, c := range cases {
		d, passed := c.rule.Check(c.points)
		assertEq(t, c.msg, c.expD, d)
		assertEq(t, c.msg, c.exp, passed)
	}
}

func testCombinedRules(t *testing.T) {
	cases := []arSt{
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Type:     "offline",
						Duration: 10,
					},
					{
						Duration: 10,
					},
				},
			},
			msg:    "OfflineGeneralOfflinePass",
			points: slices.Concat(repeat([]bool{false, true}, 2), repeat([]bool{true, false}, 8)),
			expD:   1,
			exp:    true,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Type:     "offline",
						Duration: 10,
					},
					{
						Duration: 10,
					},
				},
			},
			msg:    "OfflineGeneralOfflineFail",
			points: slices.Concat(repeat([]bool{false, false}, 2), repeat([]bool{false, true}, 8)),
			expD:   10,
			exp:    true,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Duration: 10,
					},
					{
						Type:     "offline",
						Duration: 10,
					},
				},
			},
			msg:    "GeneralOffline",
			points: slices.Concat(repeat([]bool{false, true}, 2), repeat([]bool{true, false}, 8)),
			expD:   10,
			exp:    true,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Duration: 10,
					},
					{
						Duration: 30,
					},
				},
			},
			msg:    "GeneralGeneral",
			points: slices.Concat(repeat([]bool{false, true}, 2), repeat([]bool{false, false}, 28)),
			expD:   30,
			exp:    false,
		},
		{
			rule: &AlertRule{
				Rules: []*Rule{
					{
						Duration: 10,
					},
					{
						Duration: 30,
					},
				},
			},
			msg:    "CombinedBoundCheck",
			points: slices.Concat(repeat([]bool{false, true}, 2), repeat([]bool{false, false}, 27)),
			expD:   10,
			exp:    true,
		},
	}

	for _, c := range cases {
		d, passed := c.rule.Check(c.points)
		assertEq(t, c.msg, c.expD, d)
		assertEq(t, c.msg, c.exp, passed)
	}
}

func repeat[S ~[]E, E any](x S, count int) []S {
	var slices []S
	for range count {
		tmp := make([]E, len(x))
		copy(tmp, x)
		slices = append(slices, tmp)
	}
	return slices
}

func mod[S ~[][]E, E any](x S, val E, i int) S {
	x[i][0] = val
	return x
}

func assertEq(t *testing.T, msg string, exp, act any) {
	t.Helper()
	if exp != act {
		t.Fatalf("failed to test for %s. exp=[%v] but act=[%v]", msg, exp, act)
	}
}

// TestAlertRule_ZeroDurationGeneralRule guards against a config-reachable DoS:
// a general rule with Duration:0 (the API validates Duration as "optional" with
// no minimum) previously hit fail*100/total with total==0, panicking with an
// integer divide-by-zero inside checkStatus — which has no recover and would
// take down the whole alert goroutine. boundCheck now treats duration<=0 as a
// passed (no-op) rule, so Check must return without panicking.
func TestAlertRule_ZeroDurationGeneralRule(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Check panicked on a Duration:0 general rule (config-reachable DoS): %v", r)
		}
	}()

	rule := &AlertRule{
		Rules: []*Rule{{Type: "cpu", Duration: 0}},
	}
	// The only contract here is "do not panic". A zero-duration rule is skipped,
	// so it contributes nothing to the verdict and max stays 0.
	maxD, _ := rule.Check([][]bool{{true}, {false}})
	if maxD != 0 {
		t.Fatalf("a skipped Duration:0 rule must not contribute to max, got %d", maxD)
	}
}

// Mixing a valid rule with a zero-duration rule must also be safe: the zero
// rule is skipped, the real rule still drives the verdict.
func TestAlertRule_ZeroDurationMixedWithValidRule(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Check panicked on a mixed zero/valid rule set: %v", r)
		}
	}()

	rule := &AlertRule{
		Rules: []*Rule{
			{Type: "cpu", Duration: 0},
			{Type: "cpu", Duration: 3},
		},
	}
	maxD, _ := rule.Check([][]bool{{true, false}, {true, false}, {true, false}})
	if maxD != 3 {
		t.Fatalf("the valid Duration:3 rule must still set max=3, got %d", maxD)
	}
}

// trimSamples mirrors singleton.checkStatus retention: keep the most recent
// `window` samples, clear when window<=0. window comes from RetentionWindow(),
// the production code under test.
func trimSamples(samples [][]bool, window int) [][]bool {
	if window <= 0 {
		return samples[:0]
	} else if window < len(samples) {
		return samples[len(samples)-window:]
	}
	return samples
}

// TestAlertRule_GeneralRuleAccumulatesSamples is a regression guard: a normal
// Duration>1 general rule must be able to fire. checkStatus appends one sample
// per tick then trims to the retention window; if the window is derived from
// Check's verdict (which is 0 while the rule is still filling) the history is
// wiped every tick, the window never reaches Duration, and the alert never
// raises. RetentionWindow() must keep enough samples for the rule to converge.
func TestAlertRule_GeneralRuleAccumulatesSamples(t *testing.T) {
	const duration = 10
	rule := &AlertRule{
		Rules: []*Rule{{Type: "cpu", Duration: duration}},
	}

	var samples [][]bool
	var lastPassed bool
	maxLen := 0
	for tick := 0; tick < duration*3; tick++ {
		samples = append(samples, []bool{false}) // failing sample
		_, lastPassed = rule.Check(samples)
		samples = trimSamples(samples, rule.RetentionWindow())
		if len(samples) > maxLen {
			maxLen = len(samples)
		}
	}

	if maxLen < duration {
		t.Fatalf("samples never accumulated to Duration: max window reached %d, want >= %d", maxLen, duration)
	}
	if lastPassed {
		t.Fatalf("a server failing every tick must eventually fail the check (passed=false), got passed=true")
	}
}

// TestAlertRule_RetentionWindow pins the retention contract directly.
func TestAlertRule_RetentionWindow(t *testing.T) {
	cases := []struct {
		msg  string
		rule *AlertRule
		want int
	}{
		{"single general", &AlertRule{Rules: []*Rule{{Type: "cpu", Duration: 10}}}, 10},
		{"zero duration only", &AlertRule{Rules: []*Rule{{Type: "cpu", Duration: 0}}}, 0},
		{"mixed picks max", &AlertRule{Rules: []*Rule{{Type: "cpu", Duration: 0}, {Type: "cpu", Duration: 7}}}, 7},
		{"offline keeps Duration", &AlertRule{Rules: []*Rule{{Type: "offline", Duration: 30}}}, 30},
		{"cycle looks back one", &AlertRule{Rules: []*Rule{{Type: "net_in_speed_cycle"}}}, 1},
	}
	for _, c := range cases {
		if got := c.rule.RetentionWindow(); got != c.want {
			t.Fatalf("%s: RetentionWindow()=%d want %d", c.msg, got, c.want)
		}
	}
}

// TestAlertRule_OfflineRuleAccumulatesSamples is a regression guard for offline
// alerts that never fire. Check's offline branch reads points[len-Duration:],
// so it needs Duration samples retained; if RetentionWindow trims to 1 (as an
// earlier fix wrongly did for offline rules), the window never reaches Duration,
// boundCheck keeps returning "passed", and the offline alert never raises.
func TestAlertRule_OfflineRuleAccumulatesSamples(t *testing.T) {
	const duration = 10
	rule := &AlertRule{Rules: []*Rule{{Type: "offline", Duration: duration}}}

	var samples [][]bool
	var lastPassed bool
	maxLen := 0
	for tick := 0; tick < duration*3; tick++ {
		samples = append(samples, []bool{false}) // offline sample
		_, lastPassed = rule.Check(samples)
		samples = trimSamples(samples, rule.RetentionWindow())
		if len(samples) > maxLen {
			maxLen = len(samples)
		}
	}

	if maxLen < duration {
		t.Fatalf("offline samples never accumulated to Duration: max window reached %d, want >= %d", maxLen, duration)
	}
	if lastPassed {
		t.Fatalf("a server offline every tick must eventually fail the offline check (passed=false), got passed=true")
	}
}

// TestAlertRule_CombinedRuleAccumulatesSamples drives the real trim loop for
// mixed-type alerts. The verdict is AND-of-failure: an incident fires only once
// every rule's lookback window is full and all fail. RetentionWindow must keep
// enough samples for the largest window (offline/general need Duration, cycle
// needs 1); if any rule type is under-retained the alert never fires.
func TestAlertRule_CombinedRuleAccumulatesSamples(t *testing.T) {
	cases := []struct {
		msg        string
		rule       *AlertRule
		sample     []bool
		fireAt     int // tick index where passed must first become false
		wantWindow int
	}{
		{"general3+general10", &AlertRule{Rules: []*Rule{{Type: "cpu", Duration: 3}, {Type: "memory", Duration: 10}}}, []bool{false, false}, 9, 10},
		{"offline5+general10", &AlertRule{Rules: []*Rule{{Type: "offline", Duration: 5}, {Type: "cpu", Duration: 10}}}, []bool{false, false}, 9, 10},
		{"transfer+general8", &AlertRule{Rules: []*Rule{{Type: "net_in_speed_cycle"}, {Type: "cpu", Duration: 8}}}, []bool{false, false}, 7, 8},
		{"offline3+offline12", &AlertRule{Rules: []*Rule{{Type: "offline", Duration: 3}, {Type: "offline", Duration: 12}}}, []bool{false, false}, 11, 12},
	}
	for _, c := range cases {
		if got := c.rule.RetentionWindow(); got != c.wantWindow {
			t.Fatalf("%s: RetentionWindow()=%d want %d", c.msg, got, c.wantWindow)
		}
		var samples [][]bool
		firstFire := -1
		for tick := 0; tick < c.wantWindow*3; tick++ {
			samples = append(samples, append([]bool(nil), c.sample...))
			if _, passed := c.rule.Check(samples); !passed && firstFire < 0 {
				firstFire = tick
			}
			samples = trimSamples(samples, c.rule.RetentionWindow())
		}
		if firstFire != c.fireAt {
			t.Fatalf("%s: alert first fired at tick %d, want %d (never-firing = -1)", c.msg, firstFire, c.fireAt)
		}
	}
}

func TestRuleMonthlyTransferDurationKeepsAnchorDayAcrossShortFebruary(t *testing.T) {
	loc := time.UTC
	start := time.Date(2026, time.January, 29, 0, 0, 0, 0, loc)
	now := time.Date(2026, time.March, 15, 12, 0, 0, 0, loc)
	oldNow := ruleNow
	ruleNow = func() time.Time { return now }
	t.Cleanup(func() { ruleNow = oldNow })

	rule := &Rule{
		Type:          "transfer_all_cycle",
		CycleStart:    &start,
		CycleInterval: 1,
		CycleUnit:     "month",
	}

	wantStart := time.Date(2026, time.February, 28, 0, 0, 0, 0, loc)
	wantEnd := time.Date(2026, time.March, 29, 0, 0, 0, 0, loc)
	if got := rule.GetTransferDurationStart(); !got.Equal(wantStart) {
		t.Fatalf("monthly transfer cycle start=%s want %s", got, wantStart)
	}
	if got := rule.GetTransferDurationEnd(); !got.Equal(wantEnd) {
		t.Fatalf("monthly transfer cycle end=%s want %s", got, wantEnd)
	}
}
