package workflowpolicy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	fullCommitPattern        = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
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
	validatedResolvers := make(map[string]Repository)
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
		resolver, validResolver := validatedRefResolver(step)
		if hasRun {
			c.checkRun(path+".run", run, validResolver)
		}
		if validResolver {
			validatedResolvers[resolver.id] = resolver.repository
		}
		if hasUses {
			c.checkUses(path, step, stepCheckState{redactionComplete: redactionReady, validatedResolvers: validatedResolvers})
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

func (c *checker) checkRun(path string, run *yaml.Node, validatedResolver bool) {
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
	if gitRepositoryCommand.MatchString(command) && !validatedResolver {
		rule := RuleRepositoryNotLiteral
		detail := fmt.Sprintf("git repository operation %q is allowed only in the validated resolver", strings.TrimSpace(command))
		if !strings.Contains(command, "$") {
			rule = RuleRepositoryNotAllowed
			detail = fmt.Sprintf("repository operation %q is forbidden outside the validated resolver", strings.TrimSpace(command))
		}
		c.reject(rule, at(path, run), detail)
	}
}

type stepCheckState struct {
	redactionComplete  bool
	validatedResolvers map[string]Repository
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
	actionRepository, _, found := strings.Cut(lowerAction, "@")
	switch actionRepository {
	case "actions/cache", "actions/cache/restore", "actions/cache/save", "actions/download-artifact":
		c.reject(RuleReusableExecutable, at(path+".uses", uses), fmt.Sprintf("cache or artifact reuse action %q is forbidden", actionRepository))
		return
	}
	if !found {
		c.reject(RuleOtherRepositoryRef, at(path+".uses", uses), "action must use its approved immutable SHA")
		return
	}
	approvedRepository, _, pinned := approvedAction(action)
	if approvedRepository == "" {
		c.reject(RuleRepositoryNotAllowed, at(path+".uses", uses), "action repository is not approved")
		return
	}
	if !pinned {
		c.reject(RuleOtherRepositoryRef, at(path+".uses", uses), "action must use its approved immutable SHA")
		return
	}
	switch approvedRepository {
	case "actions/checkout":
		c.checkCheckout(path, step, state.validatedResolvers)
	case "actions/setup-go":
		c.checkRequiredCacheDisabled(path, step)
	case "actions/upload-artifact":
		c.checkArtifactUpload(path, step, state.redactionComplete)
	}
	c.checkCacheInputs(path, step)
}

func approvedAction(action string) (string, string, bool) {
	repository, ref, found := strings.Cut(strings.ToLower(action), "@")
	if !found {
		return repository, "", false
	}
	approvedRefs := map[string]string{
		"actions/checkout":        "3d3c42e5aac5ba805825da76410c181273ba90b1",
		"actions/setup-go":        "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
		"actions/upload-artifact": "b7c566a772e6b6bfb58ed0dc250532a479d7789f",
	}
	approvedRef, approved := approvedRefs[repository]
	if !approved {
		return "", ref, false
	}
	return repository, ref, ref == approvedRef
}
