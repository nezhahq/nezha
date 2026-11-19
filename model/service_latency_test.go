package model

import (
	"math"
	"testing"

	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"
)

func TestServicePersistenceRejectsNonFiniteLatency(t *testing.T) {
	// go_json builds bind requests with goccy/go-json, which narrows a number
	// beyond the float32 range to an infinity instead of refusing it.
	var form ServiceForm
	require.NoError(t, json.Unmarshal([]byte(`{"min_latency":-1e39,"max_latency":1e39}`), &form))
	require.True(t, math.IsInf(float64(form.MinLatency), -1))
	require.True(t, math.IsInf(float64(form.MaxLatency), 1))
	require.Error(t, ValidateServiceLatency(form.MinLatency, form.MaxLatency))

	for _, latency := range []float32{float32(math.Inf(1)), float32(math.Inf(-1)), float32(math.NaN())} {
		require.Error(t, (&Service{Type: TaskTypeHTTPGet, MinLatency: latency}).BeforeSave(nil), "min_latency %v must not be persisted", latency)
		require.Error(t, (&Service{Type: TaskTypeHTTPGet, MaxLatency: latency}).BeforeSave(nil), "max_latency %v must not be persisted", latency)
	}

	require.NoError(t, (&Service{Type: TaskTypeHTTPGet, MinLatency: 1, MaxLatency: math.MaxFloat32}).BeforeSave(nil))
}
