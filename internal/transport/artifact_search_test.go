package transport

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type artifactSearchRunner struct {
	output   []byte
	err      error
	commands []capturedCommand
}

func (r *artifactSearchRunner) Run(_ context.Context, dir, name string, args []string, stdin []byte) ([]byte, error) {
	r.commands = append(r.commands, capturedCommand{dir: dir, name: name, args: append([]string(nil), args...), stdin: string(stdin)})
	return r.output, r.err
}

func TestSearchArtifactProducerDistinguishesAbsenceFromInvalidResultsAndQueryErrors(t *testing.T) {
	t.Setenv("BUILDKITE_AGENT_INCLUDE_RETRIED_JOBS", "true")
	source := ResultSource{StepKey: "gha-producer", PlanDigest: Digest([]byte("producer-plan"))}
	path := ResultPath(source.StepKey, source.PlanDigest)
	tests := []struct {
		name, output, wantJob string
		err                   error
		absent                bool
	}{
		{name: "one producer", output: testJobID + "\n", wantJob: testJobID},
		{name: "confirmed absence", absent: true},
		{name: "malformed job ID", output: "not-a-job\n"},
		{name: "missing job ID", output: "\n"},
		{name: "whitespace job ID", output: " \n"},
		{name: "ambiguous producers", output: testJobID + "\n" + testJobID2 + "\n"},
		{name: "duplicate artifacts", output: testJobID + "\n" + testJobID + "\n"},
		{name: "blank before valid producer", output: "\n" + testJobID + "\n"},
		{name: "blank after valid producer", output: testJobID + "\n\n"},
		{name: "authentication", err: errors.New("401 Unauthorized")},
		{name: "network", err: errors.New("connection reset by peer")},
		{name: "legacy no matches error", err: errors.New("no matches found")},
		{name: "cancelled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "partial response", output: testJobID + "\n", err: errors.New("read failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &artifactSearchRunner{output: []byte(test.output), err: test.err}
			job, err := (Agent{Runner: runner}).SearchArtifactProducer(t.Context(), path, "gha-producer")
			if job != test.wantJob || (err == nil) != (test.wantJob != "") {
				t.Fatalf("SearchArtifactProducer() = %q, %v; want producer %q", job, err, test.wantJob)
			}
			if errors.Is(err, ErrResultNotFound) != test.absent || (test.err != nil && !errors.Is(err, test.err)) {
				t.Fatalf("search error = %v; want absence %t, original query error %v", err, test.absent, test.err)
			}
			if test.wantJob == "" && !test.absent {
				runner.commands = nil
				needs, terminal, loadErr := LoadRuntimeNeeds(t.Context(), Agent{Runner: runner}, t.TempDir(), testBuildID, map[string][]ResultSource{"producer": {source}}, nil)
				if loadErr == nil || needs != nil || len(terminal) != 0 || (test.err != nil && !errors.Is(loadErr, test.err)) {
					t.Fatalf("LoadRuntimeNeeds() = %#v, %#v, %v; query failures must not recover data", needs, terminal, loadErr)
				}
			}
			want := []capturedCommand{{name: "buildkite-agent", args: []string{
				"artifact", "search", path, "--step", "gha-producer", "--format", "%j\\n", "--allow-empty-results", "--include-retried-jobs=false",
			}}}
			if !reflect.DeepEqual(runner.commands, want) {
				t.Fatalf("commands = %#v, want one explicitly scoped search %#v", runner.commands, want)
			}
		})
	}
}
