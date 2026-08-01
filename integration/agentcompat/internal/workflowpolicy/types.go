package workflowpolicy

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type Repository string

const (
	RepositoryAgent Repository = "nezhahq/agent"
	RepositoryNezha Repository = "nezhahq/nezha"
)

type Rule string

const (
	RulePrivilegedTrigger      Rule = "privileged-trigger"
	RuleSecretContext          Rule = "secret-context"
	RuleWritePermission        Rule = "write-permission"
	RuleSelfHostedRunner       Rule = "self-hosted-runner"
	RuleContainerizedExecution Rule = "containerized-execution"
	RuleRepositoryNotAllowed   Rule = "repository-not-allowed"
	RuleRepositoryNotLiteral   Rule = "repository-not-literal"
	RulePersistCredentials     Rule = "persist-credentials" // #nosec G101 -- GitHub Actions configuration key, not a credential.
	RuleReusableExecutable     Rule = "reusable-executable"
	RuleContinueOnError        Rule = "continue-on-error"
	RuleSwallowedFailure       Rule = "swallowed-failure"
	RuleMissingJobTimeout      Rule = "missing-job-timeout"
	RuleMissingConcurrency     Rule = "missing-concurrency"
	RuleArtifactRedaction      Rule = "artifact-redaction"
	RuleUntrustedExpression    Rule = "untrusted-expression"
	RuleWorkflowStructure      Rule = "workflow-structure"
)

type Violation struct {
	Rule   Rule
	Path   string
	Line   int
	Column int
	Detail string
}

type violationLocation struct {
	path string
	node *yaml.Node
}

func at(path string, node *yaml.Node) violationLocation {
	return violationLocation{path: path, node: node}
}

func (v Violation) String() string {
	location := v.Path
	if v.Line > 0 {
		location = fmt.Sprintf("%s:%d:%d", v.Path, v.Line, v.Column)
	}
	return fmt.Sprintf("%s at %s: %s", v.Rule, location, v.Detail)
}

type PolicyError struct {
	violations []Violation
}

func (e *PolicyError) Error() string {
	lines := make([]string, 0, len(e.violations)+1)
	lines = append(lines, "workflow policy rejected")
	for _, violation := range e.violations {
		lines = append(lines, "- "+violation.String())
	}
	return strings.Join(lines, "\n")
}

func (e *PolicyError) Has(rule Rule) bool {
	for _, violation := range e.violations {
		if violation.Rule == rule {
			return true
		}
	}
	return false
}

func (e *PolicyError) Violations() []Violation {
	return append([]Violation(nil), e.violations...)
}

type ParseError struct {
	Source string
	Cause  error
}

type ReadError struct {
	Path  string
	Cause error
}

func (e *ReadError) Error() string {
	return fmt.Sprintf("read workflow %q: %v", e.Path, e.Cause)
}

func (e *ReadError) Unwrap() error {
	return e.Cause
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parse workflow %q: %v", e.Source, e.Cause)
}

func (e *ParseError) Unwrap() error {
	return e.Cause
}
