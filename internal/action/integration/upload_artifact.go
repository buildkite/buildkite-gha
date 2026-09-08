//go:generate go run ./cmd/generate-upload-artifact-profiles

package integration

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/buildkite/buildkite-gha/internal/git"
)

const (
	// UploadArtifactV1Commit through UploadArtifactV7Commit are the audited
	// upstream implementations whose ZIP-mode semantics this adapter implements.
	// The v1, v2, and v3 commits are the floating legacy major releases used on
	// github.com and are admitted exactly. Raw v7 uploads remain explicitly
	// unsupported by ValidateUploadArtifactInputs.
	UploadArtifactV1Commit   = "3446296876d12d4e3a0f3145a3c87e67bf0a16b5"
	UploadArtifactV2Commit   = "82c141cc518b40d92cc801eee768e7aafc9c2fa2"
	UploadArtifactV3Commit   = "ff15f0306b3f739f7b6fd43fb5d26cd321bd4de5"
	UploadArtifactV460Commit = "65c4c4a1ddee5b72f698fdd19549f0f0fb45cf08"
	UploadArtifactCommit     = "ea165f8d65b6e75b540449e92b4886f43607fa02"
	UploadArtifactV5Commit   = "330a01c490aca151604b8cf639adc76d48f6c5d4"
	UploadArtifactV6Commit   = "b7c566a772e6b6bfb58ed0dc250532a479d7789f"
	UploadArtifactV7Commit   = "043fb46d1a93c77aae656e7c1c64a875d1fc6a0a"
	// UploadArtifactFallbackContractRelease identifies the stable contract used
	// for immutable commits outside the exact admission set.
	UploadArtifactFallbackContractRelease = "v7.0.1"

	MaxUploadArtifactNameBytes = 255
	MaxUploadArtifactRoots     = 32
	MaxUploadArtifactPathBytes = 4096
)

// uploadArtifactUnsupportedCommits are GHES-only legacy security backports
// that remain outside the github.com native adapter contract.
var uploadArtifactUnsupportedCommits = map[string]bool{
	"c6a366c94c3e0affe28c06c8df20a878f24da3cf": true, // v3.2.2
	"c6a3b2bd78b3985e4b2f15397fec357f0fd808de": true, // v3.2.2-node20
}

var uploadArtifactCommits = map[string]string{
	UploadArtifactV1Commit:   "v1.0.0",
	UploadArtifactV2Commit:   "v2.3.1",
	UploadArtifactV3Commit:   "v3.2.1",
	UploadArtifactV460Commit: "v4.6.0",
	UploadArtifactCommit:     "v4.6.2",
	UploadArtifactV5Commit:   "v5.0.0",
	UploadArtifactV6Commit:   "v6.0.0",
	UploadArtifactV7Commit:   "v7.0.1",
}

// uploadArtifactContract records the adapter-visible contract declared by one
// immutable upstream action manifest.
type uploadArtifactContract struct {
	// inputs and outputs are sorted, comma-separated name sets.
	inputs, outputs string
	nameRequired    bool
	v1              bool
	hiddenByDefault bool
}

func (c uploadArtifactContract) declaresInput(name string) bool {
	return strings.Contains(","+c.inputs+",", ","+name+",")
}

func (c uploadArtifactContract) declaresOutput(name string) bool {
	return strings.Contains(","+c.outputs+",", ","+name+",")
}

func uploadArtifactContractForCommit(commit string) (uploadArtifactContract, bool) {
	if !uploadArtifactUnsupportedCommits[commit] {
		if contract, exact := uploadArtifactCommitContracts[commit]; exact {
			return contract, true
		}
	}
	if !git.ValidObjectID(commit) || uploadArtifactUnsupportedCommits[commit] {
		return uploadArtifactContract{}, false
	}
	return uploadArtifactCommitContracts[UploadArtifactV7Commit], false
}

// UploadArtifactUsesFallbackContract reports whether an immutable commit is
// absent from the frozen snapshot and therefore uses the stable v7 contract.
func UploadArtifactUsesFallbackContract(commit string) bool {
	if !git.ValidObjectID(commit) || uploadArtifactUnsupportedCommits[commit] {
		return false
	}
	_, exact := uploadArtifactCommitContracts[commit]
	return !exact
}

// UploadArtifactSupportsOutput reports whether the selected exact or fallback
// contract declares an output.
func UploadArtifactSupportsOutput(commit, name string) bool {
	contract, _ := uploadArtifactContractForCommit(commit)
	return contract.declaresOutput(name)
}

// UploadArtifactIncludesHiddenByDefault reports whether omission of
// include-hidden-files retains hidden paths for the selected contract.
func UploadArtifactIncludesHiddenByDefault(commit string) bool {
	contract, _ := uploadArtifactContractForCommit(commit)
	return contract.hiddenByDefault
}

// UploadArtifactUsesV1Contract reports whether the selected exact contract
// has the single-path and missing-path behavior of the v1 runner plugin.
func UploadArtifactUsesV1Contract(commit string) bool {
	contract, _ := uploadArtifactContractForCommit(commit)
	return contract.v1
}

// LegacyUploadArtifactRelease reports the release label for admitted v1
// through v3 commits, which warrant an upgrade warning.
func LegacyUploadArtifactRelease(commit string) (string, bool) {
	if commit == UploadArtifactV1Commit || commit == UploadArtifactV2Commit || commit == UploadArtifactV3Commit {
		return uploadArtifactCommits[commit], true
	}
	return "", false
}

func validateUploadArtifactCommit(commit string) error {
	if !git.ValidObjectID(commit) || uploadArtifactUnsupportedCommits[commit] {
		return versionError("actions/upload-artifact", "native adapter", commit, supportedUploadArtifactContracts())
	}
	return nil
}

func supportedUploadArtifactContracts() []string {
	commits := make([]string, 0, len(uploadArtifactCommits)+2)
	for supported, version := range uploadArtifactCommits {
		commits = append(commits, version+" ("+supported+")")
	}
	sort.Strings(commits)
	return append(commits,
		"frozen upstream release and main snapshots (main "+uploadArtifactMainSnapshotCommit+")",
		"other lowercase 40-hex immutable commits via the "+UploadArtifactFallbackContractRelease+" fallback contract")
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
	contract, err := validateUploadArtifactInputNames(commit, inputs)
	if err != nil {
		return err
	}
	pathValue, ok := inputFold(inputs, "path")
	if !ok || strings.TrimSpace(pathValue) == "" {
		return fmt.Errorf("required input %q is missing", "path")
	}
	if evaluated || !uploadArtifactExpression(pathValue) {
		paths, err := UploadArtifactPaths(pathValue)
		if err != nil {
			return err
		}
		if contract.v1 && (len(paths) != 1 || strings.ContainsAny(paths[0], "*?[")) {
			return fmt.Errorf("input %q in actions/upload-artifact v1 must be one literal file or directory", "path")
		}
	}
	if contract.nameRequired {
		name, ok := inputFold(inputs, "name")
		if !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("required input %q is missing", "name")
		}
	}
	return validateUploadArtifactValues(inputs, evaluated)
}

func validateUploadArtifactValues(inputs map[string]string, evaluated bool) error {
	if value, ok := inputFold(inputs, "name"); ok && (evaluated || !uploadArtifactExpression(value)) {
		if err := ValidateUploadArtifactName(strings.TrimSpace(value)); err != nil {
			return err
		}
	}
	if value, ok := inputFold(inputs, "if-no-files-found"); ok && (evaluated || !uploadArtifactExpression(value)) && !uploadArtifactNoFilesValue(value) {
		return fmt.Errorf("input %q must be warn, error, or ignore", "if-no-files-found")
	}
	if value, ok := inputFold(inputs, "include-hidden-files"); ok && (evaluated || !uploadArtifactExpression(value)) && !actionBoolean(strings.TrimSpace(value)) {
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
	if value, ok := inputFold(inputs, "overwrite"); ok && (evaluated || !uploadArtifactExpression(value)) && !actionFalse(strings.TrimSpace(value)) {
		return fmt.Errorf("input %q may only be omitted or false; replacing artifacts is unsupported", "overwrite")
	}
	if value, ok := inputFold(inputs, "archive"); ok && (evaluated || !uploadArtifactExpression(value)) && !actionTrue(strings.TrimSpace(value)) {
		return fmt.Errorf("input %q may only be omitted or true; raw uploads are unsupported", "archive")
	}
	return nil
}

func validateUploadArtifactInputNames(commit string, inputs map[string]string) (uploadArtifactContract, error) {
	if err := validateUploadArtifactCommit(commit); err != nil {
		return uploadArtifactContract{}, err
	}
	contract, _ := uploadArtifactContractForCommit(commit)
	allowed := map[string]bool{"name": true, "path": true, "if-no-files-found": true, "include-hidden-files": true, "compression-level": true, "overwrite": true, "archive": true, "retention-days": true}
	seen := map[string]bool{}
	for _, name := range sortedNames(inputs) {
		lower := strings.ToLower(name)
		if seen[lower] {
			return uploadArtifactContract{}, fmt.Errorf("duplicate case-insensitive input %q is unsupported", name)
		}
		seen[lower] = true
		if !allowed[lower] {
			return uploadArtifactContract{}, fmt.Errorf("unknown input %q is unsupported by the bounded upload-artifact adapter", name)
		}
		if !contract.declaresInput(lower) {
			if lower == "archive" {
				return uploadArtifactContract{}, fmt.Errorf("input %q exists only in actions/upload-artifact v7", name)
			}
			return uploadArtifactContract{}, fmt.Errorf("explicit input %q is unsupported by this actions/upload-artifact release", name)
		}
	}
	return contract, nil
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
	for line := range strings.SplitSeq(value, "\n") {
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
