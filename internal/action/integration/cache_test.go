package integration

import (
	"strconv"
	"strings"
	"testing"
)

// cacheV341Commit is the withdrawn upstream v3.4.1 tag.
const cacheV341Commit = "58c1e461ab4154b5b12d40cb0e84792b845ab8ba"

func TestCacheCommitsAdmitPrincipalReleasesAndSnapshot(t *testing.T) {
	admitted := CacheCommits()
	if len(admitted) < 161 {
		t.Fatalf("CacheCommits() has %d commits, want at least 161", len(admitted))
	}
	for i := 1; i < len(admitted); i++ {
		if admitted[i-1] >= admitted[i] {
			t.Fatalf("CacheCommits() is not sorted and unique at %d", i)
		}
	}
	for _, entryPoint := range []string{"", "restore", "save"} {
		validate := validateCacheCommitFor(entryPoint)
		for _, commit := range admitted {
			if err := validate(commit); err != nil {
				t.Fatalf("admitted commit %s rejected for %q: %v", commit, entryPoint, err)
			}
		}
		for commit, version := range cacheCommits {
			if err := validate(commit); err != nil {
				t.Fatalf("principal release %s %s rejected for %q: %v", version, commit, entryPoint, err)
			}
		}
		if err := validate(cacheMainSnapshotCommit); err != nil {
			t.Fatalf("main snapshot tip rejected for %q: %v", entryPoint, err)
		}
	}
	if _, ok := cacheCommitContracts[cacheV341Commit]; !ok {
		t.Fatal("withdrawn v3.4.1 commit missing from snapshot; exclusion is untested")
	}
	for _, commit := range admitted {
		if commit == cacheV341Commit {
			t.Fatal("CacheCommits() includes the withdrawn v3.4.1 commit")
		}
	}
}

func TestCacheSnapshotContracts(t *testing.T) {
	for branch, tip := range cacheSnapshotTips {
		if _, ok := cacheCommitContracts[tip]; !ok {
			t.Errorf("snapshot branch %s tip %s has no admitted contract", branch, tip)
		}
	}
	if cacheSnapshotTips["main"] != cacheMainSnapshotCommit {
		t.Errorf("main tip %s != cacheMainSnapshotCommit %s", cacheSnapshotTips["main"], cacheMainSnapshotCommit)
	}
	for commit, contract := range cacheCommitContracts {
		if !contract.root || !contract.restore || !contract.save {
			t.Errorf("snapshotted commit %s admits only some entry points: %+v", commit, contract)
		}
		major, _, _ := strings.Cut(contract.client, ".")
		if n, err := strconv.Atoi(major); err != nil || n < 4 {
			t.Errorf("snapshotted commit %s bundles pre-cache-v2 client %q", commit, contract.client)
		}
	}
}

func TestCacheCommitRejectionsNameFrozenSnapshot(t *testing.T) {
	validate := validateCacheCommitFor("")
	for name, commit := range map[string]string{
		"withdrawn v3.4.1": cacheV341Commit,
		"unknown sha":      strings.Repeat("0", 40),
		"uppercase sha":    strings.ToUpper(CacheCommit),
		"short sha":        CacheCommit[:12],
		"tag":              "v6.1.0",
	} {
		err := validate(commit)
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		for _, want := range []string{"does not admit", "v3.4.0", "v3.5.0", CacheV3Commit, "v4.3.0", CacheV4Commit, "v5.0.3", CacheV503Commit, "v5.1.0", CacheV5Commit, "v6.0.0", "v6.1.0", CacheCommit, "frozen upstream release and main snapshots", cacheMainSnapshotCommit} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s error lacks %q: %v", name, want, err)
			}
		}
	}
	if err := validateCacheCommitFor("nested")(CacheCommit); err == nil {
		t.Fatal("unknown entry point accepted")
	}
}

func TestSubstituteCacheCommitPrefersNewestReleaseForRequestedMajor(t *testing.T) {
	for _, test := range []struct {
		ref, commit, release string
	}{
		{"v6", CacheCommit, "v6.1.0"},
		{"v6.0.0", CacheCommit, "v6.1.0"},
		{"v5", CacheV5Commit, "v5.1.0"},
		{"v4", CacheV4Commit, "v4.3.0"},
		{"v4.2.4", CacheV4Commit, "v4.3.0"},
		{"v3", CacheV3Commit, "v3.5.0"},
		{"v3.4.1", CacheV3Commit, "v3.5.0"},
		{"v2", CacheCommit, "v6.1.0"},
		{"v7", CacheCommit, "v6.1.0"},
		{"main", CacheCommit, "v6.1.0"},
		{"releases/v5", CacheCommit, "v6.1.0"},
		{strings.Repeat("0", 40), CacheCommit, "v6.1.0"},
		{"", CacheCommit, "v6.1.0"},
	} {
		commit, release := SubstituteCacheCommit(test.ref)
		if commit != test.commit || release != test.release {
			t.Errorf("SubstituteCacheCommit(%q) = %s %s, want %s %s", test.ref, commit, release, test.commit, test.release)
		}
		if validateCacheCommitFor("")(commit) != nil || validateCacheCommitFor("restore")(commit) != nil || validateCacheCommitFor("save")(commit) != nil {
			t.Errorf("SubstituteCacheCommit(%q) chose %s, which is not admitted for every entry point", test.ref, commit)
		}
	}
}
