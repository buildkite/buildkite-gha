package integration

import (
	"strings"
	"testing"
)

func TestDownloadArtifactExactContract(t *testing.T) {
	for _, commit := range DownloadArtifactCommits() {
		if err := validateDownloadArtifactCommit(commit); err != nil {
			t.Fatalf("audited commit %s rejected: %v", commit, err)
		}
	}
	if err := validateDownloadArtifactCommit(strings.Repeat("0", 40)); err == nil {
		t.Fatal("unrecognized commit accepted")
	}
	for _, commit := range DownloadArtifactCommits() {
		for _, inputs := range []map[string]string{{"name": "payload"}, {"Name": " payload ", "path": "", "merge-multiple": " False "}, {"name": "${{ github.sha }}", "path": "./out/"}} {
			if err := ValidateDownloadArtifactInputs(commit, inputs); err != nil {
				t.Fatalf("commit %s valid inputs rejected: %v", commit, err)
			}
		}
	}
	if err := ValidateDownloadArtifactRuntimeInputs(DownloadArtifactV7Commit, map[string]string{"name": "${{ github.sha }}", "path": "./"}); err == nil {
		t.Fatal("unevaluated runtime name accepted")
	}
	for _, commit := range []string{DownloadArtifactV8Commit, DownloadArtifactV801Commit} {
		if err := ValidateDownloadArtifactInputs(commit, map[string]string{"name": "payload", "skip-decompress": "false", "digest-mismatch": "error"}); err != nil {
			t.Fatalf("v8 default inputs rejected: %v", err)
		}
	}
	for name, inputs := range map[string]map[string]string{
		"all": nil, "absolute": {"name": "x", "path": "/tmp"},
		"name and pattern":              {"name": "x", "pattern": "x-*", "merge-multiple": "true"},
		"PostHog pattern without merge": {"pattern": "{junit-results-backend,product-junit-results}-*"},
		"merge":                         {"name": "x", "merge-multiple": "true"}, "ids": {"name": "x", "artifact-ids": "1"},
		"duplicate": {"name": "x", "Name": "y"}, "token": {"name": "x", "github-token": ""},
		"drive path": {"name": "x", "path": "C:/out"},
	} {
		t.Run(name, func(t *testing.T) {
			if ValidateDownloadArtifactInputs(DownloadArtifactV4Commit, inputs) == nil {
				t.Fatal("unsupported inputs accepted")
			}
		})
	}
	for name, inputs := range map[string]map[string]string{
		"raw":                    {"name": "x", "skip-decompress": "true"},
		"digest warning":         {"name": "x", "digest-mismatch": "warn"},
		"explicit empty boolean": {"name": "x", "skip-decompress": ""},
	} {
		t.Run("v8 "+name, func(t *testing.T) {
			if ValidateDownloadArtifactInputs(DownloadArtifactV801Commit, inputs) == nil {
				t.Fatal("unsupported v8 mode accepted")
			}
		})
	}
}

func TestDownloadArtifactPatterns(t *testing.T) {
	for _, test := range []struct {
		name    string
		pattern string
		want    []string
	}{
		{name: "single glob", pattern: "junit-results-backend-*", want: []string{"junit-results-backend-*"}},
		{name: "PostHog prefixes", pattern: "{junit-results-backend,product-junit-results}-*", want: []string{"junit-results-backend-*", "product-junit-results-*"}},
		{name: "duplicate prefix", pattern: "{junit,junit}-*", want: []string{"junit-*"}},
		{name: "maximum alternatives", pattern: "{a,b,c,d,e,f,g,h}-*", want: []string{"a-*", "b-*", "c-*", "d-*", "e-*", "f-*", "g-*", "h-*"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := DownloadArtifactPatterns(test.pattern)
			if err != nil || strings.Join(got, "|") != strings.Join(test.want, "|") {
				t.Fatalf("DownloadArtifactPatterns(%q) = %#v, %v, want %#v", test.pattern, got, err, test.want)
			}
		})
	}

	rejected := map[string]string{
		"empty alternative":        "{a,}-*",
		"one alternative":          "{a}-*",
		"too many alternatives":    "{a,b,c,d,e,f,g,h,i}-*",
		"nested":                   "{a,{b,c}}-*",
		"trailing brace":           "{a,b}-*}",
		"missing close":            "{a,b-*",
		"missing suffix":           "{a,b}",
		"non-leading group":        "prefix-{a,b}-*",
		"glob alternative":         "{a*,b}-*",
		"traversal":                "{../a,b}-*",
		"control character":        "{a,b\x1f}-*",
		"unsupported second group": "{a,b}-{c,d}*",
		"oversized expansion":      "{" + strings.Repeat("a", MaxDownloadArtifactPatternBytes) + ",b}-*",
	}
	for name, pattern := range rejected {
		t.Run(name, func(t *testing.T) {
			if _, err := DownloadArtifactPatterns(pattern); err == nil {
				t.Fatalf("DownloadArtifactPatterns(%q) succeeded", pattern)
			}
		})
	}
}

func TestNormalizeDownloadArtifactPath(t *testing.T) {
	for input, want := range map[string]string{"": ".", ".": ".", "./": ".", "././": ".", "out": "out", "out/": "out", "./out/": "out", "././out/": "out", "./nested/out///": "nested/out"} {
		t.Run("accept "+input, func(t *testing.T) {
			got, err := NormalizeDownloadArtifactPath(input)
			if err != nil || got != want {
				t.Fatalf("NormalizeDownloadArtifactPath(%q) = %q, %v, want %q", input, got, err, want)
			}
		})
	}
	for _, input := range []string{"/", "//", "/tmp", "C:/out", "./C:/out", "././C:/out", `\\server\share`, "../out", "./../out", ".//", ".//out", `out\child`, "out/./child", "out//child", "${{ matrix.path }}"} {
		t.Run("reject "+input, func(t *testing.T) {
			if _, err := NormalizeDownloadArtifactPath(input); err == nil {
				t.Fatalf("NormalizeDownloadArtifactPath(%q) succeeded", input)
			}
		})
	}
}
