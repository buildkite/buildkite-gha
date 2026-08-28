package program

import (
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
)

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
