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
