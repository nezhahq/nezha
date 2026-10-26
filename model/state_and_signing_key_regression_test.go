package model

import (
	"github.com/goccy/go-json"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/stretchr/testify/require"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestSecurityBoundaryNonfiniteState(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		state := PB2State(&pb.State{Cpu: value, Load1: value, Load5: value, Load15: value})
		require.Zero(t, state.CPU)
		require.Zero(t, state.Load1)
		require.Zero(t, state.Load5)
		require.Zero(t, state.Load15)
		_, err := json.Marshal(state)
		require.NoError(t, err)
	}
	_, err := json.Marshal(PB2State(&pb.State{Cpu: 25}))
	require.NoError(t, err)
}
func TestConfigSavePreservesFileSigningKey(t *testing.T) {
	t.Setenv(JWTSecretEnvKey, "")
	path := filepath.Join(t.TempDir(), "dashboard-config.yaml")
	key := "synthetic-stable-dashboard-signing-key"
	require.NoError(t, os.WriteFile(path, []byte("jwt_secret_key: "+key+"\n"), 0600))
	var config Config
	require.NoError(t, config.Read(path, nil))
	require.Equal(t, key, config.JWTSecretKey)
	require.NoError(t, config.Save())
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(contents), key)
	var restarted Config
	require.NoError(t, restarted.Read(path, nil))
	require.Equal(t, key, restarted.JWTSecretKey)
}

func TestConfigSaveDoesNotExposeEnvironmentSigningKey(t *testing.T) {
	key := "synthetic-environment-signing-key"
	t.Setenv(JWTSecretEnvKey, key)
	path := filepath.Join(t.TempDir(), "dashboard-config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("listen_port: 8008\n"), 0600))
	var config Config
	require.NoError(t, config.Read(path, nil))
	require.Equal(t, key, config.JWTSecretKey)
	require.NoError(t, config.Save())
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(contents), key)
	public, err := json.Marshal(&config)
	require.NoError(t, err)
	require.NotContains(t, string(public), key)
	require.NotContains(t, string(public), "jwt_secret_key")
}
