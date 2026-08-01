package workflowpolicy

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

func (c *checker) checkCheckout(path string, step *yaml.Node) {
	with, exists := mappingValue(step, "with")
	if !exists || with.Kind != yaml.MappingNode {
		c.reject(RulePersistCredentials, at(path+".with.persist-credentials", step), "checkout requires persist-credentials: false")
		return
	}
	persistCredentials, exists := mappingValue(with, "persist-credentials")
	if !exists || !explicitFalse(persistCredentials) {
		node := with
		if exists {
			node = persistCredentials
		}
		c.reject(RulePersistCredentials, at(path+".with.persist-credentials", node), "checkout requires persist-credentials: false as a boolean")
	}
	repositoryNode, exists := mappingValue(with, "repository")
	if !exists {
		return
	}
	repository, literal := scalarString(repositoryNode)
	if !literal || strings.Contains(repository, "${{") {
		c.reject(RuleRepositoryNotLiteral, at(path+".with.repository", repositoryNode), "checkout repository must be a literal")
		return
	}
	if repository != string(RepositoryAgent) && repository != string(RepositoryNezha) {
		detail := fmt.Sprintf("repository %q is not allowed; only nezhahq/agent and nezhahq/nezha are allowed", repository)
		c.reject(RuleRepositoryNotAllowed, at(path+".with.repository", repositoryNode), detail)
		return
	}
	ref, exists := mappingValue(with, "ref")
	if !exists {
		return
	}
	refValue, literal := scalarString(ref)
	if !literal || strings.TrimSpace(refValue) == "" || strings.Contains(refValue, "${{") {
		c.reject(RuleRepositoryNotLiteral, at(path+".with.ref", ref), "checkout ref must be a nonempty literal")
	}
}

func (c *checker) checkCacheInputs(path string, step *yaml.Node) {
	with, exists := mappingValue(step, "with")
	if !exists {
		return
	}
	for _, key := range []string{"cache", "cache-dependency-path"} {
		value, present := mappingValue(with, key)
		if present && !explicitFalse(value) {
			detail := fmt.Sprintf("dependency or executable cache input %s is forbidden", key)
			c.reject(RuleReusableExecutable, at(path+".with."+key, value), detail)
		}
	}
}
