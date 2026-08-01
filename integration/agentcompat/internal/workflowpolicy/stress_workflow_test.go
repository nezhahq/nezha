package workflowpolicy_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const agentcompatStressTestName = "TestStressPRFullEightAgentExactlyOnce"

func TestPolicy_NezhaStressWorkflowRunsPinnedCrossRepositoryTest(t *testing.T) {
	// Given
	data := readNezhaQualityWorkflow(t)
	var workflow qualityWorkflow
	require.NoError(t, yaml.Unmarshal(data, &workflow))

	// When
	stressJob, exists := workflow.Jobs["agentcompat-stress"]

	// Then
	require.True(t, exists)
	require.Equal(t, "Linux agent compatibility stress", stressJob.Name)
	require.Equal(t, "ubuntu-24.04", stressJob.RunsOn)
	require.Equal(t, 75, stressJob.TimeoutMinutes)
	require.Len(t, stressJob.Steps, 6)
	require.Equal(t, []string{
		"Checkout Nezha revision",
		"Checkout pinned Agent revision",
		"Set up Go",
		"Prepare Dashboard build inputs",
		"Require named stress test",
		"Run PR-full agent compatibility stress",
	}, []string{
		stressJob.Steps[0].Name,
		stressJob.Steps[1].Name,
		stressJob.Steps[2].Name,
		stressJob.Steps[3].Name,
		stressJob.Steps[4].Name,
		stressJob.Steps[5].Name,
	})

	nezhaCheckout := stressJob.stepNamed(t, "Checkout Nezha revision")
	require.Equal(t, "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", nezhaCheckout.Uses)
	require.Empty(t, nezhaCheckout.With.Repository)
	require.Empty(t, nezhaCheckout.With.Ref)
	require.Equal(t, "nezha", nezhaCheckout.With.Path)
	require.False(t, *nezhaCheckout.With.PersistCredentials)

	agentCheckout := stressJob.stepNamed(t, "Checkout pinned Agent revision")
	require.Equal(t, "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", agentCheckout.Uses)
	require.Equal(t, "nezhahq/agent", agentCheckout.With.Repository)
	require.Equal(t, "667e1dd5e166ffef808ec26dc20de85bc33a0a0f", agentCheckout.With.Ref)
	require.Equal(t, "agent", agentCheckout.With.Path)
	require.False(t, *agentCheckout.With.PersistCredentials)

	setupGo := stressJob.stepNamed(t, "Set up Go")
	require.Equal(t, "actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e", setupGo.Uses)
	require.Equal(t, "1.26.x", setupGo.With.GoVersion)
	require.False(t, *setupGo.With.Cache)

	prepareDashboardInputs := stressJob.stepNamed(t, "Prepare Dashboard build inputs")
	require.Equal(t, "nezha", prepareDashboardInputs.WorkingDirectory)
	require.Equal(t, strings.Join([]string{
		"go install github.com/swaggo/swag/cmd/swag@v1.16.6",
		"mkdir -p cmd/dashboard/user-dist cmd/dashboard/admin-dist",
		"printf 'placeholder\\n' > cmd/dashboard/user-dist/placeholder.txt",
		"printf 'placeholder\\n' > cmd/dashboard/admin-dist/placeholder.txt",
		"swag init --pd -d cmd/dashboard -g main.go -o cmd/dashboard/docs",
	}, "\n"), strings.TrimSpace(prepareDashboardInputs.Run))

	listStep := stressJob.stepNamed(t, "Require named stress test")
	require.Equal(t, "nezha", listStep.WorkingDirectory)
	require.Equal(t, "go test -mod=readonly -tags=agentcompat -list '^"+agentcompatStressTestName+"$' ./integration/agentcompat/internal/scenario | grep -Fx '"+agentcompatStressTestName+"'", listStep.Run)

	runStep := stressJob.stepNamed(t, "Run PR-full agent compatibility stress")
	require.Equal(t, "nezha", runStep.WorkingDirectory)
	require.Equal(t, "${{ github.workspace }}/nezha", runStep.Env.AgentcompatNezhaSource)
	require.Equal(t, "${{ github.workspace }}/agent", runStep.Env.AgentcompatAgentSource)
	require.Equal(t, "go test -mod=readonly -tags=agentcompat -run '^"+agentcompatStressTestName+"$' -count=1 -v ./integration/agentcompat/internal/scenario", runStep.Run)
}
