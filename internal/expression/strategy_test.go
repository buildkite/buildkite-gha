package expression

import (
	"reflect"
	"testing"
)

func TestStrategyRuntimeProfiles(t *testing.T) {
	strategy := map[string]any{"job-index": 2, "job-total": 5, "fail-fast": false, "max-parallel": 3}
	for _, profile := range []ProfileID{ProfileJobEnvironment, ProfileJobDefault, ProfileJobOutput, ProfileStepTemplate, ProfileServiceCredential} {
		t.Run(string(profile), func(t *testing.T) {
			site := Site{Source: "${{ strategy['JOB-INDEX'] }}/${{ strategy.job-total }}/${{ strategy.fail-fast }}/${{ strategy.max-parallel }}", Profile: profile, Result: ResultString}
			got, err := NewEngine().Evaluate(site, Values{Runtime: Context{Strategy: strategy}})
			if err != nil || got != "2/5/false/3" {
				t.Fatalf("Evaluate = %v, %v", got, err)
			}
			if _, err := NewEngine().Evaluate(site, Values{}); err == nil {
				t.Fatal("missing strategy was silently defaulted")
			}
			for _, source := range []string{"${{ strategy }}", "${{ toJSON(strategy) }}", "${{ strategy[env.KEY] }}", "${{ strategy.* }}", "${{ strategy.job-index.extra }}"} {
				site.Source = source
				if _, err := NewEngine().Validate(site); err == nil {
					t.Errorf("accepted non-scalar strategy access %s", source)
				}
			}
		})
	}
	for _, profile := range []ProfileID{ProfileJobCondition, ProfileCompileJobCondition, ProfileCallCondition, ProfileWorkflowEnvironment, ProfileRuntimeTemplate, ProfileActionStepTemplate, ProfileActionStepCondition, ProfileActionLifecycle, ProfileActionInputDefault, ProfileDockerActionArg} {
		site := Site{Source: "${{ strategy.job-index }}", Profile: profile, Result: ResultString}
		if _, err := NewEngine().Validate(site); err == nil {
			t.Errorf("strategy admitted by %s", profile)
		}
	}
}

func TestStrategyAuthorityRefinement(t *testing.T) {
	engine := NewEngine()
	site := Site{Source: "${{ strategy.job-index == 1 && steps.prepare.outputs.ready == 'yes' && github.token || 'safe' }}", Profile: ProfileStepTemplate, Result: ResultString}
	for _, test := range []struct {
		name  string
		refs  map[string]any
		token bool
	}{
		{"unknown", nil, true},
		{"wrong row", map[string]any{"strategy.job-index": 0}, false},
		{"wrong row map", map[string]any{"strategy": map[string]any{"job-index": 0}}, false},
		{"runtime unknown map", map[string]any{"strategy": map[string]any{"job-index": 1}}, true},
		{"runtime unknown", map[string]any{"strategy.job-index": 1}, true},
		{"runtime false", map[string]any{"strategy.job-index": 1, "steps.prepare.outputs.ready": "no"}, false},
		{"runtime true", map[string]any{"strategy.job-index": 1, "steps.prepare.outputs.ready": "yes"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			analysis, err := engine.Analyze(site, AbstractValues{References: test.refs})
			if err != nil || (analysis.Effects.GitHubToken == GitHubTokenDirect) != test.token {
				t.Fatalf("Analyze = %+v, %v", analysis, err)
			}
			if test.name == "runtime false" || test.name == "wrong row" {
				value, err := engine.Evaluate(site, Values{Runtime: Context{Strategy: map[string]any{"job-index": test.refs["strategy.job-index"]}, Steps: map[string]StepStatus{"prepare": {Outputs: map[string]string{"ready": "no"}}}}})
				if err != nil || !analysis.Value.Known || value != analysis.Value.Value || value != "safe" {
					t.Fatalf("known abstract/concrete mismatch: %+v, %v, %v", analysis, value, err)
				}
			}
		})
	}
	site.Source = "${{ strategy.job-index == 9 && secrets.DEPLOY || 'safe' }}"
	validation, err := engine.Validate(site)
	if err != nil || !reflect.DeepEqual(validation.Secrets, []string{"DEPLOY"}) {
		t.Fatalf("exhaustive secret inventory = %+v, %v", validation, err)
	}
	for _, source := range []string{"${{ false && secrets[env.KEY] }}", "${{ false && toJSON(strategy) }}"} {
		site.Source = source
		if _, err := engine.Analyze(site, AbstractValues{References: map[string]any{"strategy.job-index": 0}}); err == nil {
			t.Fatalf("accepted unreachable prohibited reference: %s", source)
		}
	}
}
