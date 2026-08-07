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
	// UploadArtifactCommit is the only upstream implementation whose input and
	// archive semantics this adapter implements.
	UploadArtifactCommit   = "ea165f8d65b6e75b540449e92b4886f43607fa02"
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

// ValidateUploadArtifactCommit rejects semantic drift from the audited action.
func ValidateUploadArtifactCommit(commit string) error {
	if commit != UploadArtifactCommit {
		return fmt.Errorf("actions/upload-artifact native adapter supports only commit %s, resolved %q; Phase 6 is required", UploadArtifactCommit, commit)
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

// ValidateUploadArtifactInputs validates the bounded adapter's input surface.
// The runtime applies the same validation after expression evaluation.
func ValidateUploadArtifactInputs(inputs map[string]string) error {
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
		if lower == "retention-days" {
			return fmt.Errorf("input %q is unsupported; Phase 6 is required", name)
		}
	}
	pathValue, ok := inputFold(inputs, "path")
	if !ok || strings.TrimSpace(pathValue) == "" {
		return fmt.Errorf("required input %q is missing", "path")
	}
	if _, err := UploadArtifactPaths(pathValue); err != nil {
		return err
	}
	if value, ok := inputFold(inputs, "name"); ok {
		if err := ValidateUploadArtifactName(value); err != nil {
			return err
		}
	}
	if value, ok := inputFold(inputs, "if-no-files-found"); ok && value != "warn" && value != "error" && value != "ignore" {
		return fmt.Errorf("input %q must be warn, error, or ignore", "if-no-files-found")
	}
	if value, ok := inputFold(inputs, "include-hidden-files"); ok && !uploadArtifactBoolean(value) {
		return fmt.Errorf("input %q must be a GitHub Actions boolean", "include-hidden-files")
	}
	if value, ok := inputFold(inputs, "compression-level"); ok {
		level, err := strconv.Atoi(value)
		if err != nil || level < 0 || level > 9 {
			return fmt.Errorf("input %q must be an integer from 0 to 9", "compression-level")
		}
	}
	if value, ok := inputFold(inputs, "overwrite"); ok && !uploadArtifactFalse(value) {
		return fmt.Errorf("input %q may only be omitted or false; Phase 6 is required", "overwrite")
	}
	if value, ok := inputFold(inputs, "archive"); ok && !uploadArtifactTrue(value) {
		return fmt.Errorf("input %q may only be omitted or true; Phase 6 is required", "archive")
	}
	return nil
}

// ValidateUploadArtifactName enforces the upstream forbidden-character rules
// plus this adapter's explicit manifest bound.
func ValidateUploadArtifactName(name string) error {
	if !utf8.ValidString(name) || len(name) == 0 || len(name) > MaxUploadArtifactNameBytes || strings.ContainsAny(name, `":<>|*?`+"\r\n/\\") {
		return fmt.Errorf("invalid or oversized upload-artifact name")
	}
	return nil
}

// UploadArtifactPaths returns the bounded literal workspace-relative roots.
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
		if len(root) > MaxUploadArtifactPathBytes || strings.HasPrefix(root, "!") || strings.ContainsAny(root, "*?[]{}") || strings.Contains(root, "\\") || !filepath.IsLocal(root) || path.Clean(root) != root {
			return nil, fmt.Errorf("path %q is unsafe or non-literal; bounded adapter requires clean workspace-relative paths", root)
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
			if value == sha {
				continue
			}
		case "persist-credentials":
			if value == "false" {
				continue
			}
		case "fetch-depth":
			if value == "1" {
				continue
			}
		case "clean", "set-safe-directory":
			if value == "true" {
				continue
			}
		}
		return fmt.Errorf("explicit input %q is unsupported (including empty values); Phase 6 is required", name)
	}
	return nil
}
