package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"
)

const maxSecretsPolicyBytes = 64 << 10

var (
	organizationSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	clusterIDPattern        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	buildkiteSecretPattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,254}$`)
	githubSecretPattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func validateMigrationSecretName(name string) error {
	if !buildkiteSecretPattern.MatchString(name) {
		return fmt.Errorf("secret name %q must start with a letter, contain only letters, numbers, or underscores, and be at most 255 characters", name)
	}
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "bk") || strings.HasPrefix(lower, "buildkite") {
		return fmt.Errorf("secret name %q uses a prefix reserved by Buildkite", name)
	}
	return nil
}

func readSecretsPolicy(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read policy: %w", err)
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maxSecretsPolicyBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return "", fmt.Errorf("read policy: %w", err)
	}
	if len(contents) > maxSecretsPolicyBytes {
		return "", fmt.Errorf("policy exceeds %d bytes", maxSecretsPolicyBytes)
	}
	return validateSecretsPolicy(string(contents))
}

func validateSecretsPolicy(contents string) (string, error) {
	policy := strings.TrimSpace(contents)
	if policy == "" {
		return "", errors.New("policy must not be empty")
	}
	if strings.Contains(policy, "${{") {
		return "", errors.New("policy must not contain GitHub expression syntax")
	}
	var rules []map[string]any
	if err := yaml.Unmarshal([]byte(policy), &rules); err != nil {
		return "", fmt.Errorf("parse policy YAML: %w", err)
	}
	if len(rules) == 0 {
		return "", errors.New("policy must contain at least one rule")
	}
	claims := map[string]bool{
		"pipeline_id": true, "build_source": true, "cluster_queue_id": true,
		"cluster_queue_key": true, "pipeline_slug": true, "build_branch": true,
		"build_creator": true, "build_creator_team": true,
	}
	for ruleIndex, rule := range rules {
		if len(rule) == 0 {
			return "", fmt.Errorf("policy rule %d must contain at least one claim", ruleIndex+1)
		}
		for claim, conditions := range rule {
			if !claims[claim] {
				return "", fmt.Errorf("policy rule %d contains unknown claim %q", ruleIndex+1, claim)
			}
			if !validPolicyConditions(conditions) {
				return "", fmt.Errorf("policy rule %d claim %q must be a non-empty string or list of non-empty strings", ruleIndex+1, claim)
			}
		}
	}
	return policy + "\n", nil
}

func validPolicyConditions(value any) bool {
	switch conditions := value.(type) {
	case string:
		return conditions != ""
	case []any:
		if len(conditions) == 0 {
			return false
		}
		for _, condition := range conditions {
			text, ok := condition.(string)
			if !ok || text == "" {
				return false
			}
		}
		return true
	default:
		return false
	}
}
