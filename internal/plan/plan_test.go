package plan

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestDecodePreservesPlanContract(t *testing.T) {
	fixture := validJob()
	fixture.Event.HeadRef = "feature/head-ref"
	fixture.Event.BaseRef = "main"
	fixture.OIDC = &OIDCConfiguration{Claims: []string{"organization_id"}, AWSSessionTags: []string{"pipeline_id"}, SubjectClaim: "pipeline_id"}
	source, err := Encode(fixture)
	if err != nil {
		t.Fatal(err)
	}
	job, err := Decode(source)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if job.Compiler.DistributionDigest == "" || job.Workflow.Name != "CI" || job.Event.PayloadDigest == "" || job.Event.HeadRef != "feature/head-ref" || job.Event.BaseRef != "main" || job.Target.StepKey != "gha-test" || !slices.Equal(job.OIDC.Claims, []string{"organization_id"}) {
		t.Fatalf("decoded fixture lost trust bindings: %#v", job)
	}
	validateJobPlanSchema(t, source)
}

func TestCheckoutInputsRoundTripDeterministically(t *testing.T) {
	job := validJob()
	lockID := "a-0000000000000001"
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"network", "provider-token-read"}
	job.Steps = []Step{{
		ID: "checkout", Kind: "uses", Uses: "actions/checkout@v7", Action: &ActionSelector{Lock: lockID},
		With: map[string]string{
			"filter": "blob:none", "lfs": "true", "path": "sources/application",
			"sparse-checkout": "src\ndocs\n", "sparse-checkout-cone-mode": "false",
		},
	}}
	job.Actions = []ActionLock{{
		ID: lockID, Source: "github", Repository: "actions/checkout", RequestedRef: "v7",
		Commit: strings.Repeat("a", 40), SourceDigest: "sha256:" + strings.Repeat("b", 64),
	}}
	first, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("checkout plan encoding is nondeterministic")
	}
	decoded, err := Decode(first)
	if err != nil || !maps.Equal(decoded.Steps[0].With, job.Steps[0].With) || decoded.Event.Repository != job.Event.Repository {
		t.Fatalf("decoded checkout plan = %#v, %v", decoded, err)
	}
}

func TestRemoteWorkflowSourceRoundTripAndValidation(t *testing.T) {
	job := validJob()
	job.Workflow.Path = "owner/repository/.github/workflows/ci.yml@v1"
	job.Workflow.Remote = &RemoteWorkflowSource{
		Repository: "owner/repository", RequestedRef: "v1", Commit: strings.Repeat("a", 40), SourceDigest: "sha256:" + strings.Repeat("b", 64),
	}
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	validateJobPlanSchema(t, encoded)
	decoded, err := Decode(encoded)
	if err != nil || !reflect.DeepEqual(decoded.Workflow, job.Workflow) {
		t.Fatalf("Decode() remote workflow = %#v, %v", decoded.Workflow, err)
	}

	tests := []struct {
		name string
		edit func(*Job)
		want string
	}{
		{name: "repository case", edit: func(job *Job) { job.Workflow.Remote.Repository = "Owner/repository" }, want: "invalid immutable source provenance"},
		{name: "path repository", edit: func(job *Job) { job.Workflow.Path = "other/repository/.github/workflows/ci.yml@v1" }, want: "path does not match"},
		{name: "path ref", edit: func(job *Job) { job.Workflow.Path = "owner/repository/.github/workflows/ci.yml@v2" }, want: "path does not match"},
		{name: "nested path", edit: func(job *Job) { job.Workflow.Path = "owner/repository/.github/workflows/nested/ci.yml@v1" }, want: "path does not match"},
		{name: "commit", edit: func(job *Job) { job.Workflow.Remote.Commit = strings.Repeat("A", 40) }, want: "invalid immutable source provenance"},
		{name: "tree digest", edit: func(job *Job) { job.Workflow.Remote.SourceDigest = "sha256:invalid" }, want: "invalid immutable source provenance"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := job
			remote := *job.Workflow.Remote
			invalid.Workflow.Remote = &remote
			test.edit(&invalid)
			if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCallGuardPlanAndSchemaRoundTrip(t *testing.T) {
	job := validJob()
	digest := "sha256:" + strings.Repeat("1", 64)
	job.Dependencies = []string{"gha-prepare"}
	job.DeferredInputs = map[string]DeferredInput{
		"subject": {
			Sources: []NeedSource{{StepKey: "gha-prepare", PlanDigest: digest}},
			Outputs: []NeedOutput{{Name: "value", StepKey: "gha-prepare", Output: "subject"}},
		},
	}
	job.CallGuards = []CallGuard{{
		Condition: "always() && needs.prepare.result == 'failure' && needs.prepare.outputs.ready && inputs.enabled",
		Inputs:    map[string]any{"enabled": true},
		NeedSources: map[string][]NeedSource{
			"prepare": {{StepKey: "gha-prepare", PlanDigest: digest}},
		},
		NeedOutputs: map[string][]NeedOutput{
			"prepare": {{Name: "ready", StepKey: "gha-prepare", Output: "ready"}},
		},
		DeferredInputs: map[string]DeferredInput{
			"subject": {
				Sources: []NeedSource{{StepKey: "gha-prepare", PlanDigest: digest}},
				Outputs: []NeedOutput{{Name: "value", StepKey: "gha-prepare", Output: "subject"}},
			},
		},
	}}
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	validateJobPlanSchema(t, encoded)
	decoded, err := Decode(encoded)
	if err != nil || !reflect.DeepEqual(decoded.CallGuards, job.CallGuards) || !reflect.DeepEqual(decoded.DeferredInputs, job.DeferredInputs) {
		t.Fatalf("Decode() deferred inputs/call guards = %#v / %#v, %v", decoded.DeferredInputs, decoded.CallGuards, err)
	}

	job.CallGuards[0].Condition = "matrix.os"
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), `call condition context "matrix" is unsupported`) {
		t.Fatalf("Validate() unavailable call context error = %v", err)
	}
	job.CallGuards[0].Condition = "true"
	job.CallGuards[0].NeedSources["prepare"][0].PlanDigest = "sha256:tampered"
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "producer identity") {
		t.Fatalf("Validate() tampered call producer error = %v", err)
	}
}

func TestEventHeadRefSizeLimit(t *testing.T) {
	job := validJob()
	job.Event.HeadRef = strings.Repeat("a", 1024)
	encoded, err := Encode(job)
	if err != nil {
		t.Fatalf("Encode() maximum head_ref error = %v", err)
	}
	validateJobPlanSchema(t, encoded)

	oversized := strings.Repeat("a", 1025)
	job.Event.HeadRef = oversized
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "event identity exceeds its size limit") {
		t.Fatalf("Validate() oversized head_ref error = %v", err)
	}
	if _, err := Encode(job); err == nil || !strings.Contains(err.Error(), "event identity exceeds its size limit") {
		t.Fatalf("Encode() oversized head_ref error = %v", err)
	}
	encoded = []byte(strings.Replace(string(encoded), strings.Repeat("a", 1024), oversized, 1))
	if _, err := Decode(encoded); err == nil || !strings.Contains(err.Error(), "event identity exceeds its size limit") {
		t.Fatalf("Decode() oversized head_ref error = %v", err)
	}
}

func TestEventBaseRefSizeLimit(t *testing.T) {
	job := validJob()
	job.Event.BaseRef = strings.Repeat("a", 1024)
	encoded, err := Encode(job)
	if err != nil {
		t.Fatalf("Encode() maximum base_ref error = %v", err)
	}
	validateJobPlanSchema(t, encoded)

	job.Event.BaseRef = strings.Repeat("a", 1025)
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "event identity exceeds its size limit") {
		t.Fatalf("Validate() oversized base_ref error = %v", err)
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
		{name: "unknown schema", source: strings.Replace(string(encoded), Schema, "unsupported-schema", 1), want: "unsupported job plan schema"},
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

func TestJobContinueOnErrorContract(t *testing.T) {
	job := validJob()
	job.ContinueOnError = true
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.ContinueOnError {
		t.Fatalf("decoded plan = %#v, want continue_on_error", decoded)
	}
	validateJobPlanSchema(t, encoded)
}

func TestStepControlExpressionContract(t *testing.T) {
	job := validJob()
	job.Steps[0].ContinueOnErrorExpression = "${{ matrix.experimental }}"
	job.Steps[0].TimeoutMinutesExpression = "${{ matrix.timeout }}"
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	step := decoded.Steps[0]
	if step.ContinueOnErrorExpression != "${{ matrix.experimental }}" || step.TimeoutMinutesExpression != "${{ matrix.timeout }}" {
		t.Fatalf("decoded step = %#v", step)
	}
	validateJobPlanSchema(t, encoded)

	job.Steps[0].ContinueOnError = true
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "both literal and expression continue_on_error") {
		t.Fatalf("Validate() mixed continue-on-error error = %v", err)
	}
	job = validJob()
	job.Steps[0].TimeoutMinutesExpression = "5"
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "expression must be complete") {
		t.Fatalf("Validate() incomplete expression error = %v", err)
	}
}

func TestDecodeRejectsBothStepControlWireKeysAtZeroValues(t *testing.T) {
	for _, test := range []struct {
		literal, expression string
		value               any
	}{
		{literal: "continue_on_error", expression: "continue_on_error_expression", value: false},
		{literal: "timeout_minutes", expression: "timeout_minutes_expression", value: 0},
		{literal: "continue_on_error", expression: "CONTINUE_ON_ERROR_EXPRESSION", value: false},
		{literal: "timeout_minutes", expression: "TIMEOUT_MINUTES_EXPRESSION", value: 0},
		{literal: "continue_on_error", expression: "continue_on_error_expreſſion", value: false},
	} {
		job := validJob()
		encoded, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		step := wire["steps"].([]any)[0].(map[string]any)
		step[test.literal] = test.value
		step[test.expression] = "${{ true }}"
		encoded, err = json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(encoded); err == nil || !strings.Contains(err.Error(), "both") {
			t.Errorf("Decode() %s error = %v", test.literal, err)
		}
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

func TestRequiredSecretsRoundTripAsNamesOnly(t *testing.T) {
	job := validJob()
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"HOMEBREW_TAP_GITHUB_TOKEN"}
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"required_secrets"`) || !strings.Contains(string(encoded), `"HOMEBREW_TAP_GITHUB_TOKEN"`) || strings.Contains(string(encoded), "secret-value") {
		t.Fatalf("Encode() = %s", encoded)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoded.RequiredSecrets, job.RequiredSecrets) {
		t.Fatalf("required secrets = %#v", decoded.RequiredSecrets)
	}
}

func TestSecretMappingsRoundTripAsNamesOnlyAndValidateAuthority(t *testing.T) {
	job := validJob()
	job.RequiredCapabilities = []string{"secrets"}
	job.RequiredSecrets = []string{"ORIGINAL"}
	job.SecretMappings = map[string]string{"ALIAS": "ORIGINAL"}
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	validateJobPlanSchema(t, encoded)
	if !strings.Contains(string(encoded), `"secret_mappings"`) || !strings.Contains(string(encoded), `"ALIAS": "ORIGINAL"`) || strings.Contains(string(encoded), "secret-value") {
		t.Fatalf("Encode() = %s", encoded)
	}
	decoded, err := Decode(encoded)
	if err != nil || !reflect.DeepEqual(decoded.SecretMappings, job.SecretMappings) {
		t.Fatalf("Decode() mappings = %#v, error = %v", decoded.SecretMappings, err)
	}
	for _, test := range []struct {
		name string
		edit func(*Job)
		want string
	}{
		{name: "missing source", edit: func(j *Job) { j.SecretMappings["ALIAS"] = "OTHER" }, want: "undeclared source"},
		{name: "reserved alias", edit: func(j *Job) { j.SecretMappings = map[string]string{"GITHUB_TOKEN": "ORIGINAL"} }, want: "invalid secret mapping"},
		{name: "missing capability", edit: func(j *Job) { j.RequiredCapabilities = nil }, want: "secrets capability"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := job
			changed.RequiredCapabilities = append([]string(nil), job.RequiredCapabilities...)
			changed.SecretMappings = maps.Clone(job.SecretMappings)
			test.edit(&changed)
			if err := changed.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestActionLocksRoundTripAndValidateAgainstSchema(t *testing.T) {
	job := validJob()
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Steps = []Step{{ID: "local", Kind: "uses", Uses: "./actions/build", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
	job.Actions = []ActionLock{{
		ID: "a-0000000000000001", Source: "workspace", Path: "actions/build",
		SourceDigest: "sha256:" + strings.Repeat("a", 64), DockerImage: "busybox@sha256:" + strings.Repeat("b", 64),
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
		t.Fatalf("encoding is not deterministic\nfirst: %s\nsecond: %s", encoded, reencoded)
	}
	validateJobPlanSchema(t, encoded)

	for _, test := range []struct {
		name string
		edit func(*Job)
		want string
	}{
		{name: "invalid image", edit: func(j *Job) { j.Actions[0].DockerImage = "INVALID" }, want: "invalid Docker image"},
		{name: "missing docker capability", edit: func(j *Job) { j.RequiredCapabilities = []string{"network"} }, want: "require docker capability"},
		{name: "missing network capability", edit: func(j *Job) { j.RequiredCapabilities = []string{"docker"} }, want: "require network capability"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := job
			changed.Actions = append([]ActionLock(nil), job.Actions...)
			changed.RequiredCapabilities = append([]string(nil), job.RequiredCapabilities...)
			test.edit(&changed)
			if err := changed.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRequiresMiseRoundTripAndSchema(t *testing.T) {
	requiresMise := false
	job := validJob()
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
	validateJobPlanSchema(t, encoded)
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

func TestRuntimeDistributionRoundTripAndSchema(t *testing.T) {
	requiresMise := false
	job := validJob()
	job.Compiler.Version = "dev"
	job.Runtime = &Runtime{DistributionDigest: "sha256:" + strings.Repeat("c", 64)}
	job.RequiresMise = &requiresMise
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.RuntimeDistributionDigest(); got != job.Runtime.DistributionDigest {
		t.Fatalf("RuntimeDistributionDigest() = %q, want %q", got, job.Runtime.DistributionDigest)
	}

	var planDocument any
	if err := json.Unmarshal(encoded, &planDocument); err != nil {
		t.Fatal(err)
	}
	schema := compileJobPlanSchema(t)
	if err := schema.Validate(planDocument); err != nil {
		t.Fatalf("plan does not validate against schema: %v", err)
	}
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{name: "run step without command", edit: func(document map[string]any) {
			delete(document["steps"].([]any)[0].(map[string]any), "command")
		}},
		{name: "unknown GitHub permission", edit: func(document map[string]any) {
			document["required_capabilities"] = []any{"provider-token-write"}
			document["github_token"] = map[string]any{"workflow": "ci.yml", "permissions": map[string]any{"future_permission": "read"}}
			document["event"].(map[string]any)["repository"] = "buildkite/buildkite-gha"
		}},
		{name: "incomplete GitHub action lock", edit: func(document map[string]any) {
			document["actions"] = []any{map[string]any{
				"id":            "a-0000000000000001",
				"source":        "github",
				"source_digest": "sha256:" + strings.Repeat("a", 64),
			}}
		}},
		{name: "invalid container", edit: func(document map[string]any) {
			document["container"] = map[string]any{"image": "INVALID IMAGE", "ports": []any{"65536"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var invalid map[string]any
			if err := json.Unmarshal(encoded, &invalid); err != nil {
				t.Fatal(err)
			}
			test.edit(invalid)
			if err := schema.Validate(invalid); err == nil {
				t.Fatal("schema accepted an invalid document")
			}
		})
	}

	job.Runtime = nil
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "runtime distribution digest is required") {
		t.Fatalf("Validate() missing runtime error = %v", err)
	}
	job.Runtime = &Runtime{DistributionDigest: "invalid"}
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "runtime distribution digest is required") {
		t.Fatalf("Validate() invalid runtime error = %v", err)
	}
	job.Runtime = &Runtime{DistributionDigest: "sha256:" + strings.Repeat("c", 64)}
	job.Compiler.Version = strings.Repeat("v", 257)
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "compiler version") {
		t.Fatalf("Validate() oversized compiler version error = %v", err)
	}
}

func TestRemoteNestedActionLocks(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	commit := strings.Repeat("c", 40)
	job := validJob()
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

func TestActionLockValidationMatrix(t *testing.T) {
	remote := func() Job {
		job := validJob()
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

func TestLocalChildPreservesParentIdentity(t *testing.T) {
	job := validJob()
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

func TestRemoteCompositeLocalChildUsesWorkspaceIdentity(t *testing.T) {
	job := validJob()
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

func TestActionSelectorsAndPathsRejectInvalidValues(t *testing.T) {
	local := func() Job {
		job := validJob()
		requiresMise := true
		job.RequiresMise = &requiresMise
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

func TestWorkspaceRootActionAndSharedChildDAG(t *testing.T) {
	job := validJob()
	digest := "sha256:" + strings.Repeat("a", 64)
	job.Steps = []Step{{ID: "root", Kind: "uses", Uses: "./", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
	children := make(map[string]ActionSelector, 256)
	base := "manyletters/repository@v1"
	for i := range 256 {
		variant := []byte(base)
		for bit := range 8 {
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

func TestActionLockGraphDepthAndCycles(t *testing.T) {
	chain := func(count int) Job {
		job := validJob()
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

func validJob() Job {
	return Job{
		Schema:               Schema,
		Compiler:             Compiler{Version: "0.0.0-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Runtime:              &Runtime{DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Workflow:             Workflow{Path: ".github/workflows/ci.yml", Name: "CI", Digest: "sha256:" + strings.Repeat("0", 64), LogicalJobID: "test"},
		Event:                Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:               Target{StepKey: "gha-test", Queue: "ubuntu-latest"},
		RequiredCapabilities: []string{},
		Steps:                []Step{{ID: "step-1", Kind: "run", Command: "true"}},
		RequiresMise:         new(bool),
	}
}

func compileJobPlanSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "..", "schemas", "job-plan.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(source, &document); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(Schema, document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(Schema)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func validateJobPlanSchema(t *testing.T, encoded []byte) {
	t.Helper()
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if err := compileJobPlanSchema(t).Validate(document); err != nil {
		t.Fatalf("plan does not validate against schema: %v", err)
	}
}

func TestContainerContract(t *testing.T) {
	job := validJob()
	job.RequiredCapabilities = []string{"docker", "network"}
	job.Container = &Container{Image: "node:24", Env: map[string]string{"NODE_ENV": "test"}, Ports: []string{"8080"}, Volumes: []string{"cache:/cache:ro"}, Options: "--cpus 2"}
	job.Services = map[string]ServiceContainer{"database": {
		Image:       "postgres:16",
		Credentials: &ContainerCredentials{Username: "user", Password: "password"},
		Env:         map[string]string{"POSTGRES_DB": "app"},
		Ports:       []string{"5432:5432"},
		Volumes:     []string{"database:/data"},
		Options:     "--health-retries 5",
		Command:     "postgres -c fsync=off",
		Entrypoint:  "docker-entrypoint.sh",
	}}
	job.ServiceOrder = []string{"database"}
	job.Steps = []Step{{ID: "local", Kind: "uses", Uses: "./actions/build", Action: &ActionSelector{Lock: "a-0000000000000001"}}}
	job.Actions = []ActionLock{{ID: "a-0000000000000001", Source: "workspace", Path: "actions/build", SourceDigest: "sha256:" + strings.Repeat("a", 64)}}
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Container, job.Container) || !reflect.DeepEqual(decoded.Services, job.Services) {
		t.Fatalf("decoded containers = %#v, %#v", decoded.Container, decoded.Services)
	}
	validateJobPlanSchema(t, encoded)
	wire, err := json.Marshal(struct {
		Container *Container                  `json:"container"`
		Services  map[string]ServiceContainer `json:"services"`
	}{Container: job.Container, Services: job.Services})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"container":{"image":"node:24","env":{"NODE_ENV":"test"},"ports":["8080"],"volumes":["cache:/cache:ro"],"options":"--cpus 2"},"services":{"database":{"image":"postgres:16","credentials":{"username":"user","password":"password"},"env":{"POSTGRES_DB":"app"},"ports":["5432:5432"],"volumes":["database:/data"],"options":"--health-retries 5","command":"postgres -c fsync=off","entrypoint":"docker-entrypoint.sh"}}}`
	if string(wire) != want {
		t.Fatalf("encoded containers = %s, want %s", wire, want)
	}
}

func TestContainerModelFields(t *testing.T) {
	tests := []struct {
		name   string
		typeOf reflect.Type
		fields []string
	}{
		{name: "job", typeOf: reflect.TypeFor[Container](), fields: []string{"Image:image", "Env:env,omitempty", "Ports:ports,omitempty", "Volumes:volumes,omitempty", "Options:options,omitempty"}},
		{name: "service", typeOf: reflect.TypeFor[ServiceContainer](), fields: []string{"Image:image", "Credentials:credentials,omitempty", "Env:env,omitempty", "Ports:ports,omitempty", "Volumes:volumes,omitempty", "Options:options,omitempty", "Command:command,omitempty", "Entrypoint:entrypoint,omitempty"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got []string
			for field := range test.typeOf.Fields() {
				got = append(got, field.Name+":"+string(field.Tag.Get("json")))
			}
			if !slices.Equal(got, test.fields) {
				t.Fatalf("fields = %#v, want %#v", got, test.fields)
			}
		})
	}
}

func TestJobContainerPlanRejectsUnsafeOptionsAndVolumes(t *testing.T) {
	for name, container := range map[string]Container{
		"privileged":         {Image: "node:24", Options: "--privileged"},
		"network":            {Image: "node:24", Options: "--network=host"},
		"unbounded memory":   {Image: "node:24", Options: "--memory-swap -1"},
		"repeated option":    {Image: "node:24", Options: "--cpus 1 --cpus=2"},
		"bind mount":         {Image: "node:24", Volumes: []string{"/tmp:/data"}},
		"runtime overlap":    {Image: "node:24", Volumes: []string{"cache:/__buildkite-gha/runtime"}},
		"unsupported volume": {Image: "node:24", Volumes: []string{"cache:/data:z"}},
	} {
		t.Run(name, func(t *testing.T) {
			job := validJob()
			job.RequiredCapabilities = []string{"docker", "network"}
			job.Container = &container
			if err := job.Validate(); err == nil {
				t.Fatal("Validate() accepted unsafe job container control")
			}
		})
	}
}

func TestPrerequisiteOutputProjectionContract(t *testing.T) {
	digest := "sha256:" + strings.Repeat("4", 64)
	job := validJob()
	requiresMise := true
	job.RequiresMise = &requiresMise
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
	validateJobPlanSchema(t, encoded)

	job.Steps = []Step{{ID: "step-1", Kind: "run", Command: "true"}}
	job.NeedOutputs["delegated"] = []NeedOutput{{Name: "result", StepKey: "gha-delegated-first", Output: "internal"}}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	job.NeedOutputs["delegated"][0].StepKey = "gha-other"
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "selects unknown producer") {
		t.Fatalf("Validate() error = %v, want producer binding rejection", err)
	}
}

func TestGitHubWorkflowPolicyFilename(t *testing.T) {
	valid255 := strings.Repeat("a", 251) + ".yml"
	for _, test := range []struct {
		path string
		want string
	}{
		{path: ".github/workflows/ci.yml", want: "ci.yml"},
		{path: "./.github/workflows/release.yaml", want: "release.yaml"},
		{path: ".github/workflows/Release_1.2-test.yml", want: "Release_1.2-test.yml"},
		{path: ".github/workflows/" + valid255, want: valid255},
	} {
		got, err := GitHubWorkflowPolicyFilename(test.path)
		if err != nil || got != test.want {
			t.Errorf("GitHubWorkflowPolicyFilename(%q) = %q, %v; want %q", test.path, got, err, test.want)
		}
	}

	for _, path := range []string{
		"", "ci.yml", "/.github/workflows/ci.yml", ".github/workflows/nested/ci.yml",
		".github/workflows/../ci.yml", ".github/workflows/.ci.yml", ".github/workflows/ci.json",
		".github/workflows/ci yml", ".github/workflows/" + strings.Repeat("a", 252) + ".yml",
	} {
		if _, err := GitHubWorkflowPolicyFilename(path); err == nil {
			t.Errorf("GitHubWorkflowPolicyFilename(%q) succeeded", path)
		}
	}
}

func TestValidateGitHubWorkflowAccessTokenPermissions(t *testing.T) {
	if err := ValidateGitHubWorkflowAccessTokenPermissions(map[string]string{"contents": "read", "pull_requests": "write"}); err != nil {
		t.Fatal(err)
	}
	readAll := map[string]string{
		"actions": "read", "artifact_metadata": "read", "attestations": "read", "checks": "read", "contents": "read",
		"deployments": "read", "discussions": "read", "issues": "read", "packages": "read", "pages": "read",
		"pull_requests": "read", "security_events": "read", "statuses": "read",
	}
	if err := ValidateGitHubWorkflowAccessTokenPermissions(readAll); err != nil {
		t.Fatalf("ValidateGitHubWorkflowAccessTokenPermissions(read-all) = %v", err)
	}
	for _, permissions := range []map[string]string{
		nil,
		{"models": "read"},
		{"repository_projects": "read"},
		{"id_token": "read"},
		{"code_quality": "read"},
		{"metadata": "read"},
		{"vulnerability_alerts": "read"},
		{"contents": "admin"},
	} {
		if err := ValidateGitHubWorkflowAccessTokenPermissions(permissions); err == nil {
			t.Errorf("ValidateGitHubWorkflowAccessTokenPermissions(%#v) succeeded", permissions)
		}
	}
}

func TestGitHubWorkflowTokenContractAndSchema(t *testing.T) {
	job := validJob()
	job.Event.Repository = "buildkite/buildkite-gha"
	job.RequiredCapabilities = []string{"provider-token-write"}
	job.GitHubToken = &GitHubToken{Workflow: "ci.yml", Permissions: map[string]string{"contents": "read", "pull_requests": "write"}, Aliases: []string{"TOKEN_ALIAS"}}
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	var planDocument any
	if err := json.Unmarshal(encoded, &planDocument); err != nil {
		t.Fatal(err)
	}
	schema := compileJobPlanSchema(t)
	if err := schema.Validate(planDocument); err != nil {
		t.Fatalf("plan does not validate against schema: %v", err)
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
				t.Fatalf("schema accepted token plan with %s", test.name)
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
		{name: "missing workflow", edit: func(j *Job) { j.GitHubToken.Workflow = "" }, want: "simple .yml or .yaml filename"},
		{name: "unknown permission", edit: func(j *Job) { j.GitHubToken.Permissions = map[string]string{"administration": "write"} }, want: "unsupported permission"},
		{name: "invalid access", edit: func(j *Job) { j.GitHubToken.Permissions = map[string]string{"contents": "admin"} }, want: "unsupported permission"},
		{name: "reserved alias", edit: func(j *Job) { j.GitHubToken.Aliases = []string{"GITHUB_TOKEN"} }, want: "invalid or repeated GitHub token alias"},
		{name: "unsorted aliases", edit: func(j *Job) { j.GitHubToken.Aliases = []string{"Z_TOKEN", "A_TOKEN"} }, want: "aliases must be sorted"},
		{name: "ordinary alias overlap", edit: func(j *Job) {
			j.RequiredCapabilities = []string{"provider-token-write", "secrets"}
			j.RequiredSecrets = []string{"TOKEN_ALIAS"}
		}, want: "overlaps ordinary secret authority"},
		{name: "reserved ambient secret", edit: func(j *Job) {
			j.RequiredCapabilities = []string{"provider-token-write", "secrets"}
			j.RequiredSecrets = []string{"github_token"}
		}, want: "scoped workflow token contract"},
		{name: "other provider", edit: func(j *Job) { j.Event.Provider = "cursor-origin" }, want: "valid github.com event repository"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := job
			changed.RequiredCapabilities = append([]string(nil), job.RequiredCapabilities...)
			changed.RequiredSecrets = append([]string(nil), job.RequiredSecrets...)
			permissions := map[string]string{}
			maps.Copy(permissions, job.GitHubToken.Permissions)
			changed.GitHubToken = &GitHubToken{Workflow: job.GitHubToken.Workflow, Permissions: permissions, Aliases: append([]string(nil), job.GitHubToken.Aliases...)}
			test.edit(&changed)
			if err := changed.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDefaultQueueContractAndSchema(t *testing.T) {
	job := validJob()
	job.Target.Queue = ""
	encoded, err := Encode(job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"queue"`) {
		t.Fatalf("default-targeted plan contains a queue:\n%s", encoded)
	}

	validateJobPlanSchema(t, encoded)

	job.Target.Queue = "explicit"
	if err := job.Validate(); err != nil {
		t.Fatalf("explicit queue validation error = %v", err)
	}
	job.Target.Queue = "invalid queue"
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "invalid target queue") {
		t.Fatalf("malformed queue error = %v", err)
	}
}

func TestContainerPortGrammarMatchesSchema(t *testing.T) {
	schema := compileJobPlanSchema(t)

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
			goValid := validateContainer(Container{Image: "node:24", Ports: []string{test.port}}) == nil
			job := validJob()
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

func TestContainerImageGrammarMatchesSchema(t *testing.T) {
	schema := compileJobPlanSchema(t)
	for _, test := range []struct {
		image string
		valid bool
	}{
		{"redis:7", true},
		{"ghcr.io/example/service:latest", true},
		{"localhost:5000/private/service:latest", true},
		{"127.0.0.1:65535/private/service", true},
		{"[::1]:5000/private/service", true},
		{"localhost:0/private/service", false},
		{"localhost:65536/private/service", false},
		{"LOCALHOST:5000/private/service", false},
		{"${{ matrix.image }}", false},
		{"${{ secrets.IMAGE }}", false},
		{"${{ needs.build.outputs.image }}", false},
	} {
		t.Run(test.image, func(t *testing.T) {
			job := validJob()
			job.RequiredCapabilities = []string{"docker", "network"}
			job.Container = &Container{Image: test.image}
			encoded, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			var document any
			if err := json.Unmarshal(encoded, &document); err != nil {
				t.Fatal(err)
			}
			goValid := job.Validate() == nil
			schemaValid := schema.Validate(document) == nil
			if goValid != test.valid || schemaValid != test.valid {
				t.Fatalf("Go valid = %t, schema valid = %t, want %t", goValid, schemaValid, test.valid)
			}
		})
	}
}
