package workflowpolicy_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nezhahq/nezha/integration/agentcompat/internal/workflowpolicy"
	"github.com/stretchr/testify/require"
)

func TestPolicy_RejectsSelfHostedRunner(t *testing.T) {
	assertFixtureRejected(t, rejected("self-hosted.yml", workflowpolicy.RuleSelfHostedRunner, "self-hosted"))
}

func TestPolicy_RejectsMatrixSelfHostedRunner(t *testing.T) {
	assertFixtureRejected(t, rejected("matrix-self-hosted.yml", workflowpolicy.RuleSelfHostedRunner, "self-hosted"))
}

func TestPolicy_RejectsCustomRunnerLabel(t *testing.T) {
	assertFixtureRejected(t, rejected("custom-runner.yml", workflowpolicy.RuleSelfHostedRunner, "GitHub-hosted"))
}

func TestPolicy_RejectsMatrixIncludeRunner(t *testing.T) {
	assertFixtureRejected(t, rejected("matrix-include-runner.yml", workflowpolicy.RuleSelfHostedRunner, "include"))
}

func TestPolicy_RejectsComposedCustomRunnerLabel(t *testing.T) {
	assertFixtureRejected(t, rejected("composed-custom-runner.yml", workflowpolicy.RuleSelfHostedRunner, "GitHub-hosted"))
}

func TestPolicy_RejectsDockerExecution(t *testing.T) {
	assertFixtureRejected(t, rejected("docker.yml", workflowpolicy.RuleContainerizedExecution, "docker"))
}

func TestPolicy_RejectsAbsoluteDockerExecution(t *testing.T) {
	assertFixtureRejected(t, rejected("absolute-docker.yml", workflowpolicy.RuleContainerizedExecution, "docker"))
}

func TestPolicy_RejectsAlternateAbsoluteDockerExecution(t *testing.T) {
	assertFixtureRejected(t, rejected("alternate-absolute-docker.yml", workflowpolicy.RuleContainerizedExecution, "docker"))
}

func TestPolicy_RejectsJobContainer(t *testing.T) {
	assertFixtureRejected(t, rejected("container.yml", workflowpolicy.RuleContainerizedExecution, "container"))
}

func TestPolicy_RejectsServiceContainers(t *testing.T) {
	assertFixtureRejected(t, rejected("services.yml", workflowpolicy.RuleContainerizedExecution, "services"))
}

func TestPolicy_RejectsCacheReuse(t *testing.T) {
	assertFixtureRejected(t, rejected("cache.yml", workflowpolicy.RuleReusableExecutable, "cache"))
}

func TestPolicy_RejectsSetupGoDefaultCache(t *testing.T) {
	assertFixtureRejected(t, rejected("setup-go-default-cache.yml", workflowpolicy.RuleReusableExecutable, "cache: false"))
}

func TestPolicy_RejectsArtifactExecutableReuse(t *testing.T) {
	assertFixtureRejected(t, rejected("download-artifact.yml", workflowpolicy.RuleReusableExecutable, "artifact reuse"))
}

func TestPolicy_RejectsWorkspaceExecutableReuse(t *testing.T) {
	assertFixtureRejected(t, rejected("workspace-executable.yml", workflowpolicy.RuleReusableExecutable, "workspace"))
}

func TestPolicy_RejectsLocalActionReuse(t *testing.T) {
	assertFixtureRejected(t, rejected("local-action.yml", workflowpolicy.RuleReusableExecutable, "local action"))
}

func TestPolicy_RejectsUnapprovedAction(t *testing.T) {
	assertFixtureRejected(t, rejected("unapproved-action.yml", workflowpolicy.RuleRepositoryNotAllowed, "action"))
}

func TestPolicy_RejectsActionReferenceWithoutRef(t *testing.T) {
	assertFixtureRejected(t, rejected("action-reference-missing-ref.yml", workflowpolicy.RuleWorkflowStructure, "owner/repository@ref"))
}

func TestPolicy_RejectsMalformedActionReferences(t *testing.T) {
	tests := []struct {
		name       string
		uses       string
		rule       workflowpolicy.Rule
		diagnostic string
	}{
		{name: "empty ref", uses: "actions/checkout@", rule: workflowpolicy.RuleWorkflowStructure, diagnostic: "owner/repository@ref"},
		{name: "duplicate separator", uses: "actions/checkout@v7@unexpected", rule: workflowpolicy.RuleWorkflowStructure, diagnostic: "owner/repository@ref"},
		{name: "extra action path", uses: "actions/checkout/extra@v7", rule: workflowpolicy.RuleWorkflowStructure, diagnostic: "owner/repository@ref"},
		{name: "whitespace", uses: "actions/checkout @v7", rule: workflowpolicy.RuleWorkflowStructure, diagnostic: "owner/repository@ref"},
		{name: "dynamic ref", uses: "actions/checkout@${{ inputs.ref }}", rule: workflowpolicy.RuleRepositoryNotLiteral, diagnostic: "action reference must be literal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workflow := "on:\n  pull_request:\nconcurrency: policy\npermissions:\n  contents: read\njobs:\n  verify:\n    runs-on: ubuntu-24.04\n    timeout-minutes: 10\n    steps:\n      - uses: \"" + test.uses + "\"\n        with:\n          persist-credentials: false\n"
			err := workflowpolicy.Verify([]byte(workflow), workflowpolicy.RepositoryNezha)
			requireTypedPolicyError(t, err, test.rule)
			require.ErrorContains(t, err, test.diagnostic)
		})
	}
}

func TestPolicy_RejectsReusableWorkflowJob(t *testing.T) {
	assertFixtureRejected(t, rejected("reusable-workflow-job.yml", workflowpolicy.RuleReusableExecutable, "reusable workflow"))
}

func TestPolicy_RejectsContinueOnError(t *testing.T) {
	assertFixtureRejected(t, rejected("continue-on-error.yml", workflowpolicy.RuleContinueOnError, "continue-on-error"))
}

func TestPolicy_RejectsSwallowedShellFailure(t *testing.T) {
	assertFixtureRejected(t, rejected("swallowed-failure.yml", workflowpolicy.RuleSwallowedFailure, "|| true"))
}

func TestPolicy_RejectsAlternativeSwallowedShellFailures(t *testing.T) {
	for _, fixture := range []string{"or-echo-failure.yml", "or-printf-failure.yml", "or-exit-zero-failure.yml", "set-plus-o-errexit.yml", "set-plus-e-semicolon.yml", "if-not-failure.yml", "if-condition-failure.yml", "and-if-condition-failure.yml", "nested-shell.yml"} {
		t.Run(fixture, func(t *testing.T) {
			assertFixtureRejected(t, rejected(fixture, workflowpolicy.RuleSwallowedFailure, "failure"))
		})
	}
}

func TestPolicy_RejectsMissingJobTimeout(t *testing.T) {
	assertFixtureRejected(t, rejected("missing-timeout.yml", workflowpolicy.RuleMissingJobTimeout, "timeout-minutes"))
}

func TestPolicy_RejectsMissingConcurrency(t *testing.T) {
	assertFixtureRejected(t, rejected("missing-concurrency.yml", workflowpolicy.RuleMissingConcurrency, "concurrency"))
}

func TestPolicy_RejectsEmptyConcurrency(t *testing.T) {
	assertFixtureRejected(t, rejected("empty-concurrency.yml", workflowpolicy.RuleMissingConcurrency, "concurrency"))
}

func TestPolicy_RejectsArtifactWithoutRedaction(t *testing.T) {
	assertFixtureRejected(t, rejected("artifact-without-redaction.yml", workflowpolicy.RuleArtifactRedaction, "redaction step"))
}

func TestPolicy_RejectsUnredactedArtifactPath(t *testing.T) {
	assertFixtureRejected(t, rejected("unredacted-artifact-path.yml", workflowpolicy.RuleArtifactRedaction, "redacted"))
}

func TestPolicy_RejectsNoOpRedactionStep(t *testing.T) {
	assertFixtureRejected(t, rejected("no-op-redaction.yml", workflowpolicy.RuleArtifactRedaction, "redaction step"))
}

func TestPolicy_RejectsConditionalRedaction(t *testing.T) {
	assertFixtureRejected(t, rejected("conditional-redaction.yml", workflowpolicy.RuleArtifactRedaction, "always()"))
}

func TestPolicy_RejectsRawWriteAfterRedaction(t *testing.T) {
	assertFixtureRejected(t, rejected("raw-after-redaction.yml", workflowpolicy.RuleArtifactRedaction, "immediately follow"))
}

func TestPolicy_RejectsCommandsAppendedToRedaction(t *testing.T) {
	assertFixtureRejected(t, rejected("redaction-command-append.yml", workflowpolicy.RuleArtifactRedaction, "immediately follow"))
}

func TestPolicy_RejectsUntrustedRunExpression(t *testing.T) {
	assertFixtureRejected(t, rejected("untrusted-run-expression.yml", workflowpolicy.RuleUntrustedExpression, "pull_request.title"))
}

func TestPolicy_RejectsDynamicGitRepository(t *testing.T) {
	assertFixtureRejected(t, rejected("dynamic-git-repository.yml", workflowpolicy.RuleRepositoryNotLiteral, "literal"))
}

func TestPolicy_RejectsUnapprovedGitRepository(t *testing.T) {
	assertFixtureRejected(t, rejected("unapproved-git-repository.yml", workflowpolicy.RuleRepositoryNotAllowed, "attacker/fork"))
}

func TestPolicy_RejectsEnvironmentGitRepository(t *testing.T) {
	assertFixtureRejected(t, rejected("environment-git-repository.yml", workflowpolicy.RuleRepositoryNotLiteral, "literal"))
}

func TestPolicy_RejectsExternalGitRepository(t *testing.T) {
	assertFixtureRejected(t, rejected("external-git-repository.yml", workflowpolicy.RuleRepositoryNotAllowed, "repository"))
}

func TestPolicy_RejectsPrefixedGitRepository(t *testing.T) {
	assertFixtureRejected(t, rejected("prefixed-git-repository.yml", workflowpolicy.RuleRepositoryNotLiteral, "literal"))
}

func TestPolicy_RejectsGitGlobalOptionRepository(t *testing.T) {
	assertFixtureRejected(t, rejected("git-global-option-repository.yml", workflowpolicy.RuleRepositoryNotAllowed, "repository"))
}

func TestPolicy_RejectsCommandOptionGitRepository(t *testing.T) {
	assertFixtureRejected(t, rejected("command-option-git-repository.yml", workflowpolicy.RuleRepositoryNotAllowed, "repository"))
}

func TestPolicy_RejectsGitConfigurationEnvironment(t *testing.T) {
	assertFixtureRejected(t, rejected("git-config-environment.yml", workflowpolicy.RuleRepositoryNotLiteral, "GIT_CONFIG_COUNT"))
}

func TestPolicy_RejectsGitHubEnvironmentConfiguration(t *testing.T) {
	assertFixtureRejected(t, rejected("github-environment-git-config.yml", workflowpolicy.RuleRepositoryNotLiteral, "GITHUB_ENV"))
}

func TestPolicy_RejectsIndexedUntrustedExpression(t *testing.T) {
	assertFixtureRejected(t, rejected("indexed-untrusted-expression.yml", workflowpolicy.RuleUntrustedExpression, "github['event']"))
}

func TestPolicy_RejectsDynamicIndexedUntrustedExpression(t *testing.T) {
	assertFixtureRejected(t, rejected("dynamic-indexed-untrusted-expression.yml", workflowpolicy.RuleUntrustedExpression, "github["))
}

func TestPolicy_RejectsNonliteralAction(t *testing.T) {
	assertFixtureRejected(t, rejected("nonliteral-action.yml", workflowpolicy.RuleRepositoryNotLiteral, "literal"))
}

func TestPolicy_RejectsMixedArtifactPaths(t *testing.T) {
	assertFixtureRejected(t, rejected("mixed-artifact-paths.yml", workflowpolicy.RuleArtifactRedaction, "redacted"))
}

func TestPolicy_VerifyFileUsesFreshContents(t *testing.T) {
	// Given
	temporaryDirectory := t.TempDir()
	workflowPath := filepath.Join(temporaryDirectory, "workflow.yml")
	secureWorkflow, err := os.ReadFile(fixturePath(t, "secure-nezha.yml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(workflowPath, secureWorkflow, 0o600))
	require.NoError(t, workflowpolicy.VerifyFile(workflowPath, workflowpolicy.RepositoryNezha))
	maliciousWorkflow, err := os.ReadFile(fixturePath(t, "continue-on-error.yml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(workflowPath, maliciousWorkflow, 0o600))

	// When
	err = workflowpolicy.VerifyFile(workflowPath, workflowpolicy.RepositoryNezha)

	// Then
	requireTypedPolicyError(t, err, workflowpolicy.RuleContinueOnError)
}

func TestPolicy_TempWorkflowsReportExactDiagnostics(t *testing.T) {
	// Given
	temporaryDirectory := t.TempDir()
	securePath := filepath.Join(temporaryDirectory, "secure.yml")
	maliciousPath := filepath.Join(temporaryDirectory, "malicious.yml")
	secureWorkflow, err := os.ReadFile(fixturePath(t, "secure-nezha.yml"))
	require.NoError(t, err)
	maliciousWorkflow, err := os.ReadFile(fixturePath(t, "persist-credentials-true.yml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(securePath, secureWorkflow, 0o600))
	require.NoError(t, os.WriteFile(maliciousPath, maliciousWorkflow, 0o600))

	// When
	secureError := workflowpolicy.VerifyFile(securePath, workflowpolicy.RepositoryNezha)
	maliciousError := workflowpolicy.VerifyFile(maliciousPath, workflowpolicy.RepositoryNezha)

	// Then
	require.NoError(t, secureError)
	requireTypedPolicyError(t, maliciousError, workflowpolicy.RulePersistCredentials)
	t.Logf("secure workflow: PASS")
	t.Logf("malicious workflow: %v", maliciousError)
}
