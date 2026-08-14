package integration

import (
	"strings"
	"testing"
)

func TestLookupMatchesKnownCanonicalActions(t *testing.T) {
	tests := []struct {
		identity Identity
		want     Descriptor
	}{
		{Identity{Source: "github", Repository: "actions/checkout"}, Descriptor{Adapter: AdapterCheckoutExactEventSHA}},
		{Identity{Source: "github", Repository: "actions/cache"}, Descriptor{Service: ServiceCache}},
		{Identity{Source: "github", Repository: "actions/cache", Path: "restore"}, Descriptor{Service: ServiceCache}},
		{Identity{Source: "github", Repository: "actions/cache", Path: "save"}, Descriptor{Service: ServiceCache}},
		{Identity{Source: "github", Repository: "actions/upload-artifact"}, Descriptor{Adapter: AdapterUploadArtifactBuildkite}},
		{Identity{Source: "github", Repository: "actions/upload-artifact", Path: "merge"}, Descriptor{Service: ServiceArtifact}},
		{Identity{Source: "github", Repository: "actions/download-artifact"}, Descriptor{Adapter: AdapterDownloadArtifactBuildkite}},
		{Identity{Source: "github", Repository: "actions/setup-node"}, Descriptor{CacheClientCompatibility: true}},
		{Identity{Source: "github", Repository: "actions/setup-java"}, Descriptor{CacheClientCompatibility: true}},
		{Identity{Source: "github", Repository: "actions/setup-python"}, Descriptor{CacheClientCompatibility: true}},
		{Identity{Source: "github", Repository: "actions/setup-go"}, Descriptor{CacheClientCompatibility: true}},
		{Identity{Source: "github", Repository: "actions/setup-dotnet"}, Descriptor{CacheClientCompatibility: true}},
	}
	for _, test := range tests {
		name := test.identity.Repository
		if test.identity.Path != "" {
			name += "/" + test.identity.Path
		}
		t.Run(name, func(t *testing.T) {
			got, ok := Lookup(test.identity)
			if !ok || got != test.want {
				t.Fatalf("Lookup(%#v) = %#v, %t, want %#v, true", test.identity, got, ok, test.want)
			}
		})
	}
}

func TestDownloadArtifactExactContract(t *testing.T) {
	for _, commit := range DownloadArtifactCommits() {
		if err := ValidateDownloadArtifactCommit(commit); err != nil {
			t.Fatalf("audited commit %s rejected: %v", commit, err)
		}
	}
	if err := ValidateDownloadArtifactCommit(strings.Repeat("0", 40)); err == nil {
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

func TestUploadArtifactCommitsAreExact(t *testing.T) {
	for _, commit := range []string{UploadArtifactCommit, UploadArtifactV5Commit, UploadArtifactV6Commit, UploadArtifactV7Commit} {
		if err := ValidateUploadArtifactCommit(commit); err != nil {
			t.Fatalf("audited commit %s rejected: %v", commit, err)
		}
	}
	for _, commit := range []string{"bbbca2ddaa5d8feaa63e36b76fdaad77386f024f", strings.Repeat("0", 40)} {
		if err := ValidateUploadArtifactCommit(commit); err == nil || !strings.Contains(err.Error(), UploadArtifactCommit) || !strings.Contains(err.Error(), UploadArtifactV5Commit) || !strings.Contains(err.Error(), UploadArtifactV6Commit) || !strings.Contains(err.Error(), UploadArtifactV7Commit) {
			t.Fatalf("unrecognized commit %s error = %v, want all audited commits", commit, err)
		}
	}
}

func TestCheckoutCommitAdmission(t *testing.T) {
	for _, commit := range []string{CheckoutV4Commit, CheckoutV5Commit, CheckoutV6Commit, CheckoutV7InitialCommit, CheckoutV7Commit} {
		if err := ValidateCheckoutCommit(commit); err != nil {
			t.Fatalf("audited commit %s rejected: %v", commit, err)
		}
	}
	if err := ValidateCheckoutCommit("de0fac2e4500dabe0009e67214ff5f5447ce83dd"); err != nil {
		t.Fatalf("upstream main commit rejected: %v", err)
	}
	for _, commit := range []string{
		"f43a0e5ff2bd294095638e18286ca9a3d1956744", // v3.6.0 is an ancestor of upstream main.
		"a37ce9120846195fa4ece8f58b268e6043cb2f26", // v3.7.0 isn't on upstream main.
		strings.Repeat("0", 40),
	} {
		if err := ValidateCheckoutCommit(commit); err == nil || !strings.Contains(err.Error(), "does not admit") || !strings.Contains(err.Error(), CheckoutV7Commit) || !strings.Contains(err.Error(), checkoutMainSnapshotCommit) {
			t.Fatalf("unrecognized checkout commit %s error = %v", commit, err)
		}
	}
}

func TestCacheCommitsAreExact(t *testing.T) {
	for _, commit := range []string{CacheV503Commit, CacheV5Commit, CacheCommit} {
		if err := ValidateCacheCommit(commit); err != nil {
			t.Fatalf("audited commit %s rejected: %v", commit, err)
		}
	}
	if err := ValidateCacheCommit(strings.Repeat("0", 40)); err == nil || !strings.Contains(err.Error(), "v5.0.3") || !strings.Contains(err.Error(), CacheV503Commit) || !strings.Contains(err.Error(), "v5.1.0") || !strings.Contains(err.Error(), CacheV5Commit) || !strings.Contains(err.Error(), "v6.1.0") || !strings.Contains(err.Error(), CacheCommit) {
		t.Fatalf("unrecognized cache commit error = %v", err)
	}
}

func TestUnsupportedActionVersionsSeparateGuidanceFromAdmissionDetail(t *testing.T) {
	commit := strings.Repeat("0", 40)
	tests := []struct {
		name      string
		reference string
		validate  func(string) error
		wantDocs  string
		wantBound string
	}{
		{name: "checkout", reference: "actions/checkout@v3", validate: ValidateCheckoutCommit, wantDocs: "#checkout-action", wantBound: "native adapter"},
		{name: "upload artifact", reference: "actions/upload-artifact@v6.0.2", validate: ValidateUploadArtifactCommit, wantDocs: "#upload-artifact-action", wantBound: "native adapter"},
		{name: "download artifact", reference: "actions/download-artifact@v9", validate: ValidateDownloadArtifactCommit, wantDocs: "#download-artifact-action", wantBound: "native adapter"},
		{name: "cache", reference: "actions/cache@v6.0.0", validate: ValidateCacheCommit, wantDocs: "#cache-action", wantBound: "cache-v2 service"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.validate(commit)
			message, detail, ok := UnsupportedVersionDiagnostic(test.reference, err)
			requested := test.reference[strings.LastIndex(test.reference, "@")+1:]
			if !ok || !strings.Contains(message, requested+" is unsupported") || !strings.Contains(message, test.wantDocs) {
				t.Fatalf("UnsupportedVersionDiagnostic() message = %q, ok = %t", message, ok)
			}
			if strings.Contains(message, commit) || strings.Contains(message, "supported commits") || strings.Contains(message, test.wantBound) {
				t.Fatalf("message contains lower-level detail: %q", message)
			}
			if !strings.Contains(detail, commit) || !strings.Contains(detail, "supported commits") || !strings.Contains(detail, test.wantBound) {
				t.Fatalf("detail = %q", detail)
			}
		})
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

func TestLookupDoesNotBroadenCanonicalIdentity(t *testing.T) {
	for _, identity := range []Identity{
		{Source: "workspace", Repository: "actions/checkout"},
		{Source: "github", Repository: "Actions/Checkout"},
		{Source: "github", Repository: "actions/checkout", Path: "nested"},
		{Source: "github", Repository: "actions/cache", Path: "nested"},
		{Source: "github", Repository: "actions/upload-artifact", Path: "nested"},
		{Source: "github", Repository: "actions/download-artifact", Path: "nested"},
		{Source: "github", Repository: "actions/setup-node", Path: "nested"},
		{Source: "github", Repository: "actions/setup-ruby"},
		{Source: "workspace", Repository: "actions/setup-node"},
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
		if err := ValidateCheckoutInputs(inputs, repository, sha); err != nil {
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
			if err := ValidateCheckoutInputs(inputs, repository, sha); err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("ValidateCheckoutInputs(%#v) = %v, want unsupported-capability rejection", inputs, err)
			}
		})
	}
}
