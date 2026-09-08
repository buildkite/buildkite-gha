package expression

import (
	"reflect"
	"strings"
	"testing"
)

func TestReduceDeferredInputFoldsGraphTimeValuesAndKeepsNeedsReferences(t *testing.T) {
	context := CompileContext{
		GitHub: map[string]any{"ref_name": "main", "event_name": "push", "event": map[string]any{"number": 7}},
		Matrix: map[string]any{"variant": "alpine"},
		Vars:   map[string]string{"REGISTRY": "ghcr.io"},
	}
	for _, test := range []struct {
		name       string
		template   string
		want       string
		references []NeedOutputReference
	}{
		{
			name:       "literal text around one output",
			template:   "type=raw,value=${{ needs.meta.outputs.tag }}",
			want:       "type=raw,value=${{ needs.meta.outputs.tag }}",
			references: []NeedOutputReference{{Job: "meta", Output: "tag"}},
		},
		{
			name:       "multiline template with several outputs",
			template:   "type=schedule,pattern=${{ needs.compute-suffix.outputs.prerelease }}\nlatest=${{ needs.check-latest-stable.outputs.latest }}\n",
			want:       "type=schedule,pattern=${{ needs.compute-suffix.outputs.prerelease }}\nlatest=${{ needs.check-latest-stable.outputs.latest }}\n",
			references: []NeedOutputReference{{Job: "compute-suffix", Output: "prerelease"}, {Job: "check-latest-stable", Output: "latest"}},
		},
		{
			name:       "graph-time regions fold to text",
			template:   "${{ vars.REGISTRY }}/app:${{ github.ref_name }}-${{ needs.meta.outputs.tag }}",
			want:       "ghcr.io/app:main-${{ needs.meta.outputs.tag }}",
			references: []NeedOutputReference{{Job: "meta", Output: "tag"}},
		},
		{
			name:       "graph-time subtrees inside a needs expression fold to literals",
			template:   "${{ format('{0}-{1}-{2}', matrix.variant, needs.meta.outputs.tag, github.event.number) }}",
			want:       "${{ format('{0}-{1}-{2}', 'alpine', needs.meta.outputs.tag, 7) }}",
			references: []NeedOutputReference{{Job: "meta", Output: "tag"}},
		},
		{
			name:       "repeated outputs are listed once",
			template:   "${{ needs.meta.outputs.tag }}/${{ needs.meta.outputs.tag }}",
			want:       "${{ needs.meta.outputs.tag }}/${{ needs.meta.outputs.tag }}",
			references: []NeedOutputReference{{Job: "meta", Output: "tag"}},
		},
		{
			name:     "no needs reference reduces to a string",
			template: "app:${{ github.ref_name }}",
			want:     "app:main",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, references, err := ReduceDeferredInput(test.template, context)
			if err != nil {
				t.Fatalf("ReduceDeferredInput() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("ReduceDeferredInput() = %q, want %q", got, test.want)
			}
			if !reflect.DeepEqual(references, test.references) {
				t.Fatalf("ReduceDeferredInput() references = %#v, want %#v", references, test.references)
			}
			if got, err := DeferredInputReferences(test.want); err != nil || !reflect.DeepEqual(got, test.references) {
				t.Fatalf("DeferredInputReferences(%q) = %#v, %v; want %#v", test.want, got, err, test.references)
			}
			if _, err := NewEngine().Validate(Site{Source: test.want, Profile: ProfileDeferredInput, Result: ResultString}); err != nil {
				t.Fatalf("residual does not validate as a deferred input: %v", err)
			}
		})
	}
}

func TestReduceDeferredInputRejectsUnsupportedForms(t *testing.T) {
	context := CompileContext{GitHub: map[string]any{"ref_name": "main"}}
	for _, test := range []struct {
		name     string
		template string
		want     string
	}{
		{"need result", "${{ needs.meta.result }}-${{ needs.meta.outputs.tag }}", `needs reference "needs.meta.result" must be needs.<job>.outputs.<name>`},
		{"whole outputs object", "${{ toJSON(needs.meta.outputs) }}", `needs reference "needs.meta.outputs" must be needs.<job>.outputs.<name>`},
		{"dynamic output index", "${{ needs.meta.outputs[github.ref_name] }}", "must be needs.<job>.outputs.<name>"},
		{"bracket output access", "${{ needs.meta.outputs['tag'] }}", "must be needs.<job>.outputs.<name>"},
		{"whole needs context", "${{ toJSON(needs) }}", "must be needs.<job>.outputs.<name>"},
		{"runtime-only github value", "${{ github.run_id }}-${{ needs.meta.outputs.tag }}", `unavailable value "github.run_id"`},
		{"secrets", "${{ secrets.TOKEN }}-${{ needs.meta.outputs.tag }}", `unsupported compile-time context "secrets"`},
		{"unknown function", "${{ hashFiles('go.sum') }}-${{ needs.meta.outputs.tag }}", `unsupported compile-time function "hashFiles"`},
		{"github token", "${{ github.token }}-${{ needs.meta.outputs.tag }}", `unavailable value "github.token"`},
		{"whole github", "${{ toJSON(github) }}-${{ needs.meta.outputs.tag }}", "whole github access is unsupported"},
		{"unresolved parent input", "${{ inputs.flavor }}-${{ needs.meta.outputs.tag }}", `unavailable value "inputs.flavor"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := ReduceDeferredInput(test.template, context)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReduceDeferredInput(%q) error = %v, want %q", test.template, err, test.want)
			}
		})
	}
}

func TestDeferredInputProfileEvaluatesAgainstNeedsOnly(t *testing.T) {
	engine := NewEngine()
	site := Site{Source: "type=raw,value=${{ needs.meta.outputs.tag }},suffix=-${{ needs.suffix.outputs.value }}", Profile: ProfileDeferredInput, Result: ResultString}
	runtime := Context{Needs: map[string]NeedStatus{
		"meta":   {Outputs: map[string]string{"tag": "v1.2.3"}},
		"suffix": {Outputs: map[string]string{}},
	}}
	value, err := engine.Evaluate(site, Values{Runtime: runtime})
	if err != nil || value != "type=raw,value=v1.2.3,suffix=-" {
		t.Fatalf("Evaluate() = %#v, %v", value, err)
	}
	if _, err := engine.Evaluate(Site{Source: "${{ needs.missing.outputs.tag }}", Profile: ProfileDeferredInput, Result: ResultString}, Values{Runtime: runtime}); err == nil || !strings.Contains(err.Error(), `unavailable need "missing"`) {
		t.Fatalf("Evaluate() unknown need error = %v", err)
	}
	for _, source := range []string{
		"${{ needs.meta.result }}",
		"${{ github.ref_name }}-${{ needs.meta.outputs.tag }}",
		"${{ needs.meta.outputs['tag'] }}",
		"${{ toJSON(needs.meta.outputs) }}",
		"${{ hashFiles('go.sum') }}",
	} {
		if _, err := engine.Validate(Site{Source: source, Profile: ProfileDeferredInput, Result: ResultString}); err == nil {
			t.Errorf("Validate(%q) accepted an unsupported deferred input", source)
		}
	}
}
