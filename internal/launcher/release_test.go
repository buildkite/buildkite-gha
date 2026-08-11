package launcher

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseConfigPreservesArchiveAndAddsRawLauncher(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, required := range []string{"name_template: buildkite-gha_Linux_x86_64", "src: LICENSE", "main: ./cmd/buildkite-gha-launcher", "formats: [binary]", "name_template: buildkite-gha-launcher_Linux_x86_64"} {
		if strings.Count(s, required) != 1 {
			t.Errorf("release config must contain exactly one %q", required)
		}
	}
}
