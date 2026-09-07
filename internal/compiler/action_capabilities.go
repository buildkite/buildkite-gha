package compiler

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func requiredCapabilities(repositoryRoot, workflowPath string, steps []workflow.Step) ([]string, error) {
	capabilities := map[string]struct{}{}
	for _, step := range steps {
		if step.Kind != "uses" || !strings.HasPrefix(step.Uses, "./") {
			continue
		}
		if repositoryRoot == "" {
			return nil, fmt.Errorf("resolve local action %q: workflow path %q must identify a repository root", step.Uses, workflowPath)
		}
		if err := collectActionCapabilities(repositoryRoot, step.Uses, capabilities, nil); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(capabilities))
	for capability := range capabilities {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out, nil
}

func collectActionCapabilities(repositoryRoot, uses string, capabilities map[string]struct{}, stack []string) error {
	if !strings.HasPrefix(uses, "./") {
		return fmt.Errorf("nested remote action %q is unsupported", uses)
	}
	action, err := loadLocalAction(repositoryRoot, uses)
	if err != nil {
		return err
	}
	if slices.Contains(stack, action.Path) {
		return fmt.Errorf("local action recursion detected at %q", action.Path)
	}
	if len(stack) >= metadata.MaxNestedActionDepth {
		return fmt.Errorf("local action nesting exceeds maximum depth %d at %q", metadata.MaxNestedActionDepth, action.Path)
	}
	runtime, err := action.Runtime()
	if err != nil {
		return fmt.Errorf("local action %q uses %w", uses, err)
	}
	for _, capability := range runtime.RequiredCapabilities() {
		capabilities[capability] = struct{}{}
	}
	if runtime != metadata.RuntimeComposite {
		return nil
	}
	stack = append(append([]string(nil), stack...), action.Path)
	for _, child := range action.Runs.Steps {
		if child.Uses != "" {
			if err := collectActionCapabilities(repositoryRoot, child.Uses, capabilities, stack); err != nil {
				return fmt.Errorf("local action %q nested uses %w", uses, err)
			}
		}
	}
	return nil
}

func loadLocalAction(repositoryRoot, uses string) (metadata.Metadata, error) {
	relativeActionPath := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(uses, "./")))
	return metadata.Load(repositoryRoot, relativeActionPath)
}
