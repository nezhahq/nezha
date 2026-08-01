package workflowpolicy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	dockerCommandPattern     = regexp.MustCompile(`(?mi)(?:^|[;&|]\s*|\s)(?:(?:sudo|env)\s+)?(?:/[^\s]+/)?(?:docker|podman|nerdctl|containerd|buildah|runc|crictl)(?:\s|$)`)
	gitHubEnvironmentPattern = regexp.MustCompile(`(?is)GIT_[A-Za-z0-9_]*.*GITHUB_ENV|GITHUB_ENV.*GIT_[A-Za-z0-9_]*`)
	swallowedFailurePattern  = regexp.MustCompile(`(?mi)(?:\|\|\s*(?:true|:|echo\b|printf\b|exit\s+0\b))|(?:^|[;&]\s*)set\s+\+(?:e|o\s+errexit)(?:\s|;|$)|(?:^|[;&]\s*)if\s+|(?:\b(?:bash|sh)\s+-c\b)|(?:^|[;&]\s*)trap\b[^\n]*\bexit\s+0\b|(?:[;&]\s*)(?:true|:)\s*(?:;|$)`)
	workspaceCommandPattern  = regexp.MustCompile(`(?mi)(?:\$\{\{\s*github\.workspace\s*\}\}|\$GITHUB_WORKSPACE|\$\{GITHUB_WORKSPACE\})(?:/|\\)|(?:^|[;&|]\s*)(?:sudo\s+)?(?:\.\.?/|[A-Za-z0-9_.-]+/)[^\s;&|]+`)
	gitRepositoryCommand     = regexp.MustCompile(`(?m)(?:^|[;&|]\s*|\s)(?:(?:sudo|command|env)\s+)?(?:/usr/bin/)?git\b[^\n]*(?:clone|ls-remote)\b`)
	gitConfigurationPattern  = regexp.MustCompile(`(?mi)(?:^|[;&|]\s*)(?:sudo\s+)?git(?:\s+-c\s+url\.[^\s]+\.insteadOf=\S+|\s+config\b)`)
)

func (c *checker) checkJob(name string, job *yaml.Node) {
	path := "$.jobs." + name
	timeout, exists := mappingValue(job, "timeout-minutes")
	if !exists || !positiveInteger(timeout) {
		node := job
		if exists {
			node = timeout
		}
		c.reject(RuleMissingJobTimeout, at(path+".timeout-minutes", node), "job timeout-minutes must be a positive literal")
	}
	c.checkRunner(path, job)
	if container, exists := mappingValue(job, "container"); exists {
		c.reject(RuleContainerizedExecution, at(path+".container", container), "job containers are forbidden")
	}
	if services, exists := mappingValue(job, "services"); exists {
		c.reject(RuleContainerizedExecution, at(path+".services", services), "service containers are forbidden")
	}
	c.checkPermissions(job, path+".permissions", false)
	c.checkContinueOnError(job, path)
	if reusableWorkflow, exists := mappingValue(job, "uses"); exists {
		c.reject(RuleReusableExecutable, at(path+".uses", reusableWorkflow), "job-level reusable workflows are forbidden")
		return
	}
	steps, exists := mappingValue(job, "steps")
	if !exists {
		c.reject(RuleWorkflowStructure, at(path+".steps", job), "workflow jobs must define steps")
		return
	}
	if steps.Kind != yaml.SequenceNode {
		c.reject(RuleWorkflowStructure, at(path+".steps", steps), "workflow steps must be a sequence")
		return
	}
	c.checkSteps(path, steps)
}

func (c *checker) checkSteps(jobPath string, steps *yaml.Node) {
	redactionReady := false
	for index, step := range steps.Content {
		path := jobPath + ".steps[" + strconv.Itoa(index) + "]"
		if step.Kind != yaml.MappingNode {
			c.reject(RuleWorkflowStructure, at(path, step), "workflow step must be a mapping")
			redactionReady = false
			continue
		}
		uses, hasUses := mappingValue(step, "uses")
		run, hasRun := mappingValue(step, "run")
		if !hasUses && !hasRun {
			c.reject(RuleWorkflowStructure, at(path, step), "workflow step must define a nonempty uses or run")
			redactionReady = false
			continue
		}
		if hasUses && (uses.Kind != yaml.ScalarNode || uses.Tag != "!!str" || strings.TrimSpace(uses.Value) == "") {
			c.reject(RuleWorkflowStructure, at(path+".uses", uses), "step uses must be a string action reference")
		}
		if hasRun && (run.Kind != yaml.ScalarNode || run.Tag != "!!str" || strings.TrimSpace(run.Value) == "") {
			c.reject(RuleWorkflowStructure, at(path+".run", run), "step run must be a scalar shell command")
		}
		c.checkContinueOnError(step, path)
		if hasRun {
			c.checkRun(path+".run", run)
		}
		if hasUses {
			c.checkUses(path, step, stepCheckState{redactionComplete: redactionReady})
			redactionReady = false
			continue
		}
		redactionReady = c.isRedactionStep(step)
	}
}

func (c *checker) checkContinueOnError(mapping *yaml.Node, path string) {
	value, exists := mappingValue(mapping, "continue-on-error")
	if exists && !explicitFalse(value) {
		c.reject(RuleContinueOnError, at(path+".continue-on-error", value), "continue-on-error must not enable failure suppression")
	}
}

func (c *checker) checkRun(path string, run *yaml.Node) {
	command, exists := scalarString(run)
	if !exists {
		return
	}
	if dockerCommandPattern.MatchString(command) {
		c.reject(RuleContainerizedExecution, at(path, run), "docker execution is forbidden")
	}
	if swallowedFailurePattern.MatchString(command) {
		c.reject(RuleSwallowedFailure, at(path, run), "shell failure is swallowed by || true or another ignored fallback, exit 0, or disabled errexit")
	}
	if workspaceCommandPattern.MatchString(command) {
		c.reject(RuleReusableExecutable, at(path, run), "executing a binary from the GitHub workspace is forbidden")
	}
	if gitHubEnvironmentPattern.MatchString(command) {
		c.reject(RuleRepositoryNotLiteral, at(path, run), "writing GIT_* configuration through GITHUB_ENV is forbidden")
	}
	if gitConfigurationPattern.MatchString(command) {
		c.reject(RuleRepositoryNotLiteral, at(path, run), "Git configuration mutation is forbidden")
	}
	if gitRepositoryCommand.MatchString(command) {
		rule := RuleRepositoryNotLiteral
		detail := fmt.Sprintf("git repository operation %q is forbidden", strings.TrimSpace(command))
		if !strings.Contains(command, "$") {
			rule = RuleRepositoryNotAllowed
			detail = fmt.Sprintf("repository operation %q is forbidden", strings.TrimSpace(command))
		}
		c.reject(rule, at(path, run), detail)
	}
}

type stepCheckState struct {
	redactionComplete bool
}

func (c *checker) checkUses(path string, step *yaml.Node, state stepCheckState) {
	uses, exists := mappingValue(step, "uses")
	if !exists {
		return
	}
	action, literal := scalarString(uses)
	if !literal {
		return
	}
	if strings.Contains(action, "${{") {
		c.reject(RuleRepositoryNotLiteral, at(path+".uses", uses), "action reference must be literal")
		return
	}
	lowerAction := strings.ToLower(action)
	if strings.HasPrefix(lowerAction, "docker://") {
		c.reject(RuleContainerizedExecution, at(path+".uses", uses), "Docker actions are forbidden")
		return
	}
	if strings.HasPrefix(lowerAction, "./") {
		c.reject(RuleReusableExecutable, at(path+".uses", uses), "local action reuse from the workspace is forbidden")
		return
	}
	actionRepository, valid := actionRepository(lowerAction)
	if !valid {
		c.reject(RuleWorkflowStructure, at(path+".uses", uses), "action reference must use owner/repository@ref syntax")
		return
	}
	switch actionRepository {
	case "actions/cache", "actions/cache/restore", "actions/cache/save", "actions/download-artifact":
		c.reject(RuleReusableExecutable, at(path+".uses", uses), fmt.Sprintf("cache or artifact reuse action %q is forbidden", actionRepository))
		return
	}
	if !approvedAction(actionRepository) {
		c.reject(RuleRepositoryNotAllowed, at(path+".uses", uses), "action repository is not approved")
		return
	}
	switch actionRepository {
	case "actions/checkout":
		c.checkCheckout(path, step)
	case "actions/setup-go":
		c.checkRequiredCacheDisabled(path, step)
	case "actions/upload-artifact":
		c.checkArtifactUpload(path, step, state.redactionComplete)
	}
	c.checkCacheInputs(path, step)
}

func actionRepository(action string) (string, bool) {
	repository, ref, found := strings.Cut(action, "@")
	if !found || repository == "" || ref == "" || strings.Contains(ref, "@") || strings.ContainsAny(action, " \t\r\n") {
		return "", false
	}
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return repository, true
}

func approvedAction(repository string) bool {
	switch repository {
	case "actions/checkout", "actions/setup-go", "actions/upload-artifact":
		return true
	default:
		return false
	}
}
