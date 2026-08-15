package integration

import "sort"

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

func validateCacheCommit(commit string) error {
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

// CacheCommits returns the complete immutable admission set.
func CacheCommits() []string {
	commits := make([]string, 0, len(cacheCommits))
	for commit := range cacheCommits {
		commits = append(commits, commit)
	}
	sort.Strings(commits)
	return commits
}
