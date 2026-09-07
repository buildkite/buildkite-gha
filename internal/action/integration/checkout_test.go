package integration

import (
	"strings"
	"testing"
)

func TestCheckoutCommitAdmission(t *testing.T) {
	for _, commit := range []string{
		CheckoutV1Commit, CheckoutV2Commit, CheckoutV3Commit, CheckoutV4Commit,
		CheckoutV5Commit, CheckoutV6Commit, CheckoutV7InitialCommit, CheckoutV7Commit,
		"af513c7a016048ae468971c52ed77d9562c7c819", // v1.0.0
		"722adc63f1aa60a57ec37892e133b1d319cae598", // v2.0.0
		"a12a3943b4bdde767164f792f33f40b04645d846", // v3.0.0
		"f43a0e5ff2bd294095638e18286ca9a3d1956744", // v3.6.0
		"1e31de5234b9f8995739874a8ce0492dc87873e2", // v4.0.0
	} {
		if err := validateCheckoutCommit(commit); err != nil {
			t.Fatalf("snapshotted commit %s rejected: %v", commit, err)
		}
	}
	if err := validateCheckoutCommit("de0fac2e4500dabe0009e67214ff5f5447ce83dd"); err != nil {
		t.Fatalf("upstream main commit rejected: %v", err)
	}
	unknown := strings.Repeat("0", 40)
	if err := validateCheckoutCommit(unknown); err != nil || !CheckoutUsesFallbackContract(unknown) {
		t.Fatalf("unknown immutable checkout commit fallback = %v, %t", err, CheckoutUsesFallbackContract(unknown))
	}
	if CheckoutUsesFallbackContract(CheckoutV7Commit) {
		t.Fatal("snapshotted checkout commit uses fallback contract")
	}
	for _, commit := range []string{"v8", strings.Repeat("A", 40), strings.Repeat("z", 40)} {
		if err := validateCheckoutCommit(commit); err == nil || !strings.Contains(err.Error(), "does not admit") || !strings.Contains(err.Error(), CheckoutV7Commit) || !strings.Contains(err.Error(), checkoutMainSnapshotCommit) {
			t.Fatalf("non-immutable checkout commit %s error = %v", commit, err)
		}
	}
}

func TestCheckoutSnapshotContracts(t *testing.T) {
	if len(checkoutCommitContracts) < 254 {
		t.Fatalf("snapshotted checkout commits = %d, want at least 254", len(checkoutCommitContracts))
	}
	for branch, tip := range checkoutSnapshotTips {
		if _, ok := checkoutCommitContracts[tip]; !ok {
			t.Errorf("snapshot branch %s tip %s has no admitted contract", branch, tip)
		}
	}
	for commit, contract := range checkoutCommitContracts {
		if contract.inputs == "" {
			t.Errorf("snapshotted commit %s has no declared inputs", commit)
		}
		names := strings.Split(contract.inputs, ",")
		for i, name := range names {
			if name != strings.ToLower(name) || i > 0 && names[i-1] >= name {
				t.Errorf("snapshotted commit %s has noncanonical inputs %q", commit, contract.inputs)
				break
			}
		}
		if contract.refOutput != contract.commitOutput {
			t.Errorf("snapshotted commit %s declares only one checkout output", commit)
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
		{"ref": "test-catalog", "path": "sources/test-catalog", "fetch-depth": "100"},
		{"clean": "false", "filter": "blob:none", "lfs": "true"},
		{"sparse-checkout": "src\ndocs\n", "sparse-checkout-cone-mode": "false"},
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
		"path":                 {"path": "nested/.git/path"},
		"filter":               {"filter": "blob:none\nobject:type"},
		"sparse checkout":      {"sparse-checkout": "\n\t\n"},
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

func TestValidateCheckoutInputsUsesPerCommitContract(t *testing.T) {
	repository, sha := "buildkite/buildkite-gha", strings.Repeat("a", 40)
	for _, test := range []struct {
		name, commit, input string
		wantAccepted        bool
	}{
		{name: "v2.0 before submodules", commit: "722adc63f1aa60a57ec37892e133b1d319cae598", input: "submodules"},
		{name: "v2.3.4 with submodules", commit: "5a4ac9002d0be2fb38bd78e4b4dbde5606d7042f", input: "submodules", wantAccepted: true},
		{name: "v3.0 before fetch-tags", commit: "a12a3943b4bdde767164f792f33f40b04645d846", input: "fetch-tags"},
		{name: "v3.7 with fetch-tags", commit: CheckoutV3Commit, input: "fetch-tags", wantAccepted: true},
		{name: "v4.0 before filter", commit: "1e31de5234b9f8995739874a8ce0492dc87873e2", input: "filter"},
		{name: "v4.2 with filter", commit: "d632683dd7b4114ad314bca15554477dd762a938", input: "filter", wantAccepted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := "false"
			if test.input == "filter" {
				value = ""
			}
			err := ValidateCheckoutInputs(test.commit, map[string]string{test.input: value}, repository, sha)
			if test.wantAccepted && err != nil {
				t.Fatalf("ValidateCheckoutInputs() = %v", err)
			}
			if !test.wantAccepted && (err == nil || !strings.Contains(err.Error(), "unsupported by this actions/checkout release")) {
				t.Fatalf("ValidateCheckoutInputs() = %v, want contract rejection", err)
			}
		})
	}
}

func TestValidateCheckoutInputsUsesBoundedFallbackContract(t *testing.T) {
	unknown := strings.Repeat("0", 40)
	repository, sha := "buildkite/buildkite-gha", strings.Repeat("a", 40)
	if err := ValidateCheckoutInputs(unknown, map[string]string{
		"repository": "buildkite/buildkite-gha",
		"ref":        sha,
		"path":       "sources/app",
		"filter":     "blob:none",
	}, repository, sha); err != nil {
		t.Fatalf("fallback contract rejected bounded current inputs: %v", err)
	}
	for name, inputs := range map[string]map[string]string{
		"token":               {"token": ""},
		"foreign repository":  {"repository": "other/repository"},
		"unsafe path":         {"path": "../outside"},
		"persist credentials": {"persist-credentials": "true"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCheckoutInputs(unknown, inputs, repository, sha); err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("fallback contract accepted %#v: %v", inputs, err)
			}
		})
	}
}
