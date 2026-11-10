package program

import (
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/expression"
)

func TestInventoryCompositeShellAuthorityUsesCallerScope(t *testing.T) {
	action := Action{Runtime: "composite", Steps: []ActionStep{{
		Run: &ActionRun{Command: Site{Source: "${{ inputs.b == 'true' && github.token || '' }}"}},
	}}}
	bindings := []Binding{
		{Name: "a", Value: Site{Source: "false", Surface: SurfaceStepTemplate, Result: ResultString, Provenance: ProvenanceWorkflow}},
		{Name: "b", Value: Site{Source: "${{ inputs.a }}", Surface: SurfaceStepTemplate, Result: ResultString, Provenance: ProvenanceWorkflow}},
	}
	authority, err := InventoryActionAuthority(map[string]Action{"root": action}, "root", bindings, ActionAuthorityOptions{
		WorkflowInputs: expression.InputScope{Values: map[string]any{"a": true}, Complete: true},
	})
	if err != nil || !authority.GitHubToken {
		t.Fatalf("caller input true was shadowed by sibling input false: authority=%#v, error=%v", authority, err)
	}
}

func TestInventoryCompositeShellAuthorityPrunesKnownGuards(t *testing.T) {
	for _, test := range []struct {
		source, condition string
		wantToken         bool
	}{
		{source: "${{ env.RUNTIME == 'yes' && false && github.token || '' }}"},
		{source: "${{ true && (env.RUNTIME == 'yes' && false) && github.token || '' }}"},
		{source: "${{ case(env.SELECT == 'yes', env.RUNTIME == 'yes' && false, false) && github.token || '' }}"},
		{source: "${{ case(env.RUNTIME == 'yes' && false, github.token, '') }}"},
		{source: "${{ github.token }}", condition: "success() && false"},
		{source: "${{ github.token }}", condition: "env.RUNTIME == 'yes' && false"},
		{source: "${{ env.RUNTIME == 'yes' && github.token || '' }}", wantToken: true},
		{source: "${{ github.token }}", condition: "success()", wantToken: true},
		{source: "${{ hashFiles(github.token) }}", wantToken: true},
		{source: "${{ format('constant', github.token) }}"},
		{source: "${{ toJSON(github) }}"},
	} {
		t.Run(test.source+test.condition, func(t *testing.T) {
			action := Action{Runtime: "composite", Steps: []ActionStep{{
				Condition: Site{Source: test.condition}, Run: &ActionRun{Command: Site{Source: test.source}},
			}}}
			authority, err := InventoryActionAuthority(map[string]Action{"root": action}, "root", nil, ActionAuthorityOptions{})
			if err != nil || authority.GitHubToken != test.wantToken {
				t.Fatalf("authority=%#v, error=%v, want token=%v", authority, err, test.wantToken)
			}
		})
	}
}

func TestInventoryActionAuthorityDoesNotMutateSharedActions(t *testing.T) {
	// Leave positional annotations unset, as on wire-format sites. JSON
	// comparisons cannot detect writes to these non-serialized fields.
	actions := map[string]Action{
		"root": {Runtime: "composite", Inputs: []ActionInput{{Name: "token", Default: &Site{Source: "${{ github.server_url == 'https://github.com' && github.token || '' }}"}}}, Steps: []ActionStep{
			{Invocation: &Invocation{Lock: "child", Uses: Site{Source: "./child"}, With: []Binding{{Name: "token", Value: Site{Source: "${{ inputs.token }}"}}}}},
		}},
		"child": {Runtime: "node24", Inputs: []ActionInput{
			{Name: "token", Default: &Site{Source: "${{ github.token }}"}},
			{Name: "message", Default: &Site{Source: "safe"}},
		}},
		"broken": {Runtime: "composite", Steps: []ActionStep{{Invocation: &Invocation{Lock: "missing", Uses: Site{Source: "./missing"}}}}},
	}
	want := make(map[string]Action, len(actions))
	for id, action := range actions {
		want[id] = action.Clone()
	}
	t.Run("shared readers", func(t *testing.T) {
		for _, test := range []struct {
			name, root, server string
			supplied           []Binding
			token              bool
			err                string
		}{
			{name: "GitHub default", root: "root", server: "https://github.com", token: true},
			{name: "other provider", root: "root", server: "https://other.example"},
			{name: "explicit empty input", root: "root", server: "https://github.com", supplied: []Binding{{Name: "token", Value: Site{Surface: SurfaceStepTemplate, Result: ResultString, Provenance: ProvenanceWorkflow, Purpose: PurposeActionInput}}}},
			{name: "failed child", root: "broken", server: "https://github.com", err: `action program "missing" is missing`},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				got, err := InventoryActionAuthority(actions, test.root, test.supplied, ActionAuthorityOptions{ServerURL: test.server})
				if test.err != "" {
					if err == nil || !strings.Contains(err.Error(), test.err) {
						t.Fatalf("error = %v, want %q", err, test.err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got.GitHubToken != test.token || got.EventPayload || len(got.Secrets) != 0 {
					t.Fatalf("authority = %#v, want token=%v and no event or secrets", got, test.token)
				}
			})
		}
	})
	if !reflect.DeepEqual(actions, want) {
		t.Fatal("authority analysis mutated shared action programs")
	}
}

func TestInventoryActionAuthorityRefinesOrderedKnownDefaults(t *testing.T) {
	aDefault := Site{Source: "${{ false }}", Surface: SurfaceActionInputDefault, Result: ResultString, Provenance: ProvenanceAction, Purpose: PurposeExpression}
	bDefault := Site{Source: "${{ fromJSON(inputs.a) && github.token || '' }}", Surface: SurfaceActionInputDefault, Result: ResultString, Provenance: ProvenanceAction, Purpose: PurposeExpression}
	action := Action{Runtime: "node24", Inputs: []ActionInput{
		{Name: "a", Default: &aDefault},
		{Name: "b", Default: &bDefault},
	}}
	authority, err := InventoryActionAuthority(map[string]Action{"root": action}, "root", nil, ActionAuthorityOptions{ServerURL: "https://github.com"})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("known-false earlier default granted github.token to a later default")
	}
}

func TestInventoryActionAuthorityHonorsLazyCaseDefault(t *testing.T) {
	defaultValue := Site{Source: "${{ case(true, 'safe', github.token) }}", Surface: SurfaceActionInputDefault, Result: ResultString, Provenance: ProvenanceAction, Purpose: PurposeExpression}
	action := Action{Runtime: "node24", Inputs: []ActionInput{{Name: "token", Default: &defaultValue}}}
	authority, err := InventoryActionAuthority(map[string]Action{"root": action}, "root", nil, ActionAuthorityOptions{ServerURL: "https://github.com"})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("unselected case branch retained action github.token authority")
	}
}

func TestInventoryActionAuthorityTreatsRunnerDebugAsDisabled(t *testing.T) {
	defaultValue := Site{Source: "${{ runner.debug == '1' && github.token || '' }}", Surface: SurfaceActionInputDefault, Result: ResultString, Provenance: ProvenanceAction, Purpose: PurposeExpression}
	action := Action{Runtime: "node24", Inputs: []ActionInput{{Name: "token", Default: &defaultValue}}}
	authority, err := InventoryActionAuthority(map[string]Action{"root": action}, "root", nil, ActionAuthorityOptions{ServerURL: "https://github.com"})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("disabled runner debug guard retained action github.token authority")
	}
}

func TestActionMetadataRoundTripPreservesDockerEntrypoints(t *testing.T) {
	source := metadata.Metadata{Name: "docker", Runs: metadata.Runs{
		Using: "docker", Image: "Dockerfile", Entrypoint: "main.sh",
		PreEntrypoint: "pre.sh", PostEntrypoint: "post.sh",
	}}
	action := ActionFromMetadata(source, "docker", nil)
	got := action.Metadata("action.yml", ".")
	if got.Runs.Entrypoint != "main.sh" || got.Runs.PreEntrypoint != "pre.sh" || got.Runs.PostEntrypoint != "post.sh" {
		t.Fatalf("Metadata().Runs = %#v", got.Runs)
	}
}

func TestValidateActionProgramRejectsCrossRuntimeFields(t *testing.T) {
	site := func(surface Surface, result ResultType) Site {
		return Site{Surface: surface, Result: result, Provenance: ProvenanceAction, Purpose: PurposeExpression}
	}
	base := func(runtime string) Action {
		return Action{Runtime: runtime, PreIf: site(SurfaceActionLifecycle, ResultBoolean), PostIf: site(SurfaceActionLifecycle, ResultBoolean)}
	}
	tests := []struct {
		name   string
		action Action
	}{
		{name: "node docker pre entrypoint", action: func() Action { a := base("node24"); a.Main, a.PreEntrypoint = "index.js", "pre.sh"; return a }()},
		{name: "composite lifecycle", action: func() Action {
			a := base("composite")
			a.Pre = "pre.js"
			a.Steps = []ActionStep{{Run: &ActionRun{Command: site(SurfaceStepTemplate, ResultString)}, Name: site(SurfaceStepTemplate, ResultString), Shell: site(SurfaceStepTemplate, ResultString), WorkingDirectory: site(SurfaceStepTemplate, ResultString), Condition: site(SurfaceStepCondition, ResultBoolean)}}
			return a
		}()},
		{name: "composite lifecycle condition", action: func() Action {
			a := base("composite")
			a.PreIf.Source = "always()"
			a.Steps = []ActionStep{{Run: &ActionRun{Command: site(SurfaceStepTemplate, ResultString)}, Name: site(SurfaceStepTemplate, ResultString), Shell: site(SurfaceStepTemplate, ResultString), WorkingDirectory: site(SurfaceStepTemplate, ResultString), Condition: site(SurfaceStepCondition, ResultBoolean)}}
			return a
		}()},
		{name: "docker JavaScript lifecycle", action: func() Action { a := base("docker"); a.Image, a.Post = "Dockerfile", "post.js"; return a }()},
		{name: "docker lifecycle condition", action: func() Action { a := base("docker"); a.Image, a.PostIf.Source = "Dockerfile", "always()"; return a }()},
		{name: "docker pre entrypoint", action: func() Action { a := base("docker"); a.Image, a.PreEntrypoint = "Dockerfile", "pre.sh"; return a }()},
		{name: "docker post entrypoint", action: func() Action { a := base("docker"); a.Image, a.PostEntrypoint = "Dockerfile", "post.sh"; return a }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateActionStructure(test.action); err == nil {
				t.Fatal("validateActionStructure() accepted incompatible fields")
			}
		})
	}
}
