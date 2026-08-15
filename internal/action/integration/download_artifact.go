package integration

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
)

const (
	// DownloadArtifact commits are the exact audited upstream releases whose
	// exact-name ZIP mode is implemented by the native adapter. Other selection,
	// raw-download, and digest-policy modes remain unsupported.
	DownloadArtifactV4Commit   = "d3f86a106a0bac45b974a628896c90dbdf5c8093"
	DownloadArtifactV5Commit   = "634f93cb2916e3fdff6788551b99b062d0335ce0"
	DownloadArtifactV6Commit   = "018cc2cf5baa6db3ef3c5f8a56943fffe632ef53"
	DownloadArtifactV7Commit   = "37930b1c2abaa49bbe596cd826c3c89aef350131"
	DownloadArtifactV8Commit   = "70fc10c6e5e1ce46ad2ea6f2b72d43f7d47b13c3"
	DownloadArtifactV801Commit = "3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c"
	// DownloadArtifactCommit is retained as the original v4.3.0 spelling.
	DownloadArtifactCommit = DownloadArtifactV4Commit

	MaxDownloadArtifactPatternAlternatives = 8
	MaxDownloadArtifactPatternBytes        = MaxUploadArtifactNameBytes
)

func validateDownloadArtifactCommit(commit string) error {
	for _, supported := range DownloadArtifactCommits() {
		if commit == supported {
			return nil
		}
	}
	return versionError("actions/download-artifact", "native adapter", commit, DownloadArtifactCommits())
}

// DownloadArtifactCommits returns the complete immutable admission set.
func DownloadArtifactCommits() []string {
	return []string{
		DownloadArtifactV4Commit,
		DownloadArtifactV5Commit,
		DownloadArtifactV6Commit,
		DownloadArtifactV7Commit,
		DownloadArtifactV8Commit,
		DownloadArtifactV801Commit,
	}
}

// ValidateDownloadArtifactInputs implements source-time admission for exact-name
// ZIP downloads and bounded pattern merging. Selectors are validated after evaluation by
// ValidateDownloadArtifactRuntimeInputs.
func ValidateDownloadArtifactInputs(commit string, inputs map[string]string) error {
	return validateDownloadArtifactInputs(commit, inputs, true)
}

// ValidateDownloadArtifactRuntimeInputs validates evaluated inputs at the
// runtime boundary.
func ValidateDownloadArtifactRuntimeInputs(commit string, inputs map[string]string) error {
	return validateDownloadArtifactInputs(commit, inputs, false)
}

func validateDownloadArtifactInputs(commit string, inputs map[string]string, allowNameExpression bool) error {
	if err := validateDownloadArtifactCommit(commit); err != nil {
		return err
	}
	allowed := map[string]bool{"name": true, "pattern": true, "path": true, "merge-multiple": true}
	if commit == DownloadArtifactV8Commit || commit == DownloadArtifactV801Commit {
		allowed["skip-decompress"] = true
		allowed["digest-mismatch"] = true
	}
	seen := map[string]bool{}
	for _, name := range sortedNames(inputs) {
		lower := strings.ToLower(name)
		if seen[lower] {
			return fmt.Errorf("duplicate case-insensitive input %q is unsupported", name)
		}
		seen[lower] = true
		if !allowed[lower] {
			return fmt.Errorf("input %q is unsupported by the bounded download-artifact adapter", name)
		}
	}
	name, hasName := inputFold(inputs, "name")
	name = strings.TrimSpace(name)
	hasNameExpression := strings.Contains(name, "${{")
	pattern, hasPattern := inputFold(inputs, "pattern")
	pattern = strings.TrimSpace(pattern)
	if hasName == hasPattern {
		return fmt.Errorf("exactly one of %q or %q is required", "name", "pattern")
	}
	if hasName && (name == "" || hasNameExpression && !allowNameExpression) {
		return fmt.Errorf("required input %q must be an exact literal artifact name", "name")
	}
	if hasName && !hasNameExpression && ValidateUploadArtifactName(name) != nil {
		return fmt.Errorf("required input %q must be an exact literal artifact name", "name")
	}
	if hasPattern && strings.Contains(pattern, "${{") && !allowNameExpression {
		return fmt.Errorf("input %q must be a bounded literal artifact-name glob", "pattern")
	}
	if hasPattern && !strings.Contains(pattern, "${{") {
		if _, err := DownloadArtifactPatterns(pattern); err != nil {
			return fmt.Errorf("input %q must be a bounded literal artifact-name glob: %w", "pattern", err)
		}
	}
	if value, ok := inputFold(inputs, "path"); ok {
		if _, err := NormalizeDownloadArtifactPath(value); err != nil {
			return err
		}
	}
	merge := false
	if value, ok := inputFold(inputs, "merge-multiple"); ok {
		value = strings.TrimSpace(value)
		merge = actionTrue(value)
		if !merge && !actionFalse(value) {
			return fmt.Errorf("input %q must be a GitHub Actions boolean", "merge-multiple")
		}
	}
	if hasPattern && !merge {
		return fmt.Errorf("pattern downloads require merge-multiple: true")
	}
	if hasName && merge {
		return fmt.Errorf("exact-name downloads may only omit merge-multiple or set it to false")
	}
	if value, ok := inputFold(inputs, "skip-decompress"); ok && !actionFalse(strings.TrimSpace(value)) {
		return fmt.Errorf("input %q may only be omitted or false; raw downloads are unsupported", "skip-decompress")
	}
	if value, ok := inputFold(inputs, "digest-mismatch"); ok && strings.TrimSpace(value) != "error" {
		return fmt.Errorf("input %q may only be omitted or error; digest mismatch is always fatal", "digest-mismatch")
	}
	return nil
}

// NormalizeDownloadArtifactPath accepts the safe relative spellings used by
// upstream and returns their clean slash form. It intentionally normalizes
// only leading ./ components and trailing slashes.
func NormalizeDownloadArtifactPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." {
		return ".", nil
	}
	if strings.Contains(value, "${{") || len(value) > MaxUploadArtifactPathBytes || strings.Contains(value, "\\") {
		return "", fmt.Errorf("input %q must be a safe workspace-relative literal", "path")
	}
	normalized := value
	for strings.HasPrefix(normalized, "./") {
		normalized = strings.TrimPrefix(normalized, "./")
	}
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("input %q must be a safe workspace-relative literal", "path")
	}
	normalized = strings.TrimRight(normalized, "/")
	if normalized == "" {
		return ".", nil
	}
	if windowsDrivePath(normalized) || !filepath.IsLocal(normalized) || path.Clean(normalized) != normalized {
		return "", fmt.Errorf("input %q must be a safe workspace-relative literal", "path")
	}
	return normalized, nil
}

func windowsDrivePath(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':'
}

// DownloadArtifactPatterns expands an optional leading group of literal
// artifact-name prefixes into independently validated bounded globs.
func DownloadArtifactPatterns(pattern string) ([]string, error) {
	if !strings.ContainsAny(pattern, "{}") {
		if !validDownloadArtifactPattern(pattern) {
			return nil, fmt.Errorf("invalid artifact-name glob")
		}
		return []string{pattern}, nil
	}
	if !strings.HasPrefix(pattern, "{") || strings.Count(pattern, "{") != 1 || strings.Count(pattern, "}") != 1 {
		return nil, fmt.Errorf("alternation must be one leading, non-nested prefix group")
	}
	close := strings.IndexByte(pattern, '}')
	if close < 0 || close == len(pattern)-1 {
		return nil, fmt.Errorf("alternation must be followed by an artifact-name glob suffix")
	}
	alternatives := strings.Split(pattern[1:close], ",")
	if len(alternatives) < 2 || len(alternatives) > MaxDownloadArtifactPatternAlternatives {
		return nil, fmt.Errorf("alternation has %d alternatives, require 2 through %d", len(alternatives), MaxDownloadArtifactPatternAlternatives)
	}
	suffix := pattern[close+1:]
	expanded := make([]string, 0, len(alternatives))
	seen := make(map[string]struct{}, len(alternatives))
	for _, alternative := range alternatives {
		if alternative == "" || strings.ContainsAny(alternative, "*?[]{}") {
			return nil, fmt.Errorf("alternation alternatives must be non-empty literal prefixes")
		}
		candidate := alternative + suffix
		if !validDownloadArtifactPattern(candidate) {
			return nil, fmt.Errorf("expanded alternative is not a valid artifact-name glob")
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		expanded = append(expanded, candidate)
	}
	return expanded, nil
}

func validDownloadArtifactPattern(pattern string) bool {
	if pattern == "" || len(pattern) > MaxDownloadArtifactPatternBytes || !utf8.ValidString(pattern) || strings.HasPrefix(pattern, "!") || strings.ContainsAny(pattern, "\\/:<>|{}\r\n") || !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	for _, r := range pattern {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return doublestar.ValidatePattern(pattern)
}
