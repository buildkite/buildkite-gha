// Package integration classifies actions with Buildkite-specific execution or
// service requirements.
package integration

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
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
	// CheckoutV4Commit through CheckoutV7Commit are the current audited major
	// release implementations. CheckoutV7InitialCommit is retained because it
	// is pinned by the OSS compatibility corpus; its later v7.0.1 changes do
	// not affect the adapter's bounded exact-event-SHA operation.
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
	DownloadArtifactCommit = "d3f86a106a0bac45b974a628896c90dbdf5c8093"
	// CacheCommit is the only actions/cache implementation admitted to the
	// Buildkite-backed cache-v2 service.
	CacheCommit = "55cc8345863c7cc4c66a329aec7e433d2d1c52a9"

	MaxUploadArtifactNameBytes = 255
	MaxUploadArtifactRoots     = 32
	MaxUploadArtifactPathBytes = 4096
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
}

var checkoutCommits = map[string]string{
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

// ValidateCheckoutCommit rejects semantic drift from the audited upstream
// manifests and implementations. Mutable references are resolved before this
// check, so a moved major tag must be deliberately audited and added here.
func ValidateCheckoutCommit(commit string) error {
	if _, ok := checkoutCommits[commit]; !ok {
		return fmt.Errorf("actions/checkout native adapter does not admit resolved commit %q; supported commits are %s", commit, strings.Join(sortedCheckoutCommits(), ", "))
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
		return fmt.Errorf("actions/upload-artifact native adapter does not admit resolved commit %q; supported commits are %s", commit, strings.Join(commits, ", "))
	}
	return nil
}

func ValidateDownloadArtifactCommit(commit string) error {
	if commit != DownloadArtifactCommit {
		return fmt.Errorf("actions/download-artifact native adapter supports only commit %s, resolved %q; Phase 6 is required", DownloadArtifactCommit, commit)
	}
	return nil
}

// ValidateCacheCommit rejects cache client and protocol drift from v6.1.0.
func ValidateCacheCommit(commit string) error {
	if commit != CacheCommit {
		return fmt.Errorf("buildkite cache-v2 service supports only actions/cache v6.1.0 commit %s, resolved %q", CacheCommit, commit)
	}
	return nil
}

// ValidateDownloadArtifactInputs implements only v4.3.0 exact-name mode.
func ValidateDownloadArtifactInputs(inputs map[string]string) error {
	allowed := map[string]bool{"name": true, "path": true, "merge-multiple": true}
	seen := map[string]bool{}
	for _, name := range sortedNames(inputs) {
		lower := strings.ToLower(name)
		if seen[lower] {
			return fmt.Errorf("duplicate case-insensitive input %q is unsupported; Phase 6 is required", name)
		}
		seen[lower] = true
		if !allowed[lower] {
			return fmt.Errorf("input %q is unsupported by the bounded download-artifact adapter; Phase 6 is required", name)
		}
	}
	name, ok := inputFold(inputs, "name")
	if !ok || strings.Contains(name, "${{") || ValidateUploadArtifactName(name) != nil {
		return fmt.Errorf("required input %q must be an exact literal artifact name", "name")
	}
	if value, ok := inputFold(inputs, "path"); ok {
		if value == "" || strings.Contains(value, "${{") || len(value) > MaxUploadArtifactPathBytes || strings.Contains(value, "\\") || !filepath.IsLocal(value) || path.Clean(value) != value {
			return fmt.Errorf("input %q must be a clean workspace-relative literal", "path")
		}
	}
	if value, ok := inputFold(inputs, "merge-multiple"); ok && !uploadArtifactFalse(value) {
		return fmt.Errorf("input %q may only be omitted or false; Phase 6 is required", "merge-multiple")
	}
	return nil
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
			return fmt.Errorf("duplicate case-insensitive input %q is unsupported; Phase 6 is required", name)
		}
		seen[lower] = true
		if !allowed[lower] {
			return fmt.Errorf("unknown input %q is unsupported by the bounded upload-artifact adapter; Phase 6 is required", name)
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
		return fmt.Errorf("input %q may only be omitted or false; Phase 6 is required", "overwrite")
	}
	if value, ok := inputFold(inputs, "archive"); ok && (evaluated || !uploadArtifactExpression(value)) && !uploadArtifactTrue(strings.TrimSpace(value)) {
		return fmt.Errorf("input %q may only be omitted or true; Phase 6 is required", "archive")
	}
	return nil
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
		return nil, fmt.Errorf("input %q must contain literal paths, not expressions; Phase 6 is required", "path")
	}
	var roots []string
	for _, line := range strings.Split(value, "\n") {
		root := strings.TrimSpace(line)
		if root == "" {
			continue
		}
		if len(root) > MaxUploadArtifactPathBytes || strings.HasPrefix(root, "!") || strings.HasPrefix(root, "#") || strings.ContainsAny(root, `\\?[]{}`) || uploadArtifactExtglob(root) || !filepath.IsLocal(root) {
			return nil, fmt.Errorf("path %q is unsafe; bounded adapter requires clean workspace-relative paths", root)
		}
		glob := strings.Contains(root, "*")
		directoryOnly := strings.HasSuffix(root, "/")
		components := strings.Split(root, "/")
		for i, component := range components {
			if component == ".." {
				return nil, fmt.Errorf("path %q contains traversal; bounded adapter requires workspace-relative paths", root)
			}
			if glob && component == "." && i != 0 {
				return nil, fmt.Errorf("path %q uses a non-canonical glob; Phase 6 is required", root)
			}
		}
		if glob && directoryOnly {
			return nil, fmt.Errorf("path %q uses an unsupported directory glob; Phase 6 is required", root)
		}
		root = path.Clean(root)
		if directoryOnly {
			root += "/"
		}
		if glob {
			directory, pattern := path.Split(root)
			directory = strings.TrimSuffix(directory, "/")
			if directory == "" {
				directory = "."
			}
			if strings.Contains(directory, "*") || strings.Contains(pattern, "**") {
				return nil, fmt.Errorf("path %q uses an unsupported recursive or non-final glob; Phase 6 is required", root)
			}
			if _, err := path.Match(pattern, "probe"); err != nil {
				return nil, fmt.Errorf("path %q has an invalid glob: %w", root, err)
			}
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

// ValidateCheckoutInputs enforces the input contract implemented by the
// tokenless exact-event-SHA checkout adapter.
func ValidateCheckoutInputs(inputs map[string]string, repository, sha string) error {
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
			return fmt.Errorf("duplicate case-insensitive input %q is unsupported; Phase 6 is required", name)
		}
		seen[normalized] = true
		switch normalized {
		case "repository":
			if strings.EqualFold(value, repository) {
				continue
			}
		case "ref":
			if value == "" || value == sha {
				continue
			}
		case "persist-credentials":
			if uploadArtifactFalse(value) {
				continue
			}
		case "fetch-depth":
			if value == "0" || value == "1" {
				continue
			}
		case "clean", "set-safe-directory":
			if uploadArtifactTrue(value) {
				continue
			}
		case "lfs", "submodules", "allow-unsafe-pr-checkout":
			if uploadArtifactFalse(value) {
				continue
			}
		case "fetch-tags", "show-progress":
			if uploadArtifactBoolean(value) {
				continue
			}
		case "ssh-key", "ssh-known-hosts", "path", "filter", "sparse-checkout":
			if value == "" {
				continue
			}
		case "ssh-strict", "sparse-checkout-cone-mode":
			if uploadArtifactTrue(value) {
				continue
			}
		case "ssh-user":
			if value == "git" {
				continue
			}
		case "github-server-url":
			if value == "" || value == "https://github.com" {
				continue
			}
		}
		return fmt.Errorf("explicit input %q value is unsupported; Phase 6 is required", name)
	}
	return nil
}
