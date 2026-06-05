package model

import (
	"slices"
	"testing"
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
