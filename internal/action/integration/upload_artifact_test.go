package integration

import (
	"strings"
	"testing"
)

func TestUploadArtifactCommitContracts(t *testing.T) {
	commits := []string{
		UploadArtifactV1Commit, UploadArtifactV2Commit, UploadArtifactV3Commit,
		UploadArtifactV460Commit, UploadArtifactCommit, UploadArtifactV5Commit, UploadArtifactV6Commit, UploadArtifactV7Commit,
		"c7d193f32edcb7bfad88892161225aeda64e9392", // v4.0.0
		"694cdabd8bdb0f10b2cea11669e1bf5453eed0a6", // v4.2.0
		"50769540e7f4bd5e21e526ee35c689e35e0d6874", // v4.4.0
	}
	for _, commit := range commits {
		if err := validateUploadArtifactCommit(commit); err != nil {
			t.Fatalf("audited commit %s rejected: %v", commit, err)
		}
	}
	unknown := strings.Repeat("0", 40)
	if err := validateUploadArtifactCommit(unknown); err != nil || !UploadArtifactUsesFallbackContract(unknown) {
		t.Fatalf("unknown immutable commit fallback = %v, %t", err, UploadArtifactUsesFallbackContract(unknown))
	}
	for _, commit := range []string{UploadArtifactV460Commit, UploadArtifactV7Commit} {
		if UploadArtifactUsesFallbackContract(commit) {
			t.Fatalf("known commit %s uses fallback contract", commit)
		}
	}
	for _, commit := range []string{"c6a366c94c3e0affe28c06c8df20a878f24da3cf", "c6a3b2bd78b3985e4b2f15397fec357f0fd808de", "v7", strings.Repeat("A", 40), strings.Repeat("0", 39)} {
		err := validateUploadArtifactCommit(commit)
		if err == nil {
			t.Fatalf("unsupported commit %s accepted", commit)
		}
		for supported := range uploadArtifactCommits {
			if !strings.Contains(err.Error(), supported) {
				t.Fatalf("unsupported commit %s error = %v, want principal commit %s", commit, err, supported)
			}
		}
	}
}

func TestUploadArtifactSnapshotContracts(t *testing.T) {
	if len(uploadArtifactCommitContracts) < 306 {
		t.Fatalf("snapshotted upload-artifact commits = %d, want at least 306", len(uploadArtifactCommitContracts))
	}
	for branch, tip := range uploadArtifactSnapshotTips {
		if _, ok := uploadArtifactCommitContracts[tip]; !ok {
			t.Errorf("snapshot branch %s tip %s has no admitted contract", branch, tip)
		}
	}
	for commit, contract := range uploadArtifactCommitContracts {
		if contract.inputs == "" || !contract.declaresInput("name") || !contract.declaresInput("path") {
			t.Errorf("snapshotted commit %s lacks required adapter inputs", commit)
		}
		for _, names := range []string{contract.inputs, contract.outputs} {
			if names == "" {
				continue
			}
			parts := strings.Split(names, ",")
			for i, name := range parts {
				if name != strings.ToLower(name) || i > 0 && parts[i-1] >= name {
					t.Errorf("snapshotted commit %s has noncanonical names %q", commit, names)
					break
				}
			}
		}
		if contract.nameRequired != contract.v1 {
			t.Errorf("snapshotted commit %s has inconsistent v1 name requirement", commit)
		}
	}
}

func TestUploadArtifactFallbackUsesBoundedV7Contract(t *testing.T) {
	unknown := strings.Repeat("0", 40)
	if err := ValidateUploadArtifactInputs(unknown, map[string]string{
		"path": "payload/*.zip", "archive": "true", "include-hidden-files": "false",
		"compression-level": "0", "overwrite": "false",
	}); err != nil {
		t.Fatalf("fallback rejected bounded v7 inputs: %v", err)
	}
	if !UploadArtifactSupportsOutput(unknown, "artifact-id") || !UploadArtifactSupportsOutput(unknown, "artifact-digest") || UploadArtifactIncludesHiddenByDefault(unknown) {
		t.Fatal("fallback did not use v7 outputs and hidden-file default")
	}
	for _, inputs := range []map[string]string{
		{"path": "../outside"},
		{"path": "payload", "archive": "false"},
		{"path": "payload", "overwrite": "true"},
		{"path": "payload", "future-input": "value"},
	} {
		if err := ValidateUploadArtifactInputs(unknown, inputs); err == nil {
			t.Fatalf("fallback accepted unsafe inputs %#v", inputs)
		}
	}
}

func TestUploadArtifactSnapshotPreservesManifestContracts(t *testing.T) {
	const (
		v400 = "c7d193f32edcb7bfad88892161225aeda64e9392"
		v420 = "694cdabd8bdb0f10b2cea11669e1bf5453eed0a6"
		v440 = "50769540e7f4bd5e21e526ee35c689e35e0d6874"
	)
	if !UploadArtifactIncludesHiddenByDefault(v400) || !UploadArtifactSupportsOutput(v400, "artifact-id") || UploadArtifactSupportsOutput(v400, "artifact-digest") {
		t.Fatal("v4.0.0 snapshot contract does not preserve hidden-file default and outputs")
	}
	if err := ValidateUploadArtifactInputs(v400, map[string]string{"path": "payload", "overwrite": "false"}); err == nil || !strings.Contains(err.Error(), "unsupported by this actions/upload-artifact release") {
		t.Fatalf("v4.0.0 accepted later overwrite input: %v", err)
	}
	if err := ValidateUploadArtifactInputs(v420, map[string]string{"path": "payload", "overwrite": "false"}); err != nil {
		t.Fatalf("v4.2.0 rejected declared overwrite input: %v", err)
	}
	if err := ValidateUploadArtifactInputs(v420, map[string]string{"path": "payload", "include-hidden-files": "true"}); err == nil || !strings.Contains(err.Error(), "unsupported by this actions/upload-artifact release") {
		t.Fatalf("v4.2.0 accepted later hidden-file input: %v", err)
	}
	if err := ValidateUploadArtifactInputs(v440, map[string]string{"path": "payload", "include-hidden-files": "true"}); err != nil {
		t.Fatalf("v4.4.0 rejected declared hidden-file input: %v", err)
	}
	if UploadArtifactIncludesHiddenByDefault(v440) || UploadArtifactSupportsOutput(v440, "artifact-digest") {
		t.Fatal("v4.4.0 snapshot contract does not preserve hidden-file default and outputs")
	}
	if !UploadArtifactSupportsOutput(UploadArtifactV460Commit, "artifact-digest") {
		t.Fatal("v4.6.0 snapshot contract omits artifact-digest")
	}
}

func TestValidateUploadArtifactInputs(t *testing.T) {
	for _, test := range []struct {
		commit string
		inputs map[string]string
	}{
		{UploadArtifactV1Commit, map[string]string{"name": "payload", "path": "payload/result.txt"}},
		{UploadArtifactV2Commit, map[string]string{"path": "payload/result.txt", "retention-days": "7", "if-no-files-found": "error"}},
		{UploadArtifactV3Commit, map[string]string{"path": "payload/", "include-hidden-files": "true"}},
		{UploadArtifactCommit, map[string]string{"path": "payload/result.txt"}},
		{UploadArtifactCommit, map[string]string{"name": " payload ", "path": "payload/result.txt"}},
		{UploadArtifactV5Commit, map[string]string{"path": "./payload/result.txt"}},
		{UploadArtifactV6Commit, map[string]string{"path": "payload/", "name": "${{ github.sha }}", "retention-days": "0"}},
		{UploadArtifactCommit, map[string]string{
			"path": "tests/*.log", "retention-days": " 7 ", "if-no-files-found": " warn ",
			"include-hidden-files": " TRUE ", "compression-level": " 6 ", "overwrite": " false ",
		}},
		{UploadArtifactV7Commit, map[string]string{
			"name": "payload-${{ matrix.variant }}", "path": "${{ matrix.path }}",
			"if-no-files-found": "${{ matrix.no_files }}", "include-hidden-files": "${{ matrix.hidden }}",
			"compression-level": "${{ matrix.compression }}", "overwrite": "${{ matrix.overwrite }}", "archive": "${{ matrix.archive }}",
		}},
		{UploadArtifactCommit, map[string]string{"path": "**/build/**/*.html"}},
	} {
		if err := ValidateUploadArtifactInputs(test.commit, test.inputs); err != nil {
			t.Fatalf("ValidateUploadArtifactInputs(%s, %#v) = %v", test.commit, test.inputs, err)
		}
	}

	rejected := map[string]map[string]string{
		"missing path":        nil,
		"extglob":             {"path": "tests/+(a|b).log"},
		"glob comment":        {"path": "#tests/*.log"},
		"exclusion":           {"path": "!payload/**"},
		"invalid glob":        {"path": "payload/["},
		"brace expansion":     {"path": "payload/{one,two}"},
		"too many roots":      {"path": strings.Repeat("payload\n", MaxUploadArtifactRoots+1)},
		"bad retention":       {"path": "payload", "retention-days": "-1"},
		"overwrite":           {"path": "payload", "overwrite": "true"},
		"raw upload":          {"path": "payload", "archive": "false"},
		"bad no-files":        {"path": "payload", "if-no-files-found": "WARN"},
		"bad boolean":         {"path": "payload", "include-hidden-files": "1"},
		"bad compression":     {"path": "payload", "compression-level": "10"},
		"bad name":            {"path": "payload", "name": "bad/name"},
		"unknown":             {"path": "payload", "future-input": "value"},
		"case-colliding keys": {"path": "payload", "Name": "one", "name": "two"},
	}
	for name, inputs := range rejected {
		t.Run(name, func(t *testing.T) {
			if err := ValidateUploadArtifactInputs(UploadArtifactV7Commit, inputs); err == nil {
				t.Fatalf("ValidateUploadArtifactInputs(%#v) succeeded", inputs)
			}
		})
	}
	for _, commit := range []string{UploadArtifactV460Commit, UploadArtifactCommit} {
		if err := ValidateUploadArtifactInputs(commit, map[string]string{"path": "payload", "archive": "true"}); err == nil || !strings.Contains(err.Error(), "only in actions/upload-artifact v7") {
			t.Fatalf("v4 commit %s archive input error = %v", commit, err)
		}
	}
	for _, test := range []struct {
		name   string
		commit string
		inputs map[string]string
		want   string
	}{
		{name: "v1 requires name", commit: UploadArtifactV1Commit, inputs: map[string]string{"path": "payload"}, want: `required input "name" is missing`},
		{name: "v1 rejects glob path", commit: UploadArtifactV1Commit, inputs: map[string]string{"name": "payload", "path": "payload/*.txt"}, want: "must be one literal file or directory"},
		{name: "v1 rejects v2 input", commit: UploadArtifactV1Commit, inputs: map[string]string{"name": "payload", "path": "payload", "if-no-files-found": "warn"}, want: `explicit input "if-no-files-found" is unsupported`},
		{name: "v2 rejects v3 input", commit: UploadArtifactV2Commit, inputs: map[string]string{"path": "payload", "include-hidden-files": "true"}, want: `explicit input "include-hidden-files" is unsupported`},
		{name: "v3 rejects v4 input", commit: UploadArtifactV3Commit, inputs: map[string]string{"path": "payload", "compression-level": "0"}, want: `explicit input "compression-level" is unsupported`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateUploadArtifactInputs(test.commit, test.inputs); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateUploadArtifactInputs(%s, %#v) error = %v, want %q", test.commit, test.inputs, err, test.want)
			}
		})
	}
	if err := ValidateEvaluatedUploadArtifactInputs(UploadArtifactV7Commit, map[string]string{"path": "${{ still.unresolved }}"}); err == nil {
		t.Fatal("runtime accepted an unevaluated path expression")
	}
	if err := ValidateEvaluatedUploadArtifactInputs(UploadArtifactV7Commit, map[string]string{"path": "#tests/*.log"}); err == nil {
		t.Fatal("runtime accepted an evaluated upstream glob comment as a literal path")
	}
}

func TestUploadArtifactPathsNormalizesSafeRelativeSpellings(t *testing.T) {
	got, err := UploadArtifactPaths("./artifacts.tar.gz\nlog/\ntmp/capybara/\nreports/test (1).txt\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"artifacts.tar.gz", "log/", "tmp/capybara/", "reports/test (1).txt"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("UploadArtifactPaths() = %#v, want %#v", got, want)
	}
	for _, unsafe := range []string{
		"../outside", "safe/../outside", "/absolute", `dir\file`, "./tests/**/*.log", "tests/*.log/", "tests/./*.log",
		"tests/@(a|b).log", "tests/+(a|b).log", "tests/?(a|b).log", "tests/*(a|b).log", "tests/!(a|b).log",
	} {
		if _, err := UploadArtifactPaths(unsafe); err == nil {
			t.Fatalf("UploadArtifactPaths(%q) accepted an unsafe path", unsafe)
		}
	}
}
