//go:generate go run ./cmd/generate-cache-profiles

package integration

import (
	"sort"
	"strconv"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/git"
)

const (
	// Cache commits identify representative audited actions/cache releases admitted
	// to the Buildkite-backed cache-v2 service. CacheCommit retains the original
	// v6.1.0 spelling.
	CacheV3Commit   = "6f8efc29b200d32929f49075959781ed54ec270c"
	CacheV4Commit   = "0057852bfaa89a56745cba8c7296529d2fc39830"
	CacheV503Commit = "cdf6c1fa76f9f475f3d7449005a359c84ca0f306"
	CacheV5Commit   = "caa296126883cff596d87d8935842f9db880ef25"
	CacheCommit     = "55cc8345863c7cc4c66a329aec7e433d2d1c52a9"
)

// cacheCommits label the principal upstream releases named in diagnostics.
// The complete admission set is the generated frozen snapshot in
// cache_profiles_generated.go.
var cacheCommits = map[string]string{
	"f4b3439a656ba812b8cb417d2d49f9c810103092": "v3.4.0",
	"387e18722e6ff315b24a3b8b071feddd27b7bf7e": "v3.4.2",
	"2f8e54208210a422b2efd51efaa6bd6d7ca8920f": "v3.4.3",
	CacheV3Commit: "v3.5.0",
	"1bd1e32a3bdc45362d1e726936510720a7c30a57": "v4.2.0",
	"0c907a75c2c80ebcb7f088228285e798b750cf8f": "v4.2.1",
	"d4323d4df104b026a6aa633fdb11d772146be0bf": "v4.2.2",
	"5a3ec84eff668545956fd18022155c47e93e2684": "v4.2.3",
	"0400d5f644dc74513175e3cd8d07132dd4860809": "v4.2.4",
	CacheV4Commit: "v4.3.0",
	"a7833574556fa59680c1b7cb190c1735db73ebf0": "v5.0.0",
	"9255dc7a253b0ccc959486e2bca901246202afeb": "v5.0.1",
	"8b402f58fbc84540c8b491a91e594a4576fec3d7": "v5.0.2",
	CacheV503Commit: "v5.0.3",
	"668228422ae6a00e4ad889ee87cd7109ec5666a7": "v5.0.4",
	"27d5ce7f107fe9357f9df03efb73ab90386fccae": "v5.0.5",
	CacheV5Commit: "v5.1.0",
	"2c8a9bd7457de244a408f35966fab2fb45fda9c8": "v6.0.0",
	CacheCommit: "v6.1.0",
}

// cacheUnsupportedCommits are snapshot commits upstream withdrew after
// tagging them. They stay rejected even though their bundles are
// cache-v2-capable.
var cacheUnsupportedCommits = map[string]bool{
	"58c1e461ab4154b5b12d40cb0e84792b845ab8ba": true, // v3.4.1, replaced by v3.4.2
}

// cacheContract records which entry points of one immutable upstream commit
// run a bundled @actions/cache client that speaks the cache-v2 protocol the
// Buildkite cache service implements.
type cacheContract struct {
	client              string
	root, restore, save bool
}

func (c cacheContract) admits(entryPoint string) bool {
	switch entryPoint {
	case "":
		return c.root
	case "restore":
		return c.restore
	case "save":
		return c.save
	}
	return false
}

// validateCacheCommitFor admits one action entry point only when the exact
// commit is in the frozen snapshot. Unlike the native checkout and
// upload-artifact adapters, actions/cache runs the upstream bundle against
// the job-scoped cache token, so a commit outside the snapshot never runs.
// The compiler substitutes the release from SubstituteCacheCommit instead.
func validateCacheCommitFor(entryPoint string) func(string) error {
	return func(commit string) error {
		if !git.ValidObjectID(commit) || cacheUnsupportedCommits[commit] || !cacheCommitContracts[commit].admits(entryPoint) {
			return versionError("actions/cache", "Buildkite cache-v2 service", commit, supportedCacheContracts())
		}
		return nil
	}
}

// SubstituteCacheCommit selects the audited release that runs in place of a
// resolved actions/cache commit outside the frozen snapshot. It returns the
// newest principal release sharing the requested version ref's major, or the
// newest principal release overall when the ref names no admitted major.
func SubstituteCacheCommit(requestedRef string) (commit, release string) {
	requestedMajor, _, _ := strings.Cut(strings.TrimPrefix(requestedRef, "v"), ".")
	var newest, newestForMajor cacheRelease
	for candidate, label := range cacheCommits {
		release := parseCacheRelease(candidate, label)
		if release.newer(newest) {
			newest = release
		}
		if strconv.Itoa(release.major) == requestedMajor && release.newer(newestForMajor) {
			newestForMajor = release
		}
	}
	if newestForMajor.commit != "" {
		return newestForMajor.commit, newestForMajor.label
	}
	return newest.commit, newest.label
}

type cacheRelease struct {
	commit, label       string
	major, minor, patch int
}

func parseCacheRelease(commit, label string) cacheRelease {
	release := cacheRelease{commit: commit, label: label}
	parts := strings.Split(strings.TrimPrefix(label, "v"), ".")
	numbers := []*int{&release.major, &release.minor, &release.patch}
	for i := 0; i < len(parts) && i < len(numbers); i++ {
		*numbers[i], _ = strconv.Atoi(parts[i])
	}
	return release
}

func (r cacheRelease) newer(other cacheRelease) bool {
	if other.commit == "" {
		return true
	}
	if r.major != other.major {
		return r.major > other.major
	}
	if r.minor != other.minor {
		return r.minor > other.minor
	}
	return r.patch > other.patch
}

func supportedCacheContracts() []string {
	commits := make([]string, 0, len(cacheCommits)+1)
	for supported, version := range cacheCommits {
		commits = append(commits, version+" ("+supported+")")
	}
	sort.Strings(commits)
	return append(commits, "other cache-v2 commits in the frozen upstream release and main snapshots (main "+cacheMainSnapshotCommit+")")
}

// CacheCommits returns the complete immutable admission set: every snapshot
// commit whose root, restore, and save entry points all speak cache v2.
func CacheCommits() []string {
	commits := make([]string, 0, len(cacheCommitContracts))
	for commit, contract := range cacheCommitContracts {
		if !cacheUnsupportedCommits[commit] && contract.root && contract.restore && contract.save {
			commits = append(commits, commit)
		}
	}
	sort.Strings(commits)
	return commits
}
