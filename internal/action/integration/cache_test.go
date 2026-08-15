package integration

import (
	"strings"
	"testing"
)

func TestCacheCommitsAreExact(t *testing.T) {
	if len(CacheCommits()) != 19 {
		t.Fatalf("CacheCommits() has %d commits, want 19", len(CacheCommits()))
	}
	for _, commit := range CacheCommits() {
		if err := validateCacheCommit(commit); err != nil {
			t.Fatalf("audited commit %s rejected: %v", commit, err)
		}
	}
	if err := validateCacheCommit("58c1e461ab4154b5b12d40cb0e84792b845ab8ba"); err == nil {
		t.Fatal("upstream-withdrawn v3.4.1 commit accepted")
	}
	if err := validateCacheCommit(strings.Repeat("0", 40)); err == nil || !strings.Contains(err.Error(), "v3.4.0") || !strings.Contains(err.Error(), "v3.5.0") || !strings.Contains(err.Error(), CacheV3Commit) || !strings.Contains(err.Error(), "v4.3.0") || !strings.Contains(err.Error(), CacheV4Commit) || !strings.Contains(err.Error(), "v5.0.3") || !strings.Contains(err.Error(), CacheV503Commit) || !strings.Contains(err.Error(), "v5.1.0") || !strings.Contains(err.Error(), CacheV5Commit) || !strings.Contains(err.Error(), "v6.0.0") || !strings.Contains(err.Error(), "v6.1.0") || !strings.Contains(err.Error(), CacheCommit) {
		t.Fatalf("unrecognized cache commit error = %v", err)
	}
}
