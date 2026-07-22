package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodePreservesEnvelopeFixtureContract(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "testdata", "plans", "plans", "valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	job, err := Decode(source)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if job.Compiler.DistributionDigest == "" || job.Event.PayloadDigest == "" || job.Target.StepKey != "gha-ci-test" {
		t.Fatalf("decoded fixture lost trust bindings: %#v", job)
	}
}

func TestDecodeFailsClosed(t *testing.T) {
	valid := validJob()
	encoded, err := Encode(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(encoded); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "unknown field", source: strings.Replace(string(encoded), `"steps":`, `"unexpected":true,"steps":`, 1), want: "unknown field"},
		{name: "duplicate key", source: strings.Replace(string(encoded), `"schema":`, `"schema":"duplicate","schema":`, 1), want: "duplicate JSON key"},
		{name: "unknown schema", source: strings.Replace(string(encoded), Schema, "job-plan-v2", 1), want: "unsupported job plan schema"},
		{name: "trailing JSON", source: string(encoded) + `{}`, want: "multiple JSON values"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode([]byte(test.source))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Decode() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateRejectsAmbiguousSteps(t *testing.T) {
	job := validJob()
	job.Steps = append(job.Steps, job.Steps[0])
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate step id") {
		t.Fatalf("Validate() error = %v, want duplicate step id", err)
	}
}

func TestValidateRejectsOutOfRangeTimeouts(t *testing.T) {
	job := validJob()
	job.TimeoutMinutes = 361
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "job timeout_minutes") {
		t.Fatalf("Validate() error = %v, want job timeout error", err)
	}
	job = validJob()
	job.Steps[0].TimeoutMinutes = -1
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "step-1") {
		t.Fatalf("Validate() error = %v, want step timeout error", err)
	}
}

func TestValidateRejectsInvalidStaticDependencies(t *testing.T) {
	for _, test := range []struct {
		name         string
		dependencies []string
		want         string
	}{
		{name: "unsorted", dependencies: []string{"gha-z", "gha-a"}, want: "must be sorted"},
		{name: "duplicate", dependencies: []string{"gha-a", "gha-a"}, want: "repeats dependency"},
		{name: "self", dependencies: []string{"gha-test"}, want: "invalid dependency"},
		{name: "invalid", dependencies: []string{"gha/a"}, want: "invalid dependency"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := validJob()
			job.Dependencies = test.dependencies
			if err := job.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateBindsEveryDependencyToOneLogicalNeed(t *testing.T) {
	digest := "sha256:" + strings.Repeat("4", 64)
	job := validJob()
	job.Dependencies = []string{"gha-build-one", "gha-build-two"}
	job.NeedSources = map[string][]NeedSource{
		"build": {
			{StepKey: "gha-build-one", PlanDigest: digest},
			{StepKey: "gha-build-two", PlanDigest: digest},
		},
	}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}

	job.NeedSources["other"] = []NeedSource{{StepKey: "gha-build-two", PlanDigest: digest}}
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "multiple logical owners") {
		t.Fatalf("Validate() error = %v, want duplicate logical owner rejection", err)
	}
	delete(job.NeedSources, "other")
	job.NeedSources["build"] = job.NeedSources["build"][:1]
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "dependencies and prerequisite producers differ") {
		t.Fatalf("Validate() error = %v, want uncovered dependency rejection", err)
	}
}

func TestPlanBoundaryRequiresConcreteCapabilities(t *testing.T) {
	job := validJob()
	job.RequiredCapabilities = nil
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"required_capabilities": []`) {
		t.Fatalf("Encode() = %s, want concrete capabilities array", encoded)
	}
	encoded = []byte(strings.Replace(string(encoded), `"required_capabilities": []`, `"required_capabilities": null`, 1))
	if _, err := Decode(encoded); err == nil || !strings.Contains(err.Error(), "concrete array") {
		t.Fatalf("Decode() error = %v, want concrete capabilities error", err)
	}
}

func validJob() Job {
	return Job{
		Schema:               Schema,
		Compiler:             Compiler{Version: "0.0.0-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Workflow:             Workflow{Path: ".github/workflows/ci.yml", Digest: "sha256:" + strings.Repeat("0", 64), LogicalJobID: "test"},
		Event:                Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:               Target{StepKey: "gha-test", Queue: "ubuntu-latest"},
		RequiredCapabilities: []string{},
		Steps:                []Step{{ID: "step-1", Kind: "run", Command: "true"}},
	}
}
