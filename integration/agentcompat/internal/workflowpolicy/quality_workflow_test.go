package workflowpolicy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nezhahq/nezha/integration/agentcompat/internal/workflowpolicy"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestPolicy_NezhaQualityWorkflow(t *testing.T) {
	// Given
	path := filepath.Join(repositoryRoot(t), ".github", "workflows", "test.yml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, workflowpolicy.Verify(data, workflowpolicy.RepositoryNezha))
	var document yaml.Node
	require.NoError(t, yaml.Unmarshal(data, &document))
	root := document.Content[0]
	var workflow qualityWorkflow
	require.NoError(t, yaml.Unmarshal(data, &workflow))

	// When
	jobs := mappingNodeValue(root, "jobs")
	aggregator := mappingNodeValue(jobs, "nezha-quality-required")
	needs := mappingNodeValue(aggregator, "needs")

	// Then
	triggers := mappingNodeValue(root, "on")
	require.NotNil(t, mappingNodeValue(triggers, "merge_group"))
	require.Equal(t, []string{"master"}, workflow.Triggers.Push.Branches)
	require.Empty(t, workflow.Triggers.Push.Paths)
	require.Equal(t, []string{"master"}, workflow.Triggers.PullRequest.Branches)
	require.Empty(t, workflow.Triggers.PullRequest.Paths)
	require.Equal(t, map[string]string{"contents": "read"}, workflow.Permissions)
	require.NotEmpty(t, workflow.Concurrency.Group)
	require.NotNil(t, workflow.Concurrency.CancelInProgress)
	require.True(t, *workflow.Concurrency.CancelInProgress)
	require.Len(t, workflow.Jobs, 4)

	ordinaryJob := workflow.Jobs["tests"]
	require.Equal(t, []string{"ubuntu-latest", "windows-latest", "macos-latest"}, ordinaryJob.Strategy.Matrix.OS)
	require.NotNil(t, ordinaryJob.Strategy.FailFast)
	require.False(t, *ordinaryJob.Strategy.FailFast)
	require.Equal(t, "${{ matrix.os }}", ordinaryJob.RunsOn)
	require.Equal(t, 30, ordinaryJob.TimeoutMinutes)
	requireCheckoutAndSetupGo(t, ordinaryJob.Steps)
	require.Equal(t, strings.Join([]string{
		"go install github.com/swaggo/swag/cmd/swag@v1.16.6",
		"touch ./cmd/dashboard/user-dist/a",
		"touch ./cmd/dashboard/admin-dist/a",
		"swag init --pd -d cmd/dashboard -g main.go -o cmd/dashboard/docs",
	}, "\n"), strings.TrimSpace(ordinaryJob.stepNamed(t, "Generate Swagger docs").Run))
	require.Equal(t, "go test -mod=readonly -count=1 ./...", ordinaryJob.stepNamed(t, "Unit test").Run)
	require.Equal(t, "go build -v ./cmd/dashboard", ordinaryJob.stepNamed(t, "Build dashboard").Run)

	linuxJob := workflow.Jobs["linux-race-quality"]
	require.Equal(t, "ubuntu-24.04", linuxJob.RunsOn)
	require.Equal(t, 45, linuxJob.TimeoutMinutes)
	requireCheckoutAndSetupGo(t, linuxJob.Steps)
	require.Equal(t, ordinaryJob.stepNamed(t, "Generate Swagger docs").Run, linuxJob.stepNamed(t, "Generate Swagger docs").Run)
	require.Equal(t, "go test -mod=readonly -race -shuffle=on -count=1 ./...", linuxJob.stepNamed(t, "Race and shuffle tests").Run)
	require.Equal(t, "go vet ./...", linuxJob.stepNamed(t, "Vet").Run)
	require.Equal(t, "test -z \"$(git ls-files -co --exclude-standard '*.go' -z | xargs -0 gofmt -l)\"", linuxJob.stepNamed(t, "Check formatting").Run)
	require.Equal(t, "go build ./cmd/dashboard", linuxJob.stepNamed(t, "Build dashboard").Run)
	gosecStep := linuxJob.stepNamed(t, "Run Gosec Security Scanner")
	require.Equal(t, "auto", gosecStep.Env.GoToolchain)
	require.Equal(t, strings.Join([]string{
		"go install github.com/securego/gosec/v2/cmd/gosec@v2.27.1",
		"gosec --exclude=G104,G115,G117,G203,G402,G703,G704 ./...",
	}, "\n"), strings.TrimSpace(gosecStep.Run))

	require.Len(t, scalarValues(needs), 3)
	require.ElementsMatch(t, []string{"tests", "linux-race-quality", "agentcompat-stress"}, scalarValues(needs))
	require.Equal(t, "nezha-quality-required", workflow.Jobs["nezha-quality-required"].Name)
	require.Equal(t, "${{ always() }}", mappingNodeValue(aggregator, "if").Value)
	steps := mappingNodeValue(aggregator, "steps")
	require.Len(t, steps.Content, 1)
	require.Equal(t, strings.Join([]string{
		"test \"${{ needs.tests.result }}\" = success",
		"test \"${{ needs.linux-race-quality.result }}\" = success",
		"test \"${{ needs.agentcompat-stress.result }}\" = success",
	}, "\n"), strings.TrimSpace(mappingNodeValue(steps.Content[0], "run").Value))
}

type qualityWorkflow struct {
	Triggers    qualityTriggers       `yaml:"on"`
	Permissions map[string]string     `yaml:"permissions"`
	Concurrency qualityConcurrency    `yaml:"concurrency"`
	Jobs        map[string]qualityJob `yaml:"jobs"`
}

type qualityTriggers struct {
	Push        qualityBranchTrigger `yaml:"push"`
	PullRequest qualityBranchTrigger `yaml:"pull_request"`
}

type qualityBranchTrigger struct {
	Branches []string `yaml:"branches"`
	Paths    []string `yaml:"paths"`
}

type qualityConcurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress *bool  `yaml:"cancel-in-progress"`
}

type qualityJob struct {
	Name           string          `yaml:"name"`
	Strategy       qualityStrategy `yaml:"strategy"`
	RunsOn         string          `yaml:"runs-on"`
	TimeoutMinutes int             `yaml:"timeout-minutes"`
	Steps          []qualityStep   `yaml:"steps"`
}

type qualityStrategy struct {
	FailFast *bool         `yaml:"fail-fast"`
	Matrix   qualityMatrix `yaml:"matrix"`
}

type qualityMatrix struct {
	OS []string `yaml:"os"`
}

type qualityStep struct {
	Name             string      `yaml:"name"`
	Uses             string      `yaml:"uses"`
	Run              string      `yaml:"run"`
	WorkingDirectory string      `yaml:"working-directory"`
	Env              qualityEnv  `yaml:"env"`
	With             qualityWith `yaml:"with"`
}

type qualityEnv struct {
	GoToolchain            string `yaml:"GOTOOLCHAIN"`
	AgentcompatNezhaSource string `yaml:"AGENTCOMPAT_NEZHA_SOURCE"`
	AgentcompatAgentSource string `yaml:"AGENTCOMPAT_AGENT_SOURCE"`
}

type qualityWith struct {
	PersistCredentials *bool  `yaml:"persist-credentials"`
	GoVersion          string `yaml:"go-version"`
	Cache              *bool  `yaml:"cache"`
	Repository         string `yaml:"repository"`
	Ref                string `yaml:"ref"`
	Path               string `yaml:"path"`
}

func (j qualityJob) stepNamed(t *testing.T, name string) qualityStep {
	t.Helper()
	for _, step := range j.Steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("workflow job is missing step %q", name)
	return qualityStep{}
}

func requireCheckoutAndSetupGo(t *testing.T, steps []qualityStep) {
	t.Helper()
	require.GreaterOrEqual(t, len(steps), 2)
	checkout := steps[0]
	require.Equal(t, "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", checkout.Uses)
	require.NotNil(t, checkout.With.PersistCredentials)
	require.False(t, *checkout.With.PersistCredentials)
	setupGo := steps[1]
	require.Equal(t, "actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e", setupGo.Uses)
	require.Equal(t, "1.26.x", setupGo.With.GoVersion)
	require.NotNil(t, setupGo.With.Cache)
	require.False(t, *setupGo.With.Cache)
}

func TestPolicy_AcceptsWorkflowWithoutTestsOrRequiredAggregator(t *testing.T) {
	// Given
	data, err := os.ReadFile(fixturePath(t, "quality-only.yml"))
	require.NoError(t, err)

	// When
	err = workflowpolicy.Verify(data, workflowpolicy.RepositoryNezha)

	// Then
	require.NoError(t, err)
}

func readNezhaQualityWorkflow(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github", "workflows", "test.yml"))
	require.NoError(t, err)
	return data
}
