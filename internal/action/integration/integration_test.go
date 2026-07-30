package integration

import (
	"strings"
	"testing"
)

func TestLookupMatchesCanonicalRootActions(t *testing.T) {
	tests := []struct {
		identity Identity
		want     Descriptor
	}{
		{Identity{Source: "github", Repository: "actions/checkout"}, Descriptor{Adapter: AdapterCheckoutExactEventSHA}},
		{Identity{Source: "github", Repository: "actions/cache"}, Descriptor{Service: ServiceCache}},
		{Identity{Source: "github", Repository: "actions/upload-artifact"}, Descriptor{Service: ServiceArtifact}},
		{Identity{Source: "github", Repository: "actions/download-artifact"}, Descriptor{Service: ServiceArtifact}},
	}
	for _, test := range tests {
		t.Run(test.identity.Repository, func(t *testing.T) {
			got, ok := Lookup(test.identity)
			if !ok || got != test.want {
				t.Fatalf("Lookup(%#v) = %#v, %t, want %#v, true", test.identity, got, ok, test.want)
			}
		})
	}
}

func TestLookupDoesNotBroadenCanonicalIdentity(t *testing.T) {
	for _, identity := range []Identity{
		{Source: "workspace", Repository: "actions/checkout"},
		{Source: "github", Repository: "Actions/Checkout"},
		{Source: "github", Repository: "actions/checkout", Path: "nested"},
		{Source: "github", Repository: "actions/cache", Path: "nested"},
		{Source: "github", Repository: "owner/action"},
	} {
		if descriptor, ok := Lookup(identity); ok {
			t.Fatalf("Lookup(%#v) = %#v, true, want no integration", identity, descriptor)
		}
	}
}

func TestValidateCheckoutInputs(t *testing.T) {
	repository, sha := "buildkite/buildkite-gha", strings.Repeat("a", 40)
	for _, inputs := range []map[string]string{
		nil,
		{"repository": "BUILDKITE/BUILDKITE-GHA", "ref": sha, "fetch-depth": "1", "persist-credentials": "false", "clean": "true", "set-safe-directory": "true"},
	} {
		if err := ValidateCheckoutInputs(inputs, repository, sha); err != nil {
			t.Fatalf("ValidateCheckoutInputs(%#v) = %v", inputs, err)
		}
	}

	for name, inputs := range map[string]map[string]string{
		"token":                {"token": ""},
		"foreign repository":   {"repository": "other/repository"},
		"foreign ref":          {"ref": strings.Repeat("b", 40)},
		"submodules":           {"submodules": "true"},
		"path":                 {"path": "nested"},
		"persist credentials":  {"persist-credentials": "true"},
		"case-colliding names": {"Repository": repository, "repository": repository},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCheckoutInputs(inputs, repository, sha); err == nil || !strings.Contains(err.Error(), "Phase 6") {
				t.Fatalf("ValidateCheckoutInputs(%#v) = %v, want Phase 6 rejection", inputs, err)
			}
		})
	}
}
