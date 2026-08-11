package plan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestDecodePreservesPlanContract(t *testing.T) {
	source, err := Encode(validJob())
	if err != nil {
		t.Fatal(err)
	}
	job, err := Decode(source)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if job.Compiler.DistributionDigest == "" || job.Event.PayloadDigest == "" || job.Target.StepKey != "gha-test" {
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
		{name: "unknown schema", source: strings.Replace(string(encoded), Schema, "job-plan-v3", 1), want: "unsupported job plan schema"},
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

func TestValidateConcurrentStepTopology(t *testing.T) {
	job := validJob()
	job.Steps = []Step{
		{ID: "producer", Kind: "run", Command: "true", Background: true},
		{ID: "barrier", Kind: "wait", Targets: []string{"PRODUCER"}},
		{ID: "all", Kind: "wait-all"},
		{ID: "stop", Kind: "cancel", Targets: []string{"producer"}},
	}
	if err := job.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	tests := []struct {
		name string
		step Step
		want string
	}{
		{name: "forward target", step: Step{ID: "wait", Kind: "wait", Targets: []string{"later"}}, want: "not a prior background step"},
		{name: "duplicate target", step: Step{ID: "wait", Kind: "wait", Targets: []string{"producer", "PRODUCER"}}, want: "repeats target"},
		{name: "wait all target", step: Step{ID: "wait", Kind: "wait-all", Targets: []string{"producer"}}, want: "cannot target"},
		{name: "control payload", step: Step{ID: "wait", Kind: "wait-all", Command: "true"}, want: "incompatible execution fields"},
		{name: "control continue on error", step: Step{ID: "wait", Kind: "wait-all", ContinueOnError: true}, want: "incompatible execution fields"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := validJob()
			if test.name == "duplicate target" || test.name == "wait all target" {
				job.Steps[0].ID = "producer"
				job.Steps[0].Background = true
				job.Steps = append(job.Steps, test.step)
			} else {
				job.Steps = []Step{test.step, {ID: "later", Kind: "run", Command: "true", Background: true}}
			}
			if err := job.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
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

func TestValidateAllowsNamespacedLogicalNeed(t *testing.T) {
	digest := "sha256:" + strings.Repeat("4", 64)
	job := validJob()
	job.Dependencies = []string{"gha-delegated-second"}
	job.NeedSources = map[string][]NeedSource{
		"delegated.second": {{StepKey: "gha-delegated-second", PlanDigest: digest}},
	}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}

	delete(job.NeedSources, "delegated.second")
	job.NeedSources["delegated/second"] = []NeedSource{{StepKey: "gha-delegated-second", PlanDigest: digest}}
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "invalid prerequisite") {
		t.Fatalf("Validate() error = %v, want invalid namespaced prerequisite rejection", err)
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

func TestRequiredSecretsRequireCapability(t *testing.T) {
	job := validJob()
	job.RequiredSecrets = []string{"TOKEN"}
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "secrets capability") {
		t.Fatalf("Validate() error = %v, want capability binding", err)
	}
}

func TestV3ActionLocksRoundTripAndValidateAgainstSchema(t *testing.T) {
	job := validJob()
	job.Schema = SchemaV3
	job.Steps = []Step{{ID: "local", Kind: "uses", Uses: "./actions/build", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
	job.Actions = []ActionLock{{
		ID: "a-0000000000000001", Source: "workspace", Path: "actions/build",
		SourceDigest: "sha256:" + strings.Repeat("a", 64),
	}}
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := Encode(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(reencoded) {
		t.Fatalf("v3 encoding is not deterministic\nfirst: %s\nsecond: %s", encoded, reencoded)
	}

	schemaSource, err := os.ReadFile(filepath.Join("..", "..", "schemas", "job-plan-v3.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument, planDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &planDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(SchemaV3, schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(planDocument); err != nil {
		t.Fatalf("v3 plan does not validate against schema: %v", err)
	}
}

func TestV7RequiresMiseRoundTripAndSchema(t *testing.T) {
	requiresMise := false
	job := validJob()
	job.Schema = SchemaV7
	job.Steps = []Step{{ID: "native", Kind: "uses", Uses: "actions/checkout@v4", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
	job.Actions = []ActionLock{{
		ID: "a-0000000000000001", Source: "github", Repository: "actions/checkout", RequestedRef: "v4",
		Commit: strings.Repeat("a", 40), SourceDigest: "sha256:" + strings.Repeat("b", 64),
	}}
	job.RequiresMise = &requiresMise
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil || decoded.RequiresMise == nil || *decoded.RequiresMise {
		t.Fatalf("Decode() requires_mise = %#v, error = %v", decoded.RequiresMise, err)
	}
	compiler := jsonschema.NewCompiler()
	source, err := os.ReadFile(filepath.Join("..", "..", "schemas", "job-plan-v7.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(source, &document); err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource(SchemaV7, document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(SchemaV7)
	if err != nil {
		t.Fatal(err)
	}
	var planDocument any
	if err := json.Unmarshal(encoded, &planDocument); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(planDocument); err != nil {
		t.Fatalf("v7 plan does not validate against schema: %v", err)
	}
	job.RequiresMise = nil
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "explicit requires_mise") {
		t.Fatalf("Validate() missing requires_mise error = %v", err)
	}
	job.RequiresMise = &requiresMise
	job.Steps[0].Action = nil
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "immutable selector") {
		t.Fatalf("Validate() unresolved false error = %v", err)
	}
}

func TestV3RemoteNestedActionLocks(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	commit := strings.Repeat("c", 40)
	job := validJob()
	job.Schema = SchemaV3
	job.Steps = []Step{{ID: "remote", Kind: "uses", Uses: "owner/repo/root@v1", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
	job.Actions = []ActionLock{
		{ID: "a-0000000000000001", Source: "github", Repository: "owner/repo", RequestedRef: "v1", Commit: commit, Path: "root", SourceDigest: digest, Children: map[string]ActionSelector{"owner/repo/root/child@v1": {Lock: "a-0000000000000002"}}},
		{ID: "a-0000000000000002", Source: "github", Repository: "owner/repo", RequestedRef: "v1", Commit: commit, Path: "root/child", SourceDigest: digest},
	}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	job.Actions[1].Path = "other"
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "remote child does not match lock identity") {
		t.Fatalf("Validate() error = %v, want nested remote identity rejection", err)
	}
}

func TestV3ActionLockValidationMatrix(t *testing.T) {
	remote := func() Job {
		job := validJob()
		job.Schema = SchemaV3
		job.Steps = []Step{{ID: "remote", Kind: "uses", Uses: "Owner/Repo/root@v1", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
		job.Actions = []ActionLock{{ID: "a-0000000000000001", Source: "github", Repository: "owner/repo", RequestedRef: "v1", Commit: strings.Repeat("c", 40), Path: "root", SourceDigest: "sha256:" + strings.Repeat("b", 64)}}
		return job
	}
	if err := remote().Validate(); err != nil {
		t.Fatalf("case-insensitive requested repository should match canonical lock: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Job)
		want string
	}{
		{"uppercase canonical repository", func(j *Job) { j.Actions[0].Repository = "Owner/repo" }, "invalid GitHub identity"},
		{"ref case is exact", func(j *Job) { j.Steps[0].Uses = "owner/repo/root@V1" }, "does not match"},
		{"path case is exact", func(j *Job) { j.Steps[0].Uses = "owner/repo/Root@v1" }, "does not match"},
		{"malformed step selector", func(j *Job) { j.Steps[0].Action.Lock = "bad" }, "malformed action selector"},
		{"malformed child selector", func(j *Job) { j.Actions[0].Children = map[string]ActionSelector{"owner/other@v1": {Lock: "bad"}} }, "invalid child selector"},
		{"unsorted IDs", func(j *Job) {
			j.Actions = append([]ActionLock{{ID: "a-0000000000000002", Source: "workspace", Path: "x", SourceDigest: "sha256:" + strings.Repeat("a", 64)}}, j.Actions...)
		}, "sorted IDs"},
		{"invalid digest", func(j *Job) { j.Actions[0].SourceDigest = "sha256:no" }, "invalid digest"},
		{"invalid commit", func(j *Job) { j.Actions[0].Commit = "abc" }, "invalid GitHub identity"},
		{"segment over byte limit", func(j *Job) { j.Actions[0].Path = strings.Repeat("x", 256) }, "invalid GitHub identity"},
		{"path control", func(j *Job) { j.Actions[0].Path = "x\ny" }, "invalid GitHub identity"},
		{"path invalid utf8", func(j *Job) { j.Actions[0].Path = string([]byte{0xff}) }, "invalid GitHub identity"},
		{"unused lock", func(j *Job) {
			j.Actions = append(j.Actions, ActionLock{ID: "a-0000000000000002", Source: "workspace", Path: "x", SourceDigest: "sha256:" + strings.Repeat("a", 64)})
		}, "unused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := remote()
			tt.edit(&job)
			if err := job.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestV3LocalChildPreservesParentIdentity(t *testing.T) {
	job := validJob()
	job.Schema = SchemaV3
	digest := "sha256:" + strings.Repeat("a", 64)
	job.Steps = []Step{{ID: "local", Kind: "uses", Uses: "./root", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
	job.Actions = []ActionLock{
		{ID: "a-0000000000000001", Source: "workspace", Path: "root", SourceDigest: digest, Children: map[string]ActionSelector{"./child": {Lock: "a-0000000000000002"}}},
		{ID: "a-0000000000000002", Source: "workspace", Path: "child", SourceDigest: digest},
	}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	job.Actions[1].Path = "root/child"
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "does not match workspace action identity") {
		t.Fatalf("Validate() error = %v, want workspace-root child path rejection", err)
	}
	job.Actions[1].Path = "child"
	job.Actions[1].SourceDigest = "sha256:" + strings.Repeat("b", 64)
	if err := job.Validate(); err != nil {
		t.Fatalf("workspace child may bind its own action tree digest: %v", err)
	}
}

func TestV3RemoteCompositeLocalChildUsesWorkspaceIdentity(t *testing.T) {
	job := validJob()
	job.Schema = SchemaV3
	job.Steps = []Step{{ID: "remote", Kind: "uses", Uses: "owner/repo/root@v1", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
	job.Actions = []ActionLock{
		{
			ID: "a-0000000000000001", Source: "github", Repository: "owner/repo", RequestedRef: "v1",
			Commit: strings.Repeat("c", 40), Path: "root", SourceDigest: "sha256:" + strings.Repeat("a", 64),
			Children: map[string]ActionSelector{"./child": {Lock: "a-0000000000000002"}},
		},
		{ID: "a-0000000000000002", Source: "workspace", Path: "child", SourceDigest: "sha256:" + strings.Repeat("b", 64)},
	}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	job.Actions[1] = ActionLock{
		ID: "a-0000000000000002", Source: "github", Repository: "owner/repo", RequestedRef: "v1",
		Commit: strings.Repeat("c", 40), Path: "child", SourceDigest: "sha256:" + strings.Repeat("a", 64),
	}
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "does not match workspace action identity") {
		t.Fatalf("Validate() error = %v, want downloaded-source substitution rejection", err)
	}
}

func TestV3ActionSelectorsAndPathsFailClosed(t *testing.T) {
	local := func() Job {
		job := validJob()
		job.Schema = SchemaV3
		job.Steps = []Step{{ID: "local", Kind: "uses", Uses: "./actions/build", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
		job.Actions = []ActionLock{{ID: "a-0000000000000001", Source: "workspace", Path: "actions/build", SourceDigest: "sha256:" + strings.Repeat("a", 64)}}
		return job
	}
	tests := []struct {
		name string
		edit func(*Job)
		want string
	}{
		{name: "missing selector", edit: func(j *Job) { j.Steps[0].Action = nil }, want: "has no action selector"},
		{name: "missing lock", edit: func(j *Job) { j.Steps[0].Action.Lock = "a-0000000000000002" }, want: "missing lock"},
		{name: "action on run", edit: func(j *Job) {
			j.Steps[0] = Step{ID: "run", Kind: "run", Command: "true", Action: &ActionSelector{Lock: "a-0000000000000001"}}
		}, want: "incompatible action"},
		{name: "action on control", edit: func(j *Job) {
			j.Steps[0] = Step{ID: "wait", Kind: "wait-all", Action: &ActionSelector{Lock: "a-0000000000000001"}}
		}, want: "incompatible execution"},
		{name: "local identity mismatch", edit: func(j *Job) { j.Steps[0].Uses = "./actions/other" }, want: "does not match"},
		{name: "absolute path", edit: func(j *Job) { j.Actions[0].Path = "/actions/build" }, want: "invalid workspace identity"},
		{name: "backslash path", edit: func(j *Job) { j.Actions[0].Path = `actions\build` }, want: "invalid workspace identity"},
		{name: "dot segment", edit: func(j *Job) { j.Actions[0].Path = "actions/../build" }, want: "invalid workspace identity"},
		{name: "oversized uses", edit: func(j *Job) { j.Steps[0].Uses = "./" + strings.Repeat("a", 1023) }, want: "exceeds 1024 bytes"},
		{name: "unsupported source", edit: func(j *Job) { j.Actions[0].Source = "other" }, want: "unsupported source"},
		{name: "too many locks", edit: func(j *Job) { j.Actions = make([]ActionLock, 1025) }, want: "more than 1024"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := local()
			test.edit(&job)
			if err := job.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestV3WorkspaceRootActionAndSharedChildDAG(t *testing.T) {
	job := validJob()
	job.Schema = SchemaV3
	digest := "sha256:" + strings.Repeat("a", 64)
	job.Steps = []Step{{ID: "root", Kind: "uses", Uses: "./", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
	children := make(map[string]ActionSelector, 256)
	base := "manyletters/repository@v1"
	for i := range 256 {
		variant := []byte(base)
		for bit := 0; bit < 8; bit++ {
			if i&(1<<bit) != 0 {
				variant[bit] -= 'a' - 'A'
			}
		}
		children[string(variant)] = ActionSelector{Lock: "a-0000000000000002"}
	}
	job.Actions = []ActionLock{
		{ID: "a-0000000000000001", Source: "workspace", SourceDigest: digest, Children: children},
		{ID: "a-0000000000000002", Source: "github", Repository: "manyletters/repository", RequestedRef: "v1", Commit: strings.Repeat("b", 40), SourceDigest: "sha256:" + strings.Repeat("c", 64)},
	}
	if err := job.Validate(); err != nil {
		t.Fatalf("workspace-root action with shared-child DAG rejected: %v", err)
	}
}

func TestV3ActionLockGraphDepthAndCycles(t *testing.T) {
	chain := func(count int) Job {
		job := validJob()
		job.Schema = SchemaV3
		job.Steps = []Step{{ID: "root", Kind: "uses", Uses: "./action-" + leftPadHex(0), Action: &ActionSelector{Lock: "a-0000000000000000"}}}
		job.Actions = make([]ActionLock, count)
		for i := range count {
			id := "a-" + leftPadHex(i)
			path := "child"
			if i == 0 {
				path = "action-" + leftPadHex(i)
			}
			job.Actions[i] = ActionLock{ID: id, Source: "workspace", Path: path, SourceDigest: "sha256:" + strings.Repeat(string(rune('a'+i%6)), 64)}
			if i+1 < count {
				job.Actions[i].Children = map[string]ActionSelector{"./child": {Lock: "a-" + leftPadHex(i+1)}}
			}
		}
		return job
	}
	exact := chain(metadata.MaxNestedActionDepth)
	if err := exact.Validate(); err != nil {
		t.Fatalf("exact maximum depth rejected: %v", err)
	}
	over := chain(metadata.MaxNestedActionDepth + 1)
	if err := over.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds maximum depth") {
		t.Fatalf("overflow depth error = %v", err)
	}
	cyclic := chain(2)
	cyclic.Actions[1].Children = map[string]ActionSelector{"./action-" + leftPadHex(0): {Lock: cyclic.Actions[0].ID}}
	if err := cyclic.Validate(); err == nil || !strings.Contains(err.Error(), "contains a cycle") {
		t.Fatalf("cycle error = %v", err)
	}
}

func leftPadHex(value int) string {
	const hex = "0123456789abcdef"
	encoded := make([]byte, 16)
	for i := len(encoded) - 1; i >= 0; i-- {
		encoded[i] = hex[value&15]
		value >>= 4
	}
	return string(encoded)
}

func TestLegacySchemasRejectActionFields(t *testing.T) {
	encoded, err := Encode(validJob())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		strings.Replace(string(encoded), `"steps":`, `"actions":[],"steps":`, 1),
		strings.Replace(string(encoded), `"kind": "run",`, `"kind": "run","action":{"lock":"a-0000000000000001"},`, 1),
	} {
		if _, err := Decode([]byte(field)); err == nil || !strings.Contains(err.Error(), "not supported by legacy schemas") {
			t.Fatalf("Decode() error = %v, want closed legacy contract rejection", err)
		}
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

func TestV4ContainerContractAndLegacyRejection(t *testing.T) {
	job := validJob()
	job.Schema = SchemaV4
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &Container{Image: "node:24", Env: map[string]string{"NODE_ENV": "test"}, Ports: []string{"8080"}}
	job.Services = map[string]Container{"redis": {Image: "redis:7", Ports: []string{"6379:6379"}}}
	job.Steps = []Step{{ID: "local", Kind: "uses", Uses: "./actions/build", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
	job.Actions = []ActionLock{{ID: "a-0000000000000001", Source: "workspace", Path: "actions/build", SourceDigest: "sha256:" + strings.Repeat("a", 64)}}
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(encoded); err != nil {
		t.Fatal(err)
	}
	schemaSource, err := os.ReadFile(filepath.Join("..", "..", "schemas", "job-plan-v4.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument, planDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &planDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(SchemaV4, schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(SchemaV4)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(planDocument); err != nil {
		t.Fatalf("v4 plan does not validate against schema: %v", err)
	}
	job.Actions = nil
	job.Steps = []Step{{ID: "step-1", Kind: "run", Command: "true"}}
	for _, schema := range []string{SchemaV1, SchemaV2, SchemaV3} {
		job.Schema = schema
		if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "does not support containers") {
			t.Fatalf("Validate(%s) error = %v", schema, err)
		}
	}
}

func TestV5PrerequisiteOutputProjectionContractAndLegacyRejection(t *testing.T) {
	digest := "sha256:" + strings.Repeat("4", 64)
	job := validJob()
	job.Schema = SchemaV5
	job.Dependencies = []string{"gha-delegated-first", "gha-delegated-second"}
	job.NeedSources = map[string][]NeedSource{
		"delegated": {
			{StepKey: "gha-delegated-first", PlanDigest: digest},
			{StepKey: "gha-delegated-second", PlanDigest: digest},
		},
	}
	job.NeedOutputs = map[string][]NeedOutput{"delegated": {}}
	job.Steps = []Step{{ID: "local", Kind: "uses", Uses: "./actions/build"}}
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(encoded); err != nil {
		t.Fatal(err)
	}
	schemaSource, err := os.ReadFile(filepath.Join("..", "..", "schemas", "job-plan-v5.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument, planDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &planDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(SchemaV5, schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(SchemaV5)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(planDocument); err != nil {
		t.Fatalf("v5 plan does not validate against schema: %v", err)
	}

	job.Steps = []Step{{ID: "step-1", Kind: "run", Command: "true"}}
	job.NeedOutputs["delegated"] = []NeedOutput{{Name: "result", StepKey: "gha-delegated-first", Output: "internal"}}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	job.Schema = SchemaV4
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "does not support prerequisite output projections") {
		t.Fatalf("Validate() error = %v, want legacy projection rejection", err)
	}
	job.Schema = SchemaV5
	job.NeedOutputs["delegated"][0].StepKey = "gha-other"
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "selects unknown producer") {
		t.Fatalf("Validate() error = %v, want producer binding rejection", err)
	}
}

func TestV6GitHubWorkflowTokenContractAndSchema(t *testing.T) {
	job := validJob()
	job.Schema = SchemaV6
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &GitHubToken{Permissions: map[string]string{"contents": "read", "pull_requests": "write"}}
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	schemaSource, err := os.ReadFile(filepath.Join("..", "..", "schemas", "job-plan-v6.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument, planDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &planDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(SchemaV6, schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(SchemaV6)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(planDocument); err != nil {
		t.Fatalf("v6 plan does not validate against schema: %v", err)
	}
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{name: "other provider", edit: func(document map[string]any) {
			document["event"].(map[string]any)["provider"] = "cursor-origin"
		}},
		{name: "missing repository", edit: func(document map[string]any) {
			delete(document["event"].(map[string]any), "repository")
		}},
		{name: "malformed repository", edit: func(document map[string]any) {
			document["event"].(map[string]any)["repository"] = "buildkite"
		}},
		{name: "dot repository component", edit: func(document map[string]any) {
			document["event"].(map[string]any)["repository"] = "buildkite/.."
		}},
	} {
		t.Run("schema rejects "+test.name, func(t *testing.T) {
			encodedDocument, err := json.Marshal(planDocument)
			if err != nil {
				t.Fatal(err)
			}
			var changed map[string]any
			if err := json.Unmarshal(encodedDocument, &changed); err != nil {
				t.Fatal(err)
			}
			test.edit(changed)
			if err := schema.Validate(changed); err == nil {
				t.Fatalf("v6 schema accepted token plan with %s", test.name)
			}
		})
	}

	for _, test := range []struct {
		name string
		edit func(*Job)
		want string
	}{
		{name: "missing capability", edit: func(j *Job) { j.RequiredCapabilities = []string{} }, want: "declared together"},
		{name: "missing token", edit: func(j *Job) { j.GitHubToken = nil }, want: "declared together"},
		{name: "legacy schema", edit: func(j *Job) { j.Schema = SchemaV5 }, want: "does not support GitHub workflow tokens"},
		{name: "unknown permission", edit: func(j *Job) { j.GitHubToken.Permissions = map[string]string{"administration": "write"} }, want: "unsupported permission"},
		{name: "invalid access", edit: func(j *Job) { j.GitHubToken.Permissions = map[string]string{"contents": "admin"} }, want: "unsupported permission"},
		{name: "reserved ambient secret", edit: func(j *Job) {
			j.RequiredCapabilities = []string{"provider-token-write", "secrets"}
			j.RequiredSecrets = []string{"GITHUB_TOKEN"}
		}, want: "scoped workflow token contract"},
		{name: "other provider", edit: func(j *Job) { j.Event.Provider = "cursor-origin" }, want: "valid github.com event repository"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := job
			changed.RequiredCapabilities = append([]string(nil), job.RequiredCapabilities...)
			changed.RequiredSecrets = append([]string(nil), job.RequiredSecrets...)
			permissions := map[string]string{}
			for name, access := range job.GitHubToken.Permissions {
				permissions[name] = access
			}
			changed.GitHubToken = &GitHubToken{Permissions: permissions}
			test.edit(&changed)
			if err := changed.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestV7DefaultQueueContractAndSchema(t *testing.T) {
	requiresMise := false
	job := validJob()
	job.Schema = SchemaV7
	job.Target.Queue = ""
	job.RequiresMise = &requiresMise
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"queue"`) {
		t.Fatalf("default-targeted plan contains a queue:\n%s", encoded)
	}

	schemaSource, err := os.ReadFile(filepath.Join("..", "..", "schemas", "job-plan-v7.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument, planDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &planDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(SchemaV7, schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(SchemaV7)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(planDocument); err != nil {
		t.Fatalf("v7 default-targeted plan does not validate against schema: %v", err)
	}

	job.Target.Queue = "explicit"
	if err := job.Validate(); err != nil {
		t.Fatalf("v7 explicit queue validation error = %v", err)
	}
	job.Target.Queue = "invalid queue"
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "invalid target queue") {
		t.Fatalf("v7 malformed queue error = %v", err)
	}
	for _, legacy := range []string{SchemaV1, SchemaV2, SchemaV3, SchemaV4, SchemaV5, SchemaV6} {
		job := validJob()
		job.Schema = legacy
		job.Target.Queue = ""
		if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "requires a target queue") {
			t.Fatalf("legacy schema %q empty queue error = %v", legacy, err)
		}
	}
}

func TestContainerPortGrammarMatchesSchema(t *testing.T) {
	schemaSource, err := os.ReadFile(filepath.Join("..", "..", "schemas", "job-plan-v4.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(SchemaV4, schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(SchemaV4)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		port  string
		valid bool
	}{
		{"0", false}, {"1", true}, {"65535", true}, {"65536", false},
		{"8080:80", true}, {"0:80", false}, {"8080:65536", false},
		{"53/udp", true}, {"443/tcp", true}, {"443/sctp", false},
		{"08", false}, {"+80", false}, {"80/udp/tcp", false},
	} {
		t.Run(test.port, func(t *testing.T) {
			goValid := validateContainer("node:24", nil, []string{test.port}) == nil
			job := validJob()
			job.Schema = SchemaV4
			job.RequiredCapabilities = []string{"docker", "network"}
			job.Container = &Container{Image: "node:24", Ports: []string{test.port}}
			encoded, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			var document any
			if err := json.Unmarshal(encoded, &document); err != nil {
				t.Fatal(err)
			}
			schemaValid := schema.Validate(document) == nil
			if goValid != test.valid || schemaValid != test.valid {
				t.Fatalf("Go valid = %t, schema valid = %t, want %t", goValid, schemaValid, test.valid)
			}
		})
	}
}
