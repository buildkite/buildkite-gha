package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const maxResultStepBytes = 1024 * 1024

// TerminalNeedResult records Buildkite's observed failure without a result
// receipt. It grants no outputs, artifacts, or individual job identity.
type TerminalNeedResult struct {
	BuildID string
	Source  ResultSource
	State   string
	Outcome string
	Result  string
}

type resultStep struct {
	Key     string `json:"key"`
	Type    string `json:"type"`
	State   string `json:"state"`
	Outcome string `json:"outcome"`
	Env     struct {
		PlanDigest string `json:"BUILDKITE_GHA_PLAN_DIGEST"`
	} `json:"env"`
}

func resolveTerminalNeedResult(ctx context.Context, agent Agent, buildID string, source ResultSource) (TerminalNeedResult, error) {
	before, err := agent.resultStep(ctx, buildID, source)
	if err != nil {
		return TerminalNeedResult{}, err
	}
	// A promised failure can coexist with a running job. "errored" combines
	// cancellation and other causes, so it cannot establish a GitHub result.
	if before.State != "finished" || before.Outcome != "hard_failed" {
		return TerminalNeedResult{}, fmt.Errorf("missing result from step %q: unsupported state %q, outcome %q", source.StepKey, before.State, before.Outcome)
	}
	// Step export does not identify individual attempts. Require absence from
	// every attempt rather than choosing a status over an older receipt. This
	// also rejects a receipt that appeared after the initial empty search.
	path := ResultPath(source.StepKey, source.PlanDigest)
	output, err := agent.run(ctx, []string{
		"artifact", "search", path, "--step", source.StepKey, "--build", buildID,
		"--format", "%j\\n", "--allow-empty-results", "--include-retried-jobs=true",
	}, nil)
	if err != nil {
		return TerminalNeedResult{}, fmt.Errorf("check result attempts for step %q: %w", source.StepKey, err)
	}
	if len(output) != 0 {
		return TerminalNeedResult{}, fmt.Errorf("step %q has result records from another search or attempt; retry the whole build", source.StepKey)
	}
	after, err := agent.resultStep(ctx, buildID, source)
	if err != nil {
		return TerminalNeedResult{}, err
	}
	if before != after {
		return TerminalNeedResult{}, fmt.Errorf("step %q changed while resolving its missing result; retry the whole build", source.StepKey)
	}
	return TerminalNeedResult{BuildID: buildID, Source: source, State: before.State, Outcome: before.Outcome, Result: "failure"}, nil
}

func (a Agent) resultStep(ctx context.Context, buildID string, source ResultSource) (resultStep, error) {
	args := []string{"step", "get", "--step", source.StepKey, "--build", buildID, "--format", "json"}
	var data, stderr []byte
	var err error
	if runner, ok := a.Runner.(interface {
		RunBounded(context.Context, string, string, []string, []byte, int) ([]byte, []byte, error)
	}); ok {
		data, stderr, err = runner.RunBounded(ctx, "", "buildkite-agent", args, nil, maxResultStepBytes)
	} else {
		data, err = a.run(ctx, args, nil)
	}
	if err != nil {
		if message := strings.TrimSpace(string(stderr)); message != "" {
			return resultStep{}, fmt.Errorf("read result step %q: %w: %s", source.StepKey, err, message)
		}
		return resultStep{}, fmt.Errorf("read result step %q: %w", source.StepKey, err)
	}
	if len(data) > maxResultStepBytes {
		return resultStep{}, fmt.Errorf("result step %q exceeds %d bytes", source.StepKey, maxResultStepBytes)
	}
	var step resultStep
	if err := json.Unmarshal(data, &step); err != nil {
		return resultStep{}, fmt.Errorf("invalid result step %q response", source.StepKey)
	}
	if step.Key != source.StepKey || step.Type != "command" || step.Env.PlanDigest != source.PlanDigest {
		return resultStep{}, fmt.Errorf("result step %q does not match its planned identity", source.StepKey)
	}
	return step, nil
}
