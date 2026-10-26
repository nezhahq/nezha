package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAlertDurationRejectsUnboundedHistory(t *testing.T) {
	_, ok := (&Rule{Duration: 86401}).DurationInt()
	require.False(t, ok)
	duration, ok := (&Rule{Duration: 86400}).DurationInt()
	require.True(t, ok)
	require.Equal(t, 86400, duration)
}

func TestCalendarCycleBoundsPreserveSequentialMonthNormalization(t *testing.T) {
	starts := []time.Time{time.Date(2000, 1, 31, 0, 0, 0, 0, time.UTC), time.Date(2000, 2, 29, 0, 0, 0, 0, time.UTC), time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)}
	now := time.Date(2025, 9, 13, 12, 0, 0, 0, time.UTC)
	for _, start := range starts {
		for _, unit := range []string{"day", "week", "month", "year"} {
			rule := Rule{Type: "transfer_in_cycle", CycleStart: &start, CycleInterval: 1, CycleUnit: unit}
			step := func(at time.Time) time.Time {
				switch unit {
				case "day":
					return at.AddDate(0, 0, 1)
				case "week":
					return at.AddDate(0, 0, 7)
				case "month":
					return at.AddDate(0, 1, 0)
				default:
					return at.AddDate(1, 0, 0)
				}
			}
			expectedStart, expectedEnd := start, step(start)
			for !now.Before(expectedEnd) {
				expectedStart, expectedEnd = expectedEnd, step(expectedEnd)
			}
			actualStart, actualEnd := rule.transferDurationBounds(now)
			require.Equal(t, expectedStart, actualStart, "%s starting %s", unit, start)
			require.Equal(t, expectedEnd, actualEnd)
		}
	}
}

func TestCalendarCycleDoesNotMoveFutureStartBackwards(t *testing.T) {
	start := time.Date(2020, 1, 15, 12, 0, 0, 0, time.UTC)
	now := start.Add(-24 * time.Hour)
	for _, unit := range []string{"day", "week", "month", "year"} {
		rule := Rule{Type: "transfer_in_cycle", CycleStart: &start, CycleInterval: 1, CycleUnit: unit}
		actual, end := rule.transferDurationBounds(now)
		require.Equal(t, start, actual, unit)
		require.True(t, end.After(start), unit)
	}
}
