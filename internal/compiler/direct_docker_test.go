package compiler

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestCompileDirectDockerActions(t *testing.T) {
	for _, image := range []string{"alpine:3.20", "registry.example:5000/team/tool:v2", "busybox@sha256:" + strings.Repeat("a", 64)} {
		t.Run(image, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, ".github/workflows/direct.yml")
			body := []byte(`on: push
jobs:
  direct:
    runs-on: ubuntu-latest
    steps:
      - uses: docker://` + image + `
        with:
          Entrypoint: /bin/echo
          Args: '"two words" "" $HOME --privileged'
      - parallel:
          - uses: docker://` + image + `
            with:
              entrypoint: ''
              args: parallel
`)
			jobs, err := compileUntrustedPlans(path, body, pushEvent(t), "direct-test", testDistributionDigest, "hosted")
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 1 || len(jobs[0].Actions) != 1 {
				t.Fatalf("expected one job and one shared image lock: %#v", jobs)
			}
			job := jobs[0]
			lock := job.Actions[0]
			if lock.Source != "docker" || lock.DockerImage != image || lock.SourceDigest != "" || job.GitHubToken != nil || !job.HasCapability("docker") || !job.HasCapability("network") {
				t.Fatalf("image-only plan = %#v", job)
			}
			validateCompiledPlansAgainstSchema(t, jobs)
			encoded, err := plan.Encode(job)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := plan.Decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			again, err := plan.Encode(decoded)
			if err != nil || !bytes.Equal(encoded, again) {
				t.Fatalf("image plan round trip: %v", err)
			}
			for name, mutate := range map[string]func(*plan.Job){
				"mixed source":    func(j *plan.Job) { j.Actions[0].SourceDigest = "sha256:" + strings.Repeat("b", 64) },
				"repository":      func(j *plan.Job) { j.Actions[0].Repository = "owner/repo" },
				"executables":     func(j *plan.Job) { j.Actions[0].ExecutablePaths = []string{} },
				"image mismatch":  func(j *plan.Job) { j.Actions[0].DockerImage = "busybox:latest" },
				"missing docker":  func(j *plan.Job) { j.RequiredCapabilities = []string{"network"} },
				"missing network": func(j *plan.Job) { j.RequiredCapabilities = []string{"docker"} },
				"wrong program": func(j *plan.Job) {
					a := j.Program.Actions[lock.ID]
					a.Runtime = "composite"
					j.Program.Actions[lock.ID] = a
				},
			} {
				t.Run(name, func(t *testing.T) {
					changed, err := plan.Decode(encoded)
					if err != nil {
						t.Fatal(err)
					}
					mutate(&changed)
					if err := changed.Validate(); err == nil {
						t.Fatal("malformed image plan accepted")
					}
				})
			}
		})
	}
}

func TestCompileDirectDockerRejectsInvalidImages(t *testing.T) {
	for _, reference := range []string{"docker://", "docker://alpine@sha256:bad", "docker://--privileged", "docker://alpine extra", "DOCKER://alpine:latest"} {
		if _, _, _, _, err := compileActionLocks(t.Context(), t.TempDir(), nil, []string{reference}); err == nil {
			t.Errorf("accepted invalid direct image %q", reference)
		}
	}
}

func TestDirectDockerRequiresLinux(t *testing.T) {
	for _, platform := range []Platform{PlatformDarwinARM64, PlatformWindowsAMD64} {
		t.Run(platform.String(), func(t *testing.T) {
			body := []byte("on: push\njobs:\n  direct:\n    runs-on: custom\n    steps:\n      - uses: docker://alpine:3.20\n      - uses: owner/repo@v1\n")
			_, err := compilePlansForTest(t.Context(), filepath.Join(t.TempDir(), ".github/workflows/direct.yml"), body, pushEvent(t), "direct-test", testDistributionDigest, Options{
				EventTrust:           EventUntrusted,
				Runners:              RunnerPolicy{Targets: map[string]RunnerTarget{"custom": {Queue: "hosted", Platform: platform}}, UntrustedQueues: []string{"hosted"}},
				RuntimeDistributions: map[Platform]string{platform: testDistributionDigest},
			})
			if err == nil || !strings.Contains(err.Error(), "requires Docker, which is unavailable on") {
				t.Fatalf("non-Linux direct image rejection = %v", err)
			}
		})
	}
}
