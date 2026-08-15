//go:generate ../../../scripts/update-checkout-main-commits

// Package integration classifies actions with Buildkite-specific execution or
// service requirements.
package integration

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
)

// Identity is the canonical, resolved portion of an action lock used for
// integration matching. The immutable commit and source digest remain runtime
// verification evidence rather than catalog selectors.
type Identity struct {
	Source     string
	Repository string
	Path       string
}

// Adapter identifies a runtime replacement for an action's upstream lifecycle.
type Adapter string

const (
	// AdapterCheckoutExactEventSHA checks out the event repository at its exact
	// event SHA without executing actions/checkout's JavaScript lifecycle.
	AdapterCheckoutExactEventSHA Adapter = "checkout-exact-event-sha-v1"
	// AdapterUploadArtifactBuildkite stores the pinned upload-artifact action as
	// a Buildkite-native ZIP without executing its JavaScript lifecycle.
	AdapterUploadArtifactBuildkite Adapter = "upload-artifact-buildkite-v1"
	// AdapterDownloadArtifactBuildkite extracts one producer-attributed native
	// archive without using the GitHub Actions artifact service.
	AdapterDownloadArtifactBuildkite Adapter = "download-artifact-buildkite-v1"
	// CheckoutV3Commit through CheckoutV7Commit are the current audited release
	// implementations. CheckoutV3Commit is the final v3 release and is admitted
	// exactly rather than extending the v4-and-later main-branch snapshot.
	// CheckoutV7InitialCommit is retained because it is pinned by the OSS
	// compatibility corpus; its later v7.0.1 changes do not affect the adapter's
	// bounded exact-event-SHA operation.
	CheckoutV3Commit        = "a37ce9120846195fa4ece8f58b268e6043cb2f26"
	CheckoutV4Commit        = "11d5960a326750d5838078e36cf38b85af677262"
	CheckoutV5Commit        = "fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09"
	CheckoutV6Commit        = "d23441a48e516b6c34aea4fa41551a30e30af803"
	CheckoutV7InitialCommit = "9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0"
	CheckoutV7Commit        = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	// UploadArtifactCommit through UploadArtifactV7Commit are the audited
	// upstream implementations whose ZIP-mode semantics this adapter implements.
	// Raw v7 uploads remain explicitly unsupported by
	// ValidateUploadArtifactInputs.
	UploadArtifactCommit   = "ea165f8d65b6e75b540449e92b4886f43607fa02"
	UploadArtifactV5Commit = "330a01c490aca151604b8cf639adc76d48f6c5d4"
	UploadArtifactV6Commit = "b7c566a772e6b6bfb58ed0dc250532a479d7789f"
	UploadArtifactV7Commit = "043fb46d1a93c77aae656e7c1c64a875d1fc6a0a"
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
	// CacheV503Commit, CacheV5Commit, and CacheCommit are the exact audited
	// actions/cache implementations admitted to the Buildkite-backed cache-v2
	// service. CacheCommit retains the original v6.1.0 spelling.
	CacheV503Commit = "cdf6c1fa76f9f475f3d7449005a359c84ca0f306"
	CacheV5Commit   = "caa296126883cff596d87d8935842f9db880ef25"
	CacheCommit     = "55cc8345863c7cc4c66a329aec7e433d2d1c52a9"

	MaxUploadArtifactNameBytes = 255
	MaxUploadArtifactRoots     = 32
	MaxUploadArtifactPathBytes = 4096

	MaxDownloadArtifactPatternAlternatives = 8
	MaxDownloadArtifactPatternBytes        = MaxUploadArtifactNameBytes
)

// Service identifies an Actions runtime service used by a known action.
// Service classification supports admission diagnostics only: services may be
// used transitively and must not be provisioned based solely on this catalog.
type Service string

const (
	ServiceArtifact Service = "artifact"
	ServiceCache    Service = "cache"
)

// Descriptor records Buildkite-specific handling for one exact action identity.
type Descriptor struct {
	Adapter Adapter
	Service Service
	// CacheClientCompatibility adapts bundled cache clients to Buildkite's
	// cache-v2 environment. Before enabling it, confirm GITHUB_SERVER_URL
	// affects only caching and ACTIONS_CACHE_URL is only a presence check.
	CacheClientCompatibility bool
}

// UsesNativeAdapter reports whether the resolved identity replaces the
// upstream action lifecycle with Buildkite-native execution.
func UsesNativeAdapter(identity Identity) bool {
	descriptor, _ := Lookup(identity)
	return descriptor.Adapter != ""
}

var catalog = map[Identity]Descriptor{
	{Source: "github", Repository: "actions/checkout"}:                       {Adapter: AdapterCheckoutExactEventSHA},
	{Source: "github", Repository: "actions/cache"}:                          {Service: ServiceCache},
	{Source: "github", Repository: "actions/cache", Path: "restore"}:         {Service: ServiceCache},
	{Source: "github", Repository: "actions/cache", Path: "save"}:            {Service: ServiceCache},
	{Source: "github", Repository: "actions/upload-artifact"}:                {Adapter: AdapterUploadArtifactBuildkite},
	{Source: "github", Repository: "actions/upload-artifact", Path: "merge"}: {Service: ServiceArtifact},
	{Source: "github", Repository: "actions/download-artifact"}:              {Adapter: AdapterDownloadArtifactBuildkite},
	{Source: "github", Repository: "actions/setup-node"}:                     {CacheClientCompatibility: true},
	{Source: "github", Repository: "actions/setup-java"}:                     {CacheClientCompatibility: true},
	{Source: "github", Repository: "actions/setup-python"}:                   {CacheClientCompatibility: true},
	{Source: "github", Repository: "actions/setup-go"}:                       {CacheClientCompatibility: true},
	{Source: "github", Repository: "actions/setup-dotnet"}:                   {CacheClientCompatibility: true},
}

var checkoutCommits = map[string]string{
	CheckoutV3Commit:        "v3.7.0",
	CheckoutV4Commit:        "v4",
	CheckoutV5Commit:        "v5",
	CheckoutV6Commit:        "v6",
	CheckoutV7InitialCommit: "v7.0.0",
	CheckoutV7Commit:        "v7.0.1",
}

var uploadArtifactCommits = map[string]string{
	UploadArtifactCommit:   "v4.6.2",
	UploadArtifactV5Commit: "v5.0.0",
	UploadArtifactV6Commit: "v6.0.0",
	UploadArtifactV7Commit: "v7.0.1",
}

var cacheCommits = map[string]string{
	CacheV503Commit: "v5.0.3",
	CacheV5Commit:   "v5.1.0",
	CacheCommit:     "v6.1.0",
}

type unsupportedVersionError struct {
	action    string
	commit    string
	boundary  string
	supported []string
}

func (e *unsupportedVersionError) Error() string {
	return fmt.Sprintf("%s %s does not admit resolved commit %q; supported commits are %s", e.action, e.boundary, e.commit, strings.Join(e.supported, ", "))
}

// UnsupportedVersionDiagnostic separates corrective version guidance from
// immutable admission details for a rejected action integration.
func UnsupportedVersionDiagnostic(reference string, err error) (message, detail string, ok bool) {
	var versionErr *unsupportedVersionError
	if !errors.As(err, &versionErr) {
		return "", "", false
	}
	requested := ""
	if _, ref, found := strings.Cut(reference, "@"); found {
		requested = strings.TrimSpace(ref)
	}
	anchor := map[string]string{
		"actions/checkout":          "checkout-action",
		"actions/upload-artifact":   "upload-artifact-action",
		"actions/download-artifact": "download-artifact-action",
		"actions/cache":             "cache-action",
	}[versionErr.action]
	docs := "https://github.com/buildkite/buildkite-gha/blob/main/docs/compatibility.md#" + anchor
	if requested == "" {
		message = fmt.Sprintf("This %s version is unsupported. Use a supported version from %s.", versionErr.action, docs)
	} else {
		message = fmt.Sprintf("%s %s is unsupported. Use a supported version from %s.", versionErr.action, requested, docs)
	}
	return message, versionErr.Error(), true
}

func versionError(action, boundary, commit string, supported []string) error {
	return &unsupportedVersionError{action: action, boundary: boundary, commit: commit, supported: supported}
}

// ValidateCheckoutCommit admits known releases and a static snapshot of commits
// reachable from upstream main. Mutable references are resolved before this
// check, so changes after the snapshot remain fail-closed until regeneration.
func ValidateCheckoutCommit(commit string) error {
	if _, ok := checkoutCommits[commit]; ok {
		return nil
	}
	if _, ok := checkoutMainCommits[commit]; !ok {
		supported := append(sortedCheckoutCommits(), "upstream main snapshot ("+checkoutMainSnapshotCommit+")")
		return versionError("actions/checkout", "native adapter", commit, supported)
	}
	return nil
}

func sortedCheckoutCommits() []string {
	commits := make([]string, 0, len(checkoutCommits))
	for commit, version := range checkoutCommits {
		commits = append(commits, version+" ("+commit+")")
	}
	sort.Strings(commits)
	return commits
}

// ValidateUploadArtifactCommit rejects semantic drift from the audited action.
func ValidateUploadArtifactCommit(commit string) error {
	if _, ok := uploadArtifactCommits[commit]; !ok {
		commits := make([]string, 0, len(uploadArtifactCommits))
		for supported, version := range uploadArtifactCommits {
			commits = append(commits, version+" ("+supported+")")
		}
		sort.Strings(commits)
		return versionError("actions/upload-artifact", "native adapter", commit, commits)
	}
	return nil
}

func ValidateDownloadArtifactCommit(commit string) error {
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

// ValidateCacheCommit rejects cache client and protocol drift from the exact
// audited cache-v2 clients.
func ValidateCacheCommit(commit string) error {
	if _, ok := cacheCommits[commit]; !ok {
		commits := make([]string, 0, len(cacheCommits))
		for supported, version := range cacheCommits {
			commits = append(commits, version+" ("+supported+")")
		}
		sort.Strings(commits)
		return versionError("actions/cache", "Buildkite cache-v2 service", commit, commits)
	}
	return nil
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
	if err := ValidateDownloadArtifactCommit(commit); err != nil {
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
		merge = uploadArtifactTrue(value)
		if !merge && !uploadArtifactFalse(value) {
			return fmt.Errorf("input %q must be a GitHub Actions boolean", "merge-multiple")
		}
	}
	if hasPattern && !merge {
		return fmt.Errorf("pattern downloads require merge-multiple: true")
	}
	if hasName && merge {
		return fmt.Errorf("exact-name downloads may only omit merge-multiple or set it to false")
	}
	if value, ok := inputFold(inputs, "skip-decompress"); ok && !uploadArtifactFalse(strings.TrimSpace(value)) {
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

// ValidateUploadArtifactInputs validates the bounded adapter's static input
// surface. Values containing expressions are validated by the runtime after
// evaluation.
func ValidateUploadArtifactInputs(commit string, inputs map[string]string) error {
	return validateUploadArtifactInputs(commit, inputs, false)
}

// ValidateEvaluatedUploadArtifactInputs validates the bounded adapter's input
// surface after runtime expression evaluation.
func ValidateEvaluatedUploadArtifactInputs(commit string, inputs map[string]string) error {
	return validateUploadArtifactInputs(commit, inputs, true)
}

func validateUploadArtifactInputs(commit string, inputs map[string]string, evaluated bool) error {
	if err := ValidateUploadArtifactCommit(commit); err != nil {
		return err
	}
	allowed := map[string]bool{"name": true, "path": true, "if-no-files-found": true, "include-hidden-files": true, "compression-level": true, "overwrite": true, "archive": true, "retention-days": true}
	seen := map[string]bool{}
	for _, name := range sortedNames(inputs) {
		lower := strings.ToLower(name)
		if seen[lower] {
			return fmt.Errorf("duplicate case-insensitive input %q is unsupported", name)
		}
		seen[lower] = true
		if !allowed[lower] {
			return fmt.Errorf("unknown input %q is unsupported by the bounded upload-artifact adapter", name)
		}
		if lower == "archive" && commit != UploadArtifactV7Commit {
			return fmt.Errorf("input %q exists only in actions/upload-artifact v7", name)
		}
	}
	pathValue, ok := inputFold(inputs, "path")
	if !ok || strings.TrimSpace(pathValue) == "" {
		return fmt.Errorf("required input %q is missing", "path")
	}
	if evaluated || !uploadArtifactExpression(pathValue) {
		_, err := UploadArtifactPaths(pathValue)
		if err != nil {
			return err
		}
	}
	if value, ok := inputFold(inputs, "name"); ok && (evaluated || !uploadArtifactExpression(value)) {
		if err := ValidateUploadArtifactName(strings.TrimSpace(value)); err != nil {
			return err
		}
	}
	if value, ok := inputFold(inputs, "if-no-files-found"); ok && (evaluated || !uploadArtifactExpression(value)) && !uploadArtifactNoFilesValue(value) {
		return fmt.Errorf("input %q must be warn, error, or ignore", "if-no-files-found")
	}
	if value, ok := inputFold(inputs, "include-hidden-files"); ok && (evaluated || !uploadArtifactExpression(value)) && !uploadArtifactBoolean(strings.TrimSpace(value)) {
		return fmt.Errorf("input %q must be a GitHub Actions boolean", "include-hidden-files")
	}
	if value, ok := inputFold(inputs, "compression-level"); ok && (evaluated || !uploadArtifactExpression(value)) {
		level, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || level < 0 || level > 9 {
			return fmt.Errorf("input %q must be an integer from 0 to 9", "compression-level")
		}
	}
	if value, ok := inputFold(inputs, "retention-days"); ok && (evaluated || !uploadArtifactExpression(value)) {
		days, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || days < 0 {
			return fmt.Errorf("input %q must be a non-negative integer", "retention-days")
		}
	}
	if value, ok := inputFold(inputs, "overwrite"); ok && (evaluated || !uploadArtifactExpression(value)) && !uploadArtifactFalse(strings.TrimSpace(value)) {
		return fmt.Errorf("input %q may only be omitted or false; replacing artifacts is unsupported", "overwrite")
	}
	if value, ok := inputFold(inputs, "archive"); ok && (evaluated || !uploadArtifactExpression(value)) && !uploadArtifactTrue(strings.TrimSpace(value)) {
		return fmt.Errorf("input %q may only be omitted or true; raw uploads are unsupported", "archive")
	}
	return nil
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

func uploadArtifactExpression(value string) bool {
	return strings.Contains(value, "${{")
}

func uploadArtifactNoFilesValue(value string) bool {
	value = strings.TrimSpace(value)
	return value == "warn" || value == "error" || value == "ignore"
}

// ValidateUploadArtifactName enforces the upstream forbidden-character rules
// plus this adapter's explicit manifest bound.
func ValidateUploadArtifactName(name string) error {
	if !utf8.ValidString(name) || len(name) == 0 || len(name) > MaxUploadArtifactNameBytes || strings.ContainsAny(name, `":<>|*?`+"\r\n/\\") {
		return fmt.Errorf("invalid or oversized upload-artifact name")
	}
	return nil
}

// UploadArtifactPaths returns bounded workspace-relative literal roots and
// final-component file globs.
func UploadArtifactPaths(value string) ([]string, error) {
	if strings.Contains(value, "${{") {
		return nil, fmt.Errorf("input %q must contain literal paths, not expressions", "path")
	}
	var roots []string
	for _, line := range strings.Split(value, "\n") {
		root := strings.TrimSpace(line)
		if root == "" {
			continue
		}
		if strings.HasPrefix(root, "#") || uploadArtifactExtglob(root) {
			return nil, fmt.Errorf("path %q is unsafe; bounded adapter requires literal glob paths", root)
		}
		for i, component := range strings.Split(filepath.ToSlash(root), "/") {
			if component == ".." {
				return nil, fmt.Errorf("path %q contains traversal; bounded adapter requires workspace-relative paths", root)
			}
			if component == "." && i != 0 {
				return nil, fmt.Errorf("path %q uses non-canonical components", root)
			}
		}
		leadingDot := strings.HasPrefix(root, "./")
		for strings.HasPrefix(root, "./") {
			root = strings.TrimPrefix(root, "./")
		}
		if leadingDot && strings.ContainsAny(root, "*?[") {
			return nil, fmt.Errorf("path %q uses a non-canonical glob", root)
		}
		directoryOnly := strings.HasSuffix(root, "/")
		if directoryOnly && strings.ContainsAny(root, "*?[") {
			return nil, fmt.Errorf("path %q uses an unsupported directory glob", root)
		}
		root = path.Clean(root)
		if directoryOnly && root != "." {
			root += "/"
		}
		if len(root) > MaxUploadArtifactPathBytes || strings.HasPrefix(root, "!") || strings.ContainsAny(root, "{}") || strings.Contains(root, "\\") || !filepath.IsLocal(root) || !utf8.ValidString(root) {
			return nil, fmt.Errorf("path %q is unsafe; bounded adapter requires clean workspace-relative paths", root)
		}
		for _, r := range root {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("path %q contains control characters", root)
			}
		}
		if strings.ContainsAny(root, "*?[") && !doublestar.ValidatePattern(root) {
			return nil, fmt.Errorf("path %q contains an invalid glob pattern", root)
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("required input %q has no nonblank entries", "path")
	}
	if len(roots) > MaxUploadArtifactRoots {
		return nil, fmt.Errorf("input %q has %d roots, maximum is %d", "path", len(roots), MaxUploadArtifactRoots)
	}
	return roots, nil
}

func uploadArtifactExtglob(value string) bool {
	return strings.Contains(value, "@(") || strings.Contains(value, "+(") || strings.Contains(value, "?(") || strings.Contains(value, "*(") || strings.Contains(value, "!(")
}

func uploadArtifactBoolean(value string) bool {
	return uploadArtifactTrue(value) || uploadArtifactFalse(value)
}

func uploadArtifactTrue(value string) bool {
	return value == "true" || value == "True" || value == "TRUE"
}

func uploadArtifactFalse(value string) bool {
	return value == "false" || value == "False" || value == "FALSE"
}

func sortedNames(inputs map[string]string) []string {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func inputFold(inputs map[string]string, wanted string) (string, bool) {
	for name, value := range inputs {
		if strings.EqualFold(name, wanted) {
			return value, true
		}
	}
	return "", false
}

// Lookup returns the integration for an exact canonical action identity.
func Lookup(identity Identity) (Descriptor, bool) {
	descriptor, ok := catalog[identity]
	return descriptor, ok
}

// ValidateCheckoutInputs enforces the release-specific input contract
// implemented by the tokenless event-repository checkout adapter.
func ValidateCheckoutInputs(commit string, inputs map[string]string, repository, sha string) error {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		value := inputs[name]
		normalized := strings.ToLower(name)
		if seen[normalized] {
			return fmt.Errorf("duplicate case-insensitive input %q is unsupported", name)
		}
		seen[normalized] = true
		switch normalized {
		case "repository":
			if strings.EqualFold(value, repository) {
				continue
			}
		case "ref":
			if value == "" || validCheckoutSHA(value) || validCheckoutBranch(value) {
				continue
			}
		case "persist-credentials":
			if uploadArtifactFalse(value) {
				continue
			}
		case "fetch-depth":
			if depth, err := strconv.ParseUint(value, 10, 31); err == nil && depth <= 1<<31-1 {
				continue
			}
		case "clean", "set-safe-directory":
			if uploadArtifactTrue(value) {
				continue
			}
		case "lfs", "allow-unsafe-pr-checkout":
			if uploadArtifactFalse(value) {
				continue
			}
		case "submodules":
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "", "false", "true", "recursive":
				continue
			}
		case "fetch-tags":
			if uploadArtifactBoolean(value) {
				continue
			}
		case "show-progress":
			if commit != CheckoutV3Commit && uploadArtifactBoolean(value) {
				continue
			}
		case "path":
			if value == "" || validCheckoutPath(value) {
				continue
			}
		case "ssh-key", "ssh-known-hosts", "sparse-checkout":
			if value == "" {
				continue
			}
		case "filter":
			if commit != CheckoutV3Commit && value == "" {
				continue
			}
		case "ssh-strict", "sparse-checkout-cone-mode":
			if uploadArtifactTrue(value) {
				continue
			}
		case "ssh-user":
			if commit != CheckoutV3Commit && value == "git" {
				continue
			}
		case "github-server-url":
			if value == "" || value == "https://github.com" {
				continue
			}
		}
		return fmt.Errorf("explicit input %q value is unsupported", name)
	}
	return nil
}

func validCheckoutBranch(value string) bool {
	if strings.HasPrefix(value, "refs/heads/") {
		value = strings.TrimPrefix(value, "refs/heads/")
	} else if strings.HasPrefix(value, "refs/") {
		return false
	}
	if value == "" || len(value) > 255 || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.Contains(value, "@{") || strings.ContainsAny(value, " ~^:?*[\\") || !utf8.ValidString(value) {
		return false
	}
	if len(value) == 40 || len(value) == 64 {
		isHex := true
		for _, r := range strings.ToLower(value) {
			isHex = isHex && (r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
		}
		if isHex {
			return false
		}
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, ".") || strings.HasSuffix(strings.ToLower(segment), ".lock") {
			return false
		}
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validCheckoutSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validCheckoutPath(value string) bool {
	return len(value) <= 255 && value != "." && value != ".." && !strings.EqualFold(value, ".git") && !strings.Contains(value, "/") && !strings.Contains(value, "\\") && !strings.ContainsAny(value, "\r\n\x00") && filepath.IsLocal(value)
}
