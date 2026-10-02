package transport

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type terminalReply struct {
	output, stderr string
	err            error
}

type terminalResultRunner struct {
	*resultRunner
	steps   []terminalReply
	history terminalReply
	limits  []int
}

func (r *terminalResultRunner) Run(ctx context.Context, dir, name string, args []string, stdin []byte) ([]byte, error) {
	if slices.Contains(args, "--include-retried-jobs=true") {
		r.commands = append(r.commands, capturedCommand{dir: dir, name: name, args: append([]string(nil), args...), stdin: string(stdin)})
		return []byte(r.history.output), r.history.err
	}
	return r.resultRunner.Run(ctx, dir, name, args, stdin)
}

func (r *terminalResultRunner) RunBounded(_ context.Context, dir, name string, args []string, stdin []byte, limit int) ([]byte, []byte, error) {
	r.commands = append(r.commands, capturedCommand{dir: dir, name: name, args: append([]string(nil), args...), stdin: string(stdin)})
	r.limits = append(r.limits, limit)
	if len(r.steps) == 0 {
		return nil, nil, errors.New("unexpected step lookup")
	}
	reply := r.steps[0]
	r.steps = r.steps[1:]
	if len(reply.output) > limit || len(reply.stderr) > limit {
		return nil, nil, fmt.Errorf("step response exceeds %d bytes", limit)
	}
	return []byte(reply.output), []byte(reply.stderr), reply.err
}

func newTerminalResultRunner(source ResultSource) *terminalResultRunner {
	snapshot := fmt.Sprintf(`{"key":%q,"type":"command","state":"finished","outcome":"hard_failed","env":{"BUILDKITE_GHA_PLAN_DIGEST":%q},"label":"Producer"}`, source.StepKey, source.PlanDigest)
	return &terminalResultRunner{resultRunner: &resultRunner{}, steps: []terminalReply{{output: snapshot}, {output: snapshot}}}
}

func TestLoadRuntimeNeedsRequiresStableFailureEvidence(t *testing.T) {
	source := ResultSource{StepKey: "gha-producer", PlanDigest: Digest([]byte("producer-plan"))}
	valid := newTerminalResultRunner(source).steps[0].output
	replace := func(old, new string) string { return strings.Replace(valid, old, new, 1) }
	command := exec.Command("sh", "-c", "exit 1")
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd", "/C", "exit 1")
	}
	exitErr := command.Run()
	if _, ok := exitErr.(*exec.ExitError); !ok {
		t.Fatalf("create provider exit error: %v", exitErr)
	}
	tests := []struct {
		name, stage, output, stderr, want string
		err                               error
	}{
		{name: "finished hard failure", output: valid},
		{name: "passed", output: replace(`"hard_failed"`, `"passed"`), want: "unsupported"},
		{name: "soft failure", output: replace(`"hard_failed"`, `"soft_failed"`), want: "unsupported"},
		{name: "unclassified error", output: replace(`"hard_failed"`, `"errored"`), want: "unsupported"},
		{name: "missing outcome", output: replace(`"hard_failed"`, `null`), want: "unsupported"},
		{name: "running promised failure", output: replace(`"finished"`, `"running"`), want: "unsupported"},
		{name: "broken", output: replace(`"finished"`, `"broken"`), want: "unsupported"},
		{name: "skipped", output: replace(`"finished"`, `"skipped"`), want: "unsupported"},
		{name: "cancelled", output: replace(`"finished"`, `"canceled"`), want: "unsupported"},
		{name: "unknown state", output: replace(`"finished"`, `"future_state"`), want: "unsupported"},
		{name: "wrong step", output: replace(source.StepKey, "gha-other"), want: "planned identity"},
		{name: "wrong type", output: replace(`"command"`, `"trigger"`), want: "planned identity"},
		{name: "wrong plan", output: replace(source.PlanDigest, Digest([]byte("other"))), want: "planned identity"},
		{name: "missing marker", output: replace("BUILDKITE_GHA_PLAN_DIGEST", "OTHER"), want: "planned identity"},
		{name: "malformed JSON", output: `{`, want: "invalid result step"},
		{name: "trailing JSON", output: valid + `{}`, want: "invalid result step"},
		{name: "wrong shape", output: `[]`, want: "invalid result step"},
		{name: "oversized JSON", output: valid + strings.Repeat(" ", maxResultStepBytes), want: "exceeds"},
		{name: "authentication stderr", err: exitErr, stderr: "401 Unauthorized", want: "401 Unauthorized"},
		{name: "second query stderr", stage: "after", err: exitErr, stderr: "connection reset", want: "connection reset"},
		{name: "changed snapshot", stage: "after", output: replace(`"finished"`, `"running"`), want: "changed"},
		{name: "changed plan", stage: "after", output: replace(source.PlanDigest, Digest([]byte("new"))), want: "planned identity"},
		{name: "earlier attempt receipt", stage: "history", output: testJobID + "\n", want: "another search or attempt"},
		{name: "malformed history", stage: "history", output: "\n", want: "another search or attempt"},
		{name: "history query error", stage: "history", err: context.DeadlineExceeded, want: "deadline exceeded"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := newTerminalResultRunner(source)
			reply := terminalReply{output: test.output, stderr: test.stderr, err: test.err}
			switch test.stage {
			case "history":
				runner.history = reply
			case "after":
				runner.steps[1] = reply
			default:
				runner.steps[0] = reply
			}
			needs, terminal, err := LoadRuntimeNeeds(t.Context(), Agent{Runner: runner}, t.TempDir(), testBuildID, map[string][]ResultSource{"producer": {source}}, nil)
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) || needs != nil || len(terminal) != 0 {
					t.Fatalf("LoadRuntimeNeeds() = %#v, %#v, %v; want %q without recovered data", needs, terminal, err, test.want)
				}
				if test.err != nil && !errors.Is(err, test.err) {
					t.Fatalf("error = %v, want original query error %v", err, test.err)
				}
				return
			}
			wantNeed := NeedResult{Result: "failure", Outputs: map[string]string{}}
			wantTerminal := []TerminalNeedResult{{BuildID: testBuildID, Source: source, State: "finished", Outcome: "hard_failed", Result: "failure"}}
			if err != nil || !reflect.DeepEqual(needs["producer"], wantNeed) || !reflect.DeepEqual(terminal, wantTerminal) {
				t.Fatalf("LoadRuntimeNeeds() = %#v, %#v, %v; want failure without receipt authority", needs, terminal, err)
			}
			path := ResultPath(source.StepKey, source.PlanDigest)
			step := []string{"step", "get", "--step", source.StepKey, "--build", testBuildID, "--format", "json"}
			wantCalls := []capturedCommand{
				{name: "buildkite-agent", args: []string{"artifact", "search", path, "--step", source.StepKey, "--format", "%j\\n", "--allow-empty-results", "--include-retried-jobs=false"}},
				{name: "buildkite-agent", args: step},
				{name: "buildkite-agent", args: []string{"artifact", "search", path, "--step", source.StepKey, "--build", testBuildID, "--format", "%j\\n", "--allow-empty-results", "--include-retried-jobs=true"}},
				{name: "buildkite-agent", args: step},
			}
			if !reflect.DeepEqual(runner.commands, wantCalls) || !reflect.DeepEqual(runner.limits, []int{maxResultStepBytes, maxResultStepBytes}) {
				t.Fatalf("commands/bounds = %#v/%v; want scoped history between bounded snapshots", runner.commands, runner.limits)
			}
		})
	}
}

func TestLoadRuntimeNeedsRetainsOnlyVerifiedReceiptPayloads(t *testing.T) {
	good := ResultSource{StepKey: "gha-build-one", PlanDigest: Digest([]byte("one-plan"))}
	missing := ResultSource{StepKey: "gha-build-two", PlanDigest: Digest([]byte("two-plan"))}
	manifest := resultManifest(testJobID, good.StepKey, good.PlanDigest, "success", Output{Name: "version", Value: "v1"})
	manifest.Artifacts = []ResultArtifact{resultArtifact("payload", "1", strings.Repeat("a", 64))}
	tests := []struct {
		name       string
		sources    []ResultSource
		projected  bool
		corrupt    bool
		wantResult string
	}{
		{name: "healthy receipt wins", sources: []ResultSource{good}, wantResult: "success"},
		{name: "mixed fan-in", sources: []ResultSource{missing, good}, wantResult: "failure"},
		{name: "missing projection does not conflict or invent keys", sources: []ResultSource{missing, good}, projected: true, wantResult: "failure"},
		{name: "corrupt receipt is fatal", sources: []ResultSource{good}, corrupt: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := newTerminalResultRunner(missing)
			runner.jobByStep = map[string]string{good.StepKey: testJobID}
			path := ResultPath(good.StepKey, good.PlanDigest)
			runner.dataByPath = map[string][]byte{path: mustManifest(t, manifest)}
			if test.corrupt {
				runner.dataByPath[path] = []byte("not-json")
			}
			var projections map[string][]OutputProjection
			if test.projected {
				projections = map[string][]OutputProjection{"build": {
					{Name: "version", StepKey: good.StepKey, Output: "version"},
					{Name: "version", StepKey: missing.StepKey, Output: "version"},
					{Name: "unavailable", StepKey: missing.StepKey, Output: "version"},
				}}
			}
			needs, terminal, err := LoadRuntimeNeeds(t.Context(), Agent{Runner: runner}, t.TempDir(), testBuildID, map[string][]ResultSource{"build": test.sources}, projections)
			if test.corrupt {
				if err == nil || needs != nil || len(terminal) != 0 || len(runner.commands) != 2 {
					t.Fatalf("corrupt receipt = %#v, %#v, %v; must stop before fallback", needs, terminal, err)
				}
				return
			}
			want := NeedResult{Result: test.wantResult, Outputs: map[string]string{"version": "v1"}, Producers: []Producer{manifest.Producer}, Artifacts: []NeedArtifact{{Artifact: manifest.Artifacts[0], Producer: manifest.Producer}}}
			if err != nil || !reflect.DeepEqual(needs["build"], want) {
				t.Fatalf("needs = %#v, %v; want only verified receipt payload %#v", needs, err, want)
			}
			if len(test.sources) == 1 {
				if len(terminal) != 0 || len(runner.commands) != 2 {
					t.Fatalf("healthy receipt used fallback: %#v / %#v", terminal, runner.commands)
				}
			} else if len(terminal) != 1 || terminal[0].Source != missing || terminal[0].Result != "failure" {
				t.Fatalf("terminal evidence = %#v, want only missing producer failure", terminal)
			}
		})
	}
	strict := newTerminalResultRunner(missing)
	needs, err := LoadNeeds(t.Context(), Agent{Runner: strict}, t.TempDir(), testBuildID, map[string][]ResultSource{"build": {missing}}, nil)
	if !errors.Is(err, ErrResultNotFound) || needs != nil || len(strict.commands) != 1 {
		t.Fatalf("strict LoadNeeds() = %#v, %v; must reject absence without querying state", needs, err)
	}
}
