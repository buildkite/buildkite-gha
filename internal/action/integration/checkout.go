//go:generate go run ./cmd/generate-checkout-profiles

package integration

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/git"
)

const (
	// CheckoutV1Commit through CheckoutV7Commit identify principal release
	// contracts within the broader frozen upstream snapshots.
	// CheckoutV7InitialCommit is retained because it is pinned by the OSS
	// compatibility corpus; its later v7.0.1 changes do not affect the adapter's
	// bounded exact-event-SHA operation.
	CheckoutV1Commit        = "50fbc622fc4ef5163becd7fab6573eac35f8462e"
	CheckoutV2Commit        = "0717577d45739eb3c851188b29f50ed6c0b2194e"
	CheckoutV3Commit        = "a37ce9120846195fa4ece8f58b268e6043cb2f26"
	CheckoutV4Commit        = "11d5960a326750d5838078e36cf38b85af677262"
	CheckoutV5Commit        = "fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09"
	CheckoutV6Commit        = "d23441a48e516b6c34aea4fa41551a30e30af803"
	CheckoutV7InitialCommit = "9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0"
	CheckoutV7Commit        = "3d3c42e5aac5ba805825da76410c181273ba90b1"

	// CheckoutFallbackContractRelease identifies the stable contract used for
	// immutable commits absent from the frozen per-commit snapshot.
	CheckoutFallbackContractRelease = "v7.0.1"
)

var checkoutCommits = map[string]string{
	CheckoutV1Commit:        "v1.2.0",
	CheckoutV2Commit:        "v2.8.0",
	CheckoutV3Commit:        "v3.7.0",
	CheckoutV4Commit:        "v4",
	CheckoutV5Commit:        "v5",
	CheckoutV6Commit:        "v6",
	CheckoutV7InitialCommit: "v7.0.0",
	CheckoutV7Commit:        "v7.0.1",
}

// checkoutContract records the adapter-visible contract declared by one
// immutable upstream action manifest.
type checkoutContract struct {
	// inputs is a sorted, comma-separated set of names declared by the
	// immutable upstream action manifest.
	inputs                  string
	fullHistory             bool
	refOutput, commitOutput bool
}

type checkoutInputRule struct {
	valid func(value, repository string) bool
}

// checkoutInputRules owns the supported value validation for the bounded
// native adapter. checkoutCommitContracts owns each immutable commit's input
// declarations.
var checkoutInputRules = map[string]checkoutInputRule{
	"repository": {strings.EqualFold},
	"ref": {func(value, _ string) bool {
		return value == "" || git.ValidObjectID(value) || validCheckoutBranch(value)
	}},
	"fetch-depth": {func(value, _ string) bool {
		depth, err := strconv.ParseUint(value, 10, 31)
		return err == nil && depth <= 1<<31-1
	}},
	"clean": {func(value, _ string) bool { return actionBoolean(value) }},
	"lfs":   {func(value, _ string) bool { return actionBoolean(value) }},
	"submodules": {func(value, _ string) bool {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "false", "true", "recursive":
			return true
		}
		return false
	}},
	"path": {func(value, _ string) bool {
		return value == "" || validCheckoutPath(value)
	}},
	"ssh-key":                   {func(value, _ string) bool { return value == "" }},
	"ssh-known-hosts":           {func(value, _ string) bool { return value == "" }},
	"ssh-strict":                {func(value, _ string) bool { return actionTrue(value) }},
	"persist-credentials":       {func(value, _ string) bool { return actionFalse(value) }},
	"set-safe-directory":        {func(value, _ string) bool { return actionTrue(value) }},
	"allow-unsafe-pr-checkout":  {func(value, _ string) bool { return actionFalse(value) }},
	"fetch-tags":                {func(value, _ string) bool { return actionBoolean(value) }},
	"sparse-checkout":           {func(value, _ string) bool { return validSparseCheckout(value) }},
	"sparse-checkout-cone-mode": {func(value, _ string) bool { return actionBoolean(value) }},
	"github-server-url": {func(value, _ string) bool {
		return value == "" || value == "https://github.com"
	}},
	"filter":        {func(value, _ string) bool { return validCheckoutFilter(value) }},
	"show-progress": {func(value, _ string) bool { return actionBoolean(value) }},
	"ssh-user":      {func(value, _ string) bool { return value == "git" }},
}

func (c checkoutContract) declaresInput(name string) bool {
	return strings.Contains(","+c.inputs+",", ","+name+",")
}

func checkoutContractForCommit(commit string) (checkoutContract, bool) {
	contract, exact := checkoutCommitContracts[commit]
	if exact {
		return contract, true
	}
	if !git.ValidObjectID(commit) {
		return checkoutContract{}, false
	}
	return checkoutCommitContracts[CheckoutV7Commit], false
}

// CheckoutUsesFallbackContract reports whether an immutable commit is absent
// from the frozen snapshot and therefore uses the stable fallback contract.
func CheckoutUsesFallbackContract(commit string) bool {
	if !git.ValidObjectID(commit) {
		return false
	}
	_, exact := checkoutCommitContracts[commit]
	return !exact
}

// CheckoutSupportsOutputs reports whether the selected exact or fallback
// contract declares the ref and commit outputs, added upstream in v4.2.0.
func CheckoutSupportsOutputs(commit string) bool {
	contract, _ := checkoutContractForCommit(commit)
	return contract.refOutput && contract.commitOutput
}

// CheckoutDefaultsToFullHistory reports whether the selected exact or fallback
// contract fetches full history when fetch-depth is omitted.
func CheckoutDefaultsToFullHistory(commit string) bool {
	contract, _ := checkoutContractForCommit(commit)
	return contract.fullHistory
}

// LegacyCheckoutRelease reports the admitted release label for the v1 and v2
// commits, which predate the v3.7.0 contract and warrant an upgrade warning.
func LegacyCheckoutRelease(commit string) (string, bool) {
	if commit == CheckoutV1Commit || commit == CheckoutV2Commit {
		return checkoutCommits[commit], true
	}
	return "", false
}

// validateCheckoutCommit admits immutable commits. Commits captured by the
// frozen snapshots retain their exact contract; other valid SHAs use the
// stable fallback contract.
func validateCheckoutCommit(commit string) error {
	if !git.ValidObjectID(commit) {
		return versionError("actions/checkout", "native adapter", commit, supportedCheckoutContracts())
	}
	return nil
}

func supportedCheckoutContracts() []string {
	return append(sortedCheckoutCommits(),
		"frozen upstream release and main snapshots (main "+checkoutMainSnapshotCommit+")",
		"other lowercase 40-hex immutable commits via the "+CheckoutFallbackContractRelease+" fallback contract")
}

func sortedCheckoutCommits() []string {
	commits := make([]string, 0, len(checkoutCommits))
	for commit, version := range checkoutCommits {
		commits = append(commits, version+" ("+commit+")")
	}
	sort.Strings(commits)
	return commits
}

// ValidateCheckoutInputs enforces the release-specific input contract
// implemented by the tokenless event-repository checkout adapter.
func ValidateCheckoutInputs(commit string, inputs map[string]string, repository, sha string) error {
	contract, exact := checkoutContractForCommit(commit)
	if !exact && !git.ValidObjectID(commit) {
		return versionError("actions/checkout", "native adapter", commit, supportedCheckoutContracts())
	}
	names := sortedNames(inputs)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		value := inputs[name]
		normalized := strings.ToLower(name)
		if seen[normalized] {
			return fmt.Errorf("duplicate case-insensitive input %q is unsupported", name)
		}
		seen[normalized] = true
		if !contract.declaresInput(normalized) {
			return fmt.Errorf("explicit input %q is unsupported by this actions/checkout release", name)
		}
		rule, ok := checkoutInputRules[normalized]
		if !ok {
			return fmt.Errorf("explicit input %q value is unsupported", name)
		}
		if !rule.valid(value, repository) {
			return fmt.Errorf("explicit input %q value is unsupported", name)
		}
	}
	return nil
}

func validCheckoutBranch(value string) bool {
	if after, ok := strings.CutPrefix(value, "refs/heads/"); ok {
		value = after
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
	for segment := range strings.SplitSeq(value, "/") {
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

func validCheckoutPath(value string) bool {
	if len(value) > 1024 || value == "." || value == ".." || strings.Contains(value, "\\") || strings.ContainsAny(value, "\r\n\x00") || !filepath.IsLocal(value) {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

func validCheckoutFilter(value string) bool {
	return value == "" || len(value) <= 1024 && !strings.ContainsAny(value, "\r\n\x00")
}

func validSparseCheckout(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 1<<20 || strings.ContainsRune(value, '\x00') {
		return false
	}
	lines := 0
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	return lines > 0 && lines <= 1000
}
