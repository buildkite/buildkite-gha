package launcher

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestParseSelector(t *testing.T) {
	t.Parallel()
	tests := map[string]string{"": "", "latest": "latest", "0.8.0": "v0.8.0", "v1.2.3": "v1.2.3"}
	for input, want := range tests {
		got, err := parseSelector(input)
		if err != nil || got != want {
			t.Errorf("parseSelector(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"0.7.9", "v1.2.3-rc.1", "1.2", "1.2.3+meta", "../1.2.3", "01.2.3", "LATEST"} {
		if _, err := parseSelector(input); err == nil {
			t.Errorf("parseSelector(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseChecksumRequiresOneEntry(t *testing.T) {
	t.Parallel()
	h := strings.Repeat("a", 64)
	if got, err := parseChecksum([]byte(h+"  "+archiveName+"\n"), archiveName); err != nil || got != h {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, data := range []string{"", "bad  " + archiveName, h + "  " + archiveName + "\n" + h + "  " + archiveName} {
		if _, err := parseChecksum([]byte(data), archiveName); err == nil {
			t.Errorf("accepted %q", data)
		}
	}
}

func TestCacheRootPrecedence(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	env := map[string]string{"BUILDKITE_COMPUTE_TYPE": "hosted", "MISE_HOSTED_CACHE_VOLUME_ROOT": d, "BUILDKITE_AGENT_DATA_PATH": "/agent", "XDG_CACHE_HOME": "/xdg", "HOME": "/home"}
	c := config{getenv: func(k string) string { return env[k] }}
	got, _ := c.cacheRoot()
	if want := filepath.Join(d, "github-actions-buildkite-plugin"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	env["BUILDKITE_GITHUB_ACTIONS_PLUGIN_CACHE_ROOT"] = "/test-only"
	if got, _ := c.cacheRoot(); got != "/test-only" {
		t.Fatalf("override got %q", got)
	}
	delete(env, "BUILDKITE_GITHUB_ACTIONS_PLUGIN_CACHE_ROOT")
	delete(env, "BUILDKITE_COMPUTE_TYPE")
	if got, _ := c.cacheRoot(); got != "/agent/cache/github-actions-buildkite-plugin" {
		t.Fatalf("agent cache got %q", got)
	}
	delete(env, "BUILDKITE_AGENT_DATA_PATH")
	if got, _ := c.cacheRoot(); got != "/xdg/buildkite/github-actions-buildkite-plugin" {
		t.Fatalf("XDG cache got %q", got)
	}
	delete(env, "XDG_CACHE_HOME")
	if got, _ := c.cacheRoot(); got != "/home/.cache/buildkite/github-actions-buildkite-plugin" {
		t.Fatalf("home cache got %q", got)
	}
}

func TestOpenCacheRootRejectsSymlinkAncestor(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	link := filepath.Join(d, "link")
	target := t.TempDir()
	if err := os.Mkdir(filepath.Join(target, "cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if root, err := openCacheRoot(filepath.Join(link, "cache")); err == nil {
		_ = root.Close()
		t.Fatal("accepted symlink root")
	}
}

func TestRedirectPolicyRestrictsSchemeHostAndPort(t *testing.T) {
	t.Parallel()
	policy := redirectPolicy(map[string]bool{"github.com": true, "release-assets.githubusercontent.com": true}, "https", 3)
	for _, raw := range []string{"https://github.com/release", "https://release-assets.githubusercontent.com/asset", "https://github.com:443/release"} {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := policy(req, []*http.Request{{}}); err != nil {
			t.Errorf("policy rejected %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://github.com/release", "https://example.com/release", "https://github.com:8443/release", "https://user@github.com/release"} {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if policy(req, []*http.Request{{}}) == nil {
			t.Errorf("policy accepted %q", raw)
		}
	}
}

func TestTextBusyRetryIsBoundedAndErrorSpecific(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		err          error
		wantAttempts int
	}{
		{name: "text busy", err: syscall.ETXTBSY, wantAttempts: textBusyTries},
		{name: "other error", err: syscall.EACCES, wantAttempts: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			_, err := startWithTextBusyRetry(func() *exec.Cmd { return exec.Command("unused") }, func(*exec.Cmd) error {
				attempts++
				return test.err
			})
			if !errors.Is(err, test.err) || attempts != test.wantAttempts {
				t.Fatalf("retry = %v after %d attempts, want %v after %d", err, attempts, test.err, test.wantAttempts)
			}
		})
	}
}
