package integration

import (
	"strings"
	"testing"
)

func TestCheckoutCommitAdmission(t *testing.T) {
	for _, commit := range []string{CheckoutV1Commit, CheckoutV2Commit, CheckoutV3Commit, CheckoutV4Commit, CheckoutV5Commit, CheckoutV6Commit, CheckoutV7InitialCommit, CheckoutV7Commit} {
		if err := validateCheckoutCommit(commit); err != nil {
			t.Fatalf("audited commit %s rejected: %v", commit, err)
		}
	}
	if err := validateCheckoutCommit("de0fac2e4500dabe0009e67214ff5f5447ce83dd"); err != nil {
		t.Fatalf("upstream main commit rejected: %v", err)
	}
	for _, commit := range []string{
		"f43a0e5ff2bd294095638e18286ca9a3d1956744", // v3.6.0 is an ancestor of upstream main.
		strings.Repeat("0", 40),
	} {
		if err := validateCheckoutCommit(commit); err == nil || !strings.Contains(err.Error(), "does not admit") || !strings.Contains(err.Error(), CheckoutV7Commit) || !strings.Contains(err.Error(), checkoutMainSnapshotCommit) {
			t.Fatalf("unrecognized checkout commit %s error = %v", commit, err)
		}
	}
}

func TestValidateCheckoutInputs(t *testing.T) {
	repository, sha := "buildkite/buildkite-gha", strings.Repeat("a", 40)
	for _, inputs := range []map[string]string{
		nil,
		{
			"repository": "BUILDKITE/BUILDKITE-GHA", "ref": sha, "fetch-depth": "1",
			"persist-credentials": "FALSE", "clean": "True", "set-safe-directory": "TRUE",
			"ssh-key": "", "ssh-known-hosts": "", "ssh-strict": "true", "ssh-user": "git",
			"path": "", "filter": "", "sparse-checkout": "", "sparse-checkout-cone-mode": "true",
			"fetch-tags": "true", "show-progress": "False", "lfs": "false", "submodules": "FALSE",
			"github-server-url": "https://github.com", "allow-unsafe-pr-checkout": "false",
		},
		{"ref": "", "github-server-url": ""},
		{"fetch-depth": "0"},
		{"fetch-depth": "100"},
		{"ref": strings.Repeat("b", 40)},
		{"ref": "test-catalog", "path": "test-catalog", "fetch-depth": "100"},
		{"submodules": " TrUe "},
		{"submodules": " ReCuRsIvE "},
	} {
		if err := ValidateCheckoutInputs(CheckoutV7Commit, inputs, repository, sha); err != nil {
			t.Fatalf("ValidateCheckoutInputs(%#v) = %v", inputs, err)
		}
	}

	for name, inputs := range map[string]map[string]string{
		"token":                {"token": ""},
		"foreign repository":   {"repository": "other/repository"},
		"SHA-256 ref":          {"ref": strings.Repeat("b", 64)},
		"invalid submodules":   {"submodules": "yes"},
		"path":                 {"path": "nested/path"},
		"filter":               {"filter": "blob:none"},
		"sparse checkout":      {"sparse-checkout": "src"},
		"lfs":                  {"lfs": "true"},
		"fetch depth":          {"fetch-depth": "-1"},
		"ssh key":              {"ssh-key": "key"},
		"GHES":                 {"github-server-url": "https://github.example"},
		"unknown":              {"future-input": "value"},
		"persist credentials":  {"persist-credentials": "true"},
		"case-colliding names": {"Repository": repository, "repository": repository},
		"uppercase SHA":        {"ref": strings.Repeat("B", 40)},
		"tag ref":              {"ref": "refs/tags/v1"},
		"option ref":           {"ref": "-branch"},
		"git metadata path":    {"path": ".git"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCheckoutInputs(CheckoutV7Commit, inputs, repository, sha); err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("ValidateCheckoutInputs(%#v) = %v, want unsupported-capability rejection", inputs, err)
			}
		})
	}
}

func TestValidateCheckoutV3InputsRejectsLaterContract(t *testing.T) {
	repository, sha := "buildkite/buildkite-gha", strings.Repeat("a", 40)
	for _, input := range []map[string]string{
		{"filter": ""},
		{"show-progress": "true"},
		{"ssh-user": "git"},
	} {
		if err := ValidateCheckoutInputs(CheckoutV3Commit, input, repository, sha); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("ValidateCheckoutInputs(%#v) = %v, want v3 contract rejection", input, err)
		}
	}
	if err := ValidateCheckoutInputs(CheckoutV3Commit, map[string]string{
		"repository": repository,
		"ref":        sha,
		"fetch-tags": "false",
	}, repository, sha); err != nil {
		t.Fatalf("ValidateCheckoutInputs() rejected v3 inputs: %v", err)
	}
}

func TestValidateCheckoutLegacyInputsRejectLaterContracts(t *testing.T) {
	repository, sha := "buildkite/buildkite-gha", strings.Repeat("a", 40)
	if err := ValidateCheckoutInputs(CheckoutV1Commit, map[string]string{
		"repository": repository, "ref": sha, "fetch-depth": "0",
		"clean": "true", "lfs": "false", "submodules": "recursive", "path": "sources",
	}, repository, sha); err != nil {
		t.Fatalf("ValidateCheckoutInputs() rejected v1 inputs: %v", err)
	}
	if err := ValidateCheckoutInputs(CheckoutV2Commit, map[string]string{
		"repository": repository, "ref": sha, "fetch-depth": "1",
		"ssh-key": "", "ssh-known-hosts": "", "ssh-strict": "true", "persist-credentials": "false",
		"set-safe-directory": "true", "allow-unsafe-pr-checkout": "false",
	}, repository, sha); err != nil {
		t.Fatalf("ValidateCheckoutInputs() rejected v2 inputs: %v", err)
	}

	for name, test := range map[string]struct {
		commit string
		inputs map[string]string
	}{
		"v1 persist-credentials": {CheckoutV1Commit, map[string]string{"persist-credentials": "false"}},
		"v1 ssh-key":             {CheckoutV1Commit, map[string]string{"ssh-key": ""}},
		"v1 set-safe-directory":  {CheckoutV1Commit, map[string]string{"set-safe-directory": "true"}},
		"v1 fetch-tags":          {CheckoutV1Commit, map[string]string{"fetch-tags": "false"}},
		"v2 fetch-tags":          {CheckoutV2Commit, map[string]string{"fetch-tags": "true"}},
		"v2 sparse-checkout":     {CheckoutV2Commit, map[string]string{"sparse-checkout": ""}},
		"v2 github-server-url":   {CheckoutV2Commit, map[string]string{"github-server-url": ""}},
		"v2 filter":              {CheckoutV2Commit, map[string]string{"filter": ""}},
		"v2 show-progress":       {CheckoutV2Commit, map[string]string{"show-progress": "true"}},
		"v2 ssh-user":            {CheckoutV2Commit, map[string]string{"ssh-user": "git"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateCheckoutInputs(test.commit, test.inputs, repository, sha)
			if err == nil || !strings.Contains(err.Error(), "unsupported by this actions/checkout release") {
				t.Fatalf("ValidateCheckoutInputs(%#v) = %v, want release-contract rejection", test.inputs, err)
			}
		})
	}
}
