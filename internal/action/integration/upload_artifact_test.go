package integration

import (
	"strings"
	"testing"
)

func TestUploadArtifactCommitsAreExact(t *testing.T) {
	for _, commit := range []string{UploadArtifactCommit, UploadArtifactV5Commit, UploadArtifactV6Commit, UploadArtifactV7Commit} {
		if err := validateUploadArtifactCommit(commit); err != nil {
			t.Fatalf("audited commit %s rejected: %v", commit, err)
		}
	}
	for _, commit := range []string{"bbbca2ddaa5d8feaa63e36b76fdaad77386f024f", strings.Repeat("0", 40)} {
		if err := validateUploadArtifactCommit(commit); err == nil || !strings.Contains(err.Error(), UploadArtifactCommit) || !strings.Contains(err.Error(), UploadArtifactV5Commit) || !strings.Contains(err.Error(), UploadArtifactV6Commit) || !strings.Contains(err.Error(), UploadArtifactV7Commit) {
			t.Fatalf("unrecognized commit %s error = %v, want all audited commits", commit, err)
		}
	}
}

func TestValidateUploadArtifactInputs(t *testing.T) {
	for _, test := range []struct {
		commit string
		inputs map[string]string
	}{
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
	if err := ValidateUploadArtifactInputs(UploadArtifactCommit, map[string]string{"path": "payload", "archive": "true"}); err == nil || !strings.Contains(err.Error(), "only in actions/upload-artifact v7") {
		t.Fatalf("v4 archive input error = %v", err)
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
