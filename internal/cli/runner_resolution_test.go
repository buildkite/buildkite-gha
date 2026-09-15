package cli

import (
	"reflect"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

func TestRunnerSelectorIsConfigured(t *testing.T) {
	linux := compiler.RunnerTarget{Queue: "linux", Platform: compiler.PlatformLinuxAMD64}
	cachedLinux := compiler.RunnerTarget{Queue: "linux", Platform: compiler.PlatformLinuxAMD64, Cache: &compiler.CacheVolume{Paths: []string{"/home/runner/.cache"}, Name: "dependencies", Size: "40g"}}
	equivalentCachedLinux := compiler.RunnerTarget{Queue: "linux", Platform: compiler.PlatformLinuxAMD64, Cache: &compiler.CacheVolume{Paths: []string{"/home/runner/.cache"}, Name: "dependencies", Size: "40g"}}
	other := compiler.RunnerTarget{Queue: "other", Platform: compiler.PlatformLinuxAMD64}
	targets := map[string]compiler.RunnerTarget{"ubuntu-18.04": linux, "self-hosted": linux, "cached": cachedLinux, "equivalent-cached": equivalentCachedLinux, "other": other}
	for _, test := range []struct {
		labels []string
		want   bool
	}{
		{labels: []string{"Ubuntu-18.04"}, want: true},
		{labels: []string{"self-hosted", "ubuntu-18.04"}, want: true},
		{labels: []string{"cached", "equivalent-cached"}, want: true},
		{labels: []string{"self-hosted", "missing"}},
		{labels: []string{"self-hosted", "other"}},
		{labels: nil},
	} {
		if got := runnerSelectorIsConfigured(test.labels, targets); got != test.want {
			t.Errorf("runnerSelectorIsConfigured(%q) = %v, want %v", test.labels, got, test.want)
		}
	}
}

func TestRunnerRequirementsIncludeExplicitMultiLabelTargets(t *testing.T) {
	target := compiler.RunnerTarget{Queue: "custom-linux", Platform: compiler.PlatformLinuxAMD64}
	configured := map[string]compiler.RunnerTarget{"self-hosted": target, "ubuntu-latest": target}
	jobs := []compiler.JobInstance{
		{RunsOn: []string{"self-hosted", "Ubuntu-Latest"}},
		{RunsOn: []string{"macos-latest"}},
		{RunsOn: []string{"self-hosted", "Ubuntu-Latest"}},
	}
	requirements := uniqueRunnerRequirements([]compiler.Report{{Jobs: jobs}}, configured)
	want := []gharuntime.RunnerRequirement{
		{ID: "r1", Labels: []string{"self-hosted", "Ubuntu-Latest"}, ConfiguredTarget: &gharuntime.ConfiguredRunnerTarget{Queue: "custom-linux", Platform: "linux/amd64"}},
		{ID: "r2", Labels: []string{"macos-latest"}},
	}
	if !reflect.DeepEqual(requirements, want) {
		t.Fatalf("requirements = %#v, want %#v", requirements, want)
	}
}
