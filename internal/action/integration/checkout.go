//go:generate ../../../scripts/update-checkout-main-commits

package integration

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// CheckoutV1Commit through CheckoutV7Commit are the current audited release
	// implementations. CheckoutV1Commit, CheckoutV2Commit, and CheckoutV3Commit
	// are the final v1, v2, and v3 releases and are admitted exactly rather
	// than extending the v4-and-later main-branch snapshot.
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

// checkoutInputIntroduced records the earliest admitted release generation
// declaring each version-gated input. Inputs absent from this map are declared
// by every admitted release.
var checkoutInputIntroduced = map[string]int{
	"ssh-key":                   2,
	"ssh-known-hosts":           2,
	"ssh-strict":                2,
	"persist-credentials":       2,
	"set-safe-directory":        2,
	"allow-unsafe-pr-checkout":  2,
	"fetch-tags":                3,
	"sparse-checkout":           3,
	"sparse-checkout-cone-mode": 3,
	"github-server-url":         3,
	"filter":                    4,
	"show-progress":             4,
	"ssh-user":                  4,
}

func checkoutGeneration(commit string) int {
	switch commit {
	case CheckoutV1Commit:
		return 1
	case CheckoutV2Commit:
		return 2
	case CheckoutV3Commit:
		return 3
	}
	return 4
}

// CheckoutSupportsOutputs reports whether the admitted release declares the
// ref and commit outputs, added upstream in v4.2.0.
func CheckoutSupportsOutputs(commit string) bool {
	return checkoutGeneration(commit) >= 4
}

// CheckoutDefaultsToFullHistory reports whether the admitted release fetched
// full history when fetch-depth was omitted, as v1's runner plugin did.
func CheckoutDefaultsToFullHistory(commit string) bool {
	return commit == CheckoutV1Commit
}

// LegacyCheckoutRelease reports the admitted release label for the v1 and v2
// commits, which predate the v3.7.0 contract and warrant an upgrade warning.
func LegacyCheckoutRelease(commit string) (string, bool) {
	if commit == CheckoutV1Commit || commit == CheckoutV2Commit {
		return checkoutCommits[commit], true
	}
	return "", false
}

// validateCheckoutCommit admits known releases and a static snapshot of
// commits reachable from upstream main. Mutable references are resolved before
// this check, so changes after the snapshot are rejected until regeneration.
func validateCheckoutCommit(commit string) error {
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

// ValidateCheckoutInputs enforces the release-specific input contract
// implemented by the tokenless event-repository checkout adapter.
func ValidateCheckoutInputs(commit string, inputs map[string]string, repository, sha string) error {
	names := sortedNames(inputs)
	seen := make(map[string]bool, len(names))
	generation := checkoutGeneration(commit)
	for _, name := range names {
		value := inputs[name]
		normalized := strings.ToLower(name)
		if seen[normalized] {
			return fmt.Errorf("duplicate case-insensitive input %q is unsupported", name)
		}
		seen[normalized] = true
		if checkoutInputIntroduced[normalized] > generation {
			return fmt.Errorf("explicit input %q is unsupported by this actions/checkout release", name)
		}
		switch normalized {
		case "repository":
			if strings.EqualFold(value, repository) {
				continue
			}
		case "ref":
			if value == "" || ValidCheckoutSHA(value) || validCheckoutBranch(value) {
				continue
			}
		case "persist-credentials":
			if actionFalse(value) {
				continue
			}
		case "fetch-depth":
			if depth, err := strconv.ParseUint(value, 10, 31); err == nil && depth <= 1<<31-1 {
				continue
			}
		case "clean":
			if actionBoolean(value) {
				continue
			}
		case "set-safe-directory":
			if actionTrue(value) {
				continue
			}
		case "lfs":
			if actionBoolean(value) {
				continue
			}
		case "allow-unsafe-pr-checkout":
			if actionFalse(value) {
				continue
			}
		case "submodules":
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "", "false", "true", "recursive":
				continue
			}
		case "fetch-tags":
			if actionBoolean(value) {
				continue
			}
		case "show-progress":
			if actionBoolean(value) {
				continue
			}
		case "path":
			if value == "" || validCheckoutPath(value) {
				continue
			}
		case "ssh-key", "ssh-known-hosts":
			if value == "" {
				continue
			}
		case "sparse-checkout":
			if validSparseCheckout(value) {
				continue
			}
		case "filter":
			if validCheckoutFilter(value) {
				continue
			}
		case "sparse-checkout-cone-mode":
			if actionBoolean(value) {
				continue
			}
		case "ssh-strict":
			if actionTrue(value) {
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

// ValidCheckoutSHA reports whether value is a lowercase 40-hex Git commit ID.
func ValidCheckoutSHA(value string) bool {
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
