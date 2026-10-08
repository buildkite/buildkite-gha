package source

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolverMutableRefCachePersistsAcrossProcesses(t *testing.T) {
	if os.Getenv("GO_WANT_ACTION_REF_CACHE_HELPER") == "1" {
		resolver, err := NewResolver(nil, WithTestEndpoints(os.Getenv("TEST_ACTION_REF_API")))
		if err != nil {
			t.Fatal(err)
		}
		// Test endpoints disable the automatic user cache. Share only the
		// parent's temporary directory between these real resolver processes.
		resolver.cfg.mutableRefs, err = newMutableRefCache(os.Getenv("TEST_ACTION_REF_CACHE"), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := Parse("owner/repo@v1")
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := resolver.Resolve(t.Context(), ref)
		if err != nil || resolved.Commit != testSHA {
			t.Fatalf("Resolve() = %#v, %v; want commit %s", resolved, err, testSHA)
		}
		return
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/repos/owner/repo/git/ref/tags/v1" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, testSHA)
	}))
	defer server.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	for _, test := range []struct {
		name      string
		cache     string
		wantCalls int32
	}{
		{"cold", cache, 1},
		{"warm process", cache, 1},
		{"separate cache", t.TempDir(), 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), executable, "-test.run=^TestResolverMutableRefCachePersistsAcrossProcesses$")
			command.Env = append(os.Environ(),
				"GO_WANT_ACTION_REF_CACHE_HELPER=1",
				"TEST_ACTION_REF_API="+server.URL,
				"TEST_ACTION_REF_CACHE="+test.cache,
			)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("resolver process: %v\n%s", err, output)
			}
			if got := calls.Load(); got != test.wantCalls {
				t.Fatalf("GitHub requests = %d, want %d", got, test.wantCalls)
			}
		})
	}
}
