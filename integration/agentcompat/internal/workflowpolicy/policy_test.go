package workflowpolicy_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nezhahq/nezha/integration/agentcompat/internal/workflowpolicy"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestPolicy_AcceptsSecureNezhaWorkflow(t *testing.T) {
	// Given
	path := fixturePath(t, "secure-nezha.yml")

	// When
	err := workflowpolicy.VerifyFile(path, workflowpolicy.RepositoryNezha)

	// Then
	require.NoError(t, err)
}

func TestPolicy_AcceptsSecureAgentWorkflow(t *testing.T) {
	// Given
	path := fixturePath(t, "secure-agent.yml")

	// When
	err := workflowpolicy.VerifyFile(path, workflowpolicy.RepositoryAgent)

	// Then
	require.NoError(t, err)
}

func TestPolicy_AcceptsCrossRepositoryCheckoutRefs(t *testing.T) {
	tests := []struct {
		name       string
		fixture    string
		repository workflowpolicy.Repository
	}{
		{name: "default branch", fixture: "cross-repository-default-ref.yml", repository: workflowpolicy.RepositoryNezha},
		{name: "branch", fixture: "secure-agent.yml", repository: workflowpolicy.RepositoryAgent},
		{name: "version tag", fixture: "secure-nezha.yml", repository: workflowpolicy.RepositoryNezha},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := workflowpolicy.VerifyFile(fixturePath(t, test.fixture), test.repository)
			require.NoError(t, err)
		})
	}
}

func TestPolicy_RejectsInvalidCrossRepositoryCheckoutRefs(t *testing.T) {
	tests := []struct {
		name string
		ref  string
	}{
		{name: "dynamic", ref: "${{ inputs.ref }}"},
		{name: "non-string", ref: "false"},
		{name: "empty", ref: "\"\""},
		{name: "whitespace only", ref: "\"  \""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workflow := "on:\n  pull_request:\nconcurrency: policy\npermissions:\n  contents: read\njobs:\n  verify:\n    runs-on: ubuntu-24.04\n    timeout-minutes: 10\n    steps:\n      - uses: actions/checkout@v7.0.1\n        with:\n          repository: nezhahq/agent\n          ref: " + test.ref + "\n          persist-credentials: false\n"
			err := workflowpolicy.Verify([]byte(workflow), workflowpolicy.RepositoryNezha)
			requireTypedPolicyError(t, err, workflowpolicy.RuleRepositoryNotLiteral)
			require.ErrorContains(t, err, "checkout ref must be a nonempty literal")
		})
	}
}

func TestPolicy_RejectsPullRequestTarget(t *testing.T) {
	assertFixtureRejected(t, rejected("pull-request-target.yml", workflowpolicy.RulePrivilegedTrigger, "pull_request_target"))
}

func TestPolicy_RejectsPrivilegedWorkflowRun(t *testing.T) {
	assertFixtureRejected(t, rejected("workflow-run.yml", workflowpolicy.RulePrivilegedTrigger, "workflow_run"))
}

func TestPolicy_RejectsSecretContext(t *testing.T) {
	assertFixtureRejected(t, rejected("secret-context.yml", workflowpolicy.RuleSecretContext, "secrets"))
}

func TestPolicy_RejectsAggregateSecretContext(t *testing.T) {
	assertFixtureRejected(t, rejected("aggregate-secret-context.yml", workflowpolicy.RuleSecretContext, "secrets"))
}

func TestPolicy_RejectsWritePermission(t *testing.T) {
	assertFixtureRejected(t, rejected("write-permission.yml", workflowpolicy.RuleWritePermission, "contents"))
}

func TestPolicy_RejectsIDTokenPermission(t *testing.T) {
	assertFixtureRejected(t, rejected("id-token-write.yml", workflowpolicy.RuleWritePermission, "id-token"))
}

func TestPolicy_RejectsMissingRootPermissions(t *testing.T) {
	assertFixtureRejected(t, rejected("missing-root-permissions.yml", workflowpolicy.RuleWritePermission, "root permissions"))
}

func TestPolicy_RejectsPermissionsSequence(t *testing.T) {
	assertFixtureRejected(t, rejected("permissions-sequence.yml", workflowpolicy.RuleWritePermission, "mapping"))
}

func TestPolicy_RejectsQuotedFalsePersistCredentials(t *testing.T) {
	assertFixtureRejected(t, rejected("quoted-false-security-controls.yml", workflowpolicy.RulePersistCredentials, "boolean"))
}

func TestPolicy_RejectsFractionalTimeout(t *testing.T) {
	assertFixtureRejected(t, rejected("numeric-timeout.yml", workflowpolicy.RuleMissingJobTimeout, "positive literal"))
}

func TestPolicy_RejectsNonScalarPermissionValue(t *testing.T) {
	assertFixtureRejected(t, rejected("mapping-permission-value.yml", workflowpolicy.RuleWritePermission, "read or none"))
}

func TestPolicy_RejectsNonMappingUsesStep(t *testing.T) {
	assertFixtureRejected(t, rejected("nonmapping-uses.yml", workflowpolicy.RuleWorkflowStructure, "string action reference"))
}

func TestPolicy_RejectsNonStringUsesReference(t *testing.T) {
	assertFixtureRejected(t, rejected("boolean-uses.yml", workflowpolicy.RuleWorkflowStructure, "string action reference"))
}

func TestPolicy_RejectsMutableRepositoryInput(t *testing.T) {
	assertFixtureRejected(t, rejected("unapproved-repository.yml", workflowpolicy.RuleRepositoryNotAllowed, "attacker/fork"))
}

func TestPolicy_RejectsNonliteralRepository(t *testing.T) {
	assertFixtureRejected(t, rejected("nonliteral-repository.yml", workflowpolicy.RuleRepositoryNotLiteral, "literal"))
}

func TestPolicy_RejectsMissingPersistCredentialsFalse(t *testing.T) {
	assertFixtureRejected(t, rejected("missing-persist-credentials.yml", workflowpolicy.RulePersistCredentials, "persist-credentials"))
}

func TestPolicy_RejectsPersistCredentialsTrue(t *testing.T) {
	assertFixtureRejected(t, rejected("persist-credentials-true.yml", workflowpolicy.RulePersistCredentials, "false"))
}

func TestPolicy_RejectsMalformedYAML(t *testing.T) {
	// Given
	path := fixturePath(t, "malformed.yml")

	// When
	err := workflowpolicy.VerifyFile(path, workflowpolicy.RepositoryNezha)

	// Then
	var parseError *workflowpolicy.ParseError
	require.ErrorAs(t, err, &parseError)
	require.Contains(t, err.Error(), "parse workflow")
}

func TestPolicy_RejectsDuplicateKeys(t *testing.T) {
	// Given
	path := fixturePath(t, "duplicate-jobs.yml")

	// When
	err := workflowpolicy.VerifyFile(path, workflowpolicy.RepositoryNezha)

	// Then
	var parseError *workflowpolicy.ParseError
	require.ErrorAs(t, err, &parseError)
	require.Contains(t, err.Error(), "duplicate key")
}

func TestPolicy_RejectsYAMLAlias(t *testing.T) {
	// Given
	path := fixturePath(t, "yaml-alias.yml")

	// When
	err := workflowpolicy.VerifyFile(path, workflowpolicy.RepositoryNezha)

	// Then
	var parseError *workflowpolicy.ParseError
	require.ErrorAs(t, err, &parseError)
	require.Contains(t, err.Error(), "aliases are not allowed")
}

func TestPolicy_RejectsTrailingEmptyYAMLDocument(t *testing.T) {
	assertFixtureParseRejected(t, "trailing-empty-document.yml", "multiple YAML documents")
}

func TestPolicy_RejectsBareYAMLAnchor(t *testing.T) {
	assertFixtureParseRejected(t, "bare-anchor.yml", "anchors are not allowed")
}

func TestPolicy_RejectsMalformedStepValues(t *testing.T) {
	for _, fixture := range []string{"empty-step.yml", "empty-run.yml", "nonstring-run.yml", "empty-uses.yml"} {
		t.Run(fixture, func(t *testing.T) {
			assertFixtureRejected(t, rejected(fixture, workflowpolicy.RuleWorkflowStructure, "step"))
		})
	}
}

func TestPolicy_RejectsSecretTokenSources(t *testing.T) {
	for _, fixture := range []string{"github-token.yml", "github-token-environment.yml"} {
		t.Run(fixture, func(t *testing.T) {
			assertFixtureRejected(t, rejected(fixture, workflowpolicy.RuleSecretContext, "token"))
		})
	}
}

func TestPolicy_RejectsNonBooleanConcurrencyCancellation(t *testing.T) {
	assertFixtureRejected(t, rejected("nonboolean-concurrency-cancel.yml", workflowpolicy.RuleMissingConcurrency, "boolean"))
}

func TestPolicy_RejectsMissingJobs(t *testing.T) {
	assertFixtureRejected(t, rejected("missing-jobs.yml", workflowpolicy.RuleWorkflowStructure, "nonempty mapping"))
}

func TestPolicy_RejectsNonmappingStep(t *testing.T) {
	assertFixtureRejected(t, rejected("nonmapping-step.yml", workflowpolicy.RuleWorkflowStructure, "step must be a mapping"))
}

func TestPolicy_RejectsNonsequenceSteps(t *testing.T) {
	assertFixtureRejected(t, rejected("nonsequence-steps.yml", workflowpolicy.RuleWorkflowStructure, "steps must be a sequence"))
}

func TestPolicy_MissingFutureWorkflowReturnsTypedReadError(t *testing.T) {
	// Given
	path := filepath.Join(t.TempDir(), "agent-compatibility.yml")

	// When
	err := workflowpolicy.VerifyFile(path, workflowpolicy.RepositoryNezha)

	// Then
	var readError *workflowpolicy.ReadError
	require.ErrorAs(t, err, &readError)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPolicy_RejectsUnsupportedRepositoryBeforeWorkflowChecks(t *testing.T) {
	// Given
	data, err := os.ReadFile(fixturePath(t, "secure-nezha.yml"))
	require.NoError(t, err)

	// When
	err = workflowpolicy.Verify(data, workflowpolicy.Repository("attacker/fork"))

	// Then
	var policyError *workflowpolicy.PolicyError
	require.ErrorAs(t, err, &policyError)
	require.True(t, policyError.Has(workflowpolicy.RuleRepositoryNotAllowed))
}

type rejectionExpectation struct {
	fixture    string
	repository workflowpolicy.Repository
	rule       workflowpolicy.Rule
	diagnostic string
}

func rejected(fixture string, rule workflowpolicy.Rule, diagnostic string) rejectionExpectation {
	return rejectionExpectation{fixture: fixture, repository: workflowpolicy.RepositoryNezha, rule: rule, diagnostic: diagnostic}
}

func assertFixtureRejected(t *testing.T, expectation rejectionExpectation) {
	t.Helper()

	// Given
	path := fixturePath(t, expectation.fixture)

	// When
	err := workflowpolicy.VerifyFile(path, expectation.repository)

	// Then
	var policyError *workflowpolicy.PolicyError
	require.ErrorAs(t, err, &policyError)
	require.True(t, policyError.Has(expectation.rule), "diagnostic: %v", err)
	require.Contains(t, err.Error(), expectation.diagnostic)
}

func assertFixtureParseRejected(t *testing.T, fixture string, diagnostic string) {
	t.Helper()
	err := workflowpolicy.VerifyFile(fixturePath(t, fixture), workflowpolicy.RepositoryNezha)
	var parseError *workflowpolicy.ParseError
	require.ErrorAs(t, err, &parseError)
	require.Contains(t, err.Error(), diagnostic)
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

func mappingNodeValue(node *yaml.Node, key string) *yaml.Node {
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func scalarValues(node *yaml.Node) []string {
	values := make([]string, 0, len(node.Content))
	for _, child := range node.Content {
		values = append(values, child.Value)
	}
	return values
}

func requireTypedPolicyError(t *testing.T, err error, rule workflowpolicy.Rule) *workflowpolicy.PolicyError {
	t.Helper()
	var policyError *workflowpolicy.PolicyError
	require.True(t, errors.As(err, &policyError), "expected typed policy error, got %T: %v", err, err)
	require.True(t, policyError.Has(rule), "diagnostic: %v", err)
	return policyError
}
