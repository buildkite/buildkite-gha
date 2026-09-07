package cli

import (
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
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
