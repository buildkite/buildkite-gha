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
)

var checkoutCommits = map[string]string{
	CheckoutV3Commit:        "v3.7.0",
	CheckoutV4Commit:        "v4",
	CheckoutV5Commit:        "v5",
	CheckoutV6Commit:        "v6",
	CheckoutV7InitialCommit: "v7.0.0",
	CheckoutV7Commit:        "v7.0.1",
}

// validateCheckoutCommit admits known releases and a static snapshot of
// commits reachable from upstream main. Mutable references are resolved before
// this check, so changes after the snapshot remain fail-closed until regeneration.
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

// IsCheckoutV3 reports whether commit uses the audited v3.7.0 contract.
func IsCheckoutV3(commit string) bool {
	return commit == CheckoutV3Commit
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
	v3 := IsCheckoutV3(commit)
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
		case "clean", "set-safe-directory":
			if actionTrue(value) {
				continue
			}
		case "lfs", "allow-unsafe-pr-checkout":
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
			if !v3 && actionBoolean(value) {
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
			if !v3 && value == "" {
				continue
			}
		case "ssh-strict", "sparse-checkout-cone-mode":
			if actionTrue(value) {
				continue
			}
		case "ssh-user":
			if !v3 && value == "git" {
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
	return len(value) <= 255 && value != "." && value != ".." && !strings.EqualFold(value, ".git") && !strings.Contains(value, "/") && !strings.Contains(value, "\\") && !strings.ContainsAny(value, "\r\n\x00") && filepath.IsLocal(value)
}
