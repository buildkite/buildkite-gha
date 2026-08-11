// Package launcher installs and runs a verified buildkite-gha release.
package launcher

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	repositoryURL = "https://github.com/buildkite/buildkite-gha"
	archiveName   = "buildkite-gha_Linux_x86_64.tar.gz"
	maxChecksums  = 2 << 20
	maxArchive    = 128 << 20
	maxVersionOut = 1 << 10
)

var versionRE = regexp.MustCompile(`^(?:v)?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type config struct {
	env           []string
	getenv        func(string) string
	stderr        io.Writer
	stdout        io.Writer
	stdin         io.Reader
	client        *http.Client
	repositoryURL string
	goos, goarch  string
	tempDir       string
}

// Run executes the zero-argument stable launcher and returns the process exit code.
func Run(args []string) int {
	c := defaultConfig()
	if len(args) != 0 {
		_, _ = fmt.Fprintln(c.stderr, "buildkite-gha-launcher: arguments are not accepted")
		return 2
	}
	if err := c.run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return 128 + int(status.Signal())
			}
			return exitErr.ExitCode()
		}
		_, _ = fmt.Fprintf(c.stderr, "buildkite-gha-launcher: %v\n", err)
		return 1
	}
	return 0
}

func defaultConfig() config {
	return config{env: os.Environ(), getenv: os.Getenv, stderr: os.Stderr, stdout: os.Stdout, stdin: os.Stdin,
		client: &http.Client{Timeout: 45 * time.Second}, repositoryURL: repositoryURL,
		goos: runtime.GOOS, goarch: runtime.GOARCH, tempDir: os.TempDir()}
}

func parseSelector(s string) (string, error) {
	if s == "" || s == "latest" {
		return s, nil
	}
	if !versionRE.MatchString(s) {
		return "", fmt.Errorf("version must be latest or an exact stable semantic version")
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(strings.TrimPrefix(s, "v"), "%d.%d.%d", &major, &minor, &patch); err != nil || major == 0 && minor < 8 {
		return "", fmt.Errorf("version must be at least 0.8.0")
	}
	return fmt.Sprintf("v%d.%d.%d", major, minor, patch), nil
}

func (c config) run() error {
	if c.goos != "linux" || c.goarch != "amd64" {
		return fmt.Errorf("unsupported platform %s/%s (requires linux/amd64)", c.goos, c.goarch)
	}
	selector, err := parseSelector(c.getenv("BUILDKITE_PLUGIN_GITHUB_ACTIONS_VERSION"))
	if err != nil {
		return err
	}
	if selector == "" || selector == "latest" {
		selector, err = c.resolveLatest()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(c.stderr, "buildkite-gha-launcher: resolved latest to %s\n", selector)
	}
	version := strings.TrimPrefix(selector, "v")
	checksums, err := c.fetch(selector, "checksums.txt", maxChecksums)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	want, err := parseChecksum(checksums, archiveName)
	if err != nil {
		return err
	}
	root, persistent := c.cacheRoot()
	if root == "" {
		root, err = os.MkdirTemp(c.tempDir, "buildkite-gha-cache-")
		if err != nil {
			return fmt.Errorf("create temporary cache: %w", err)
		}
		defer func() { _ = os.RemoveAll(root) }()
	}
	cache, err := openCacheRoot(root)
	if err != nil {
		if persistent {
			_, _ = fmt.Fprintf(c.stderr, "buildkite-gha-launcher: warning: cache unavailable: %v; using temporary cache\n", err)
		}
		root, err = os.MkdirTemp(c.tempDir, "buildkite-gha-cache-")
		if err != nil {
			return fmt.Errorf("create temporary cache: %w", err)
		}
		defer func() { _ = os.RemoveAll(root) }()
		cache, err = openCacheRoot(root)
		if err != nil {
			return fmt.Errorf("open temporary cache: %w", err)
		}
	}
	defer func() { _ = cache.Close() }()
	entry := filepath.ToSlash(filepath.Join(selector, "Linux_x86_64", want, archiveName))
	archive, downloaded, err := c.privateArchive(cache, selector, want, entry)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(archive) }()
	runDir, err := os.MkdirTemp(c.tempDir, "buildkite-gha-run-")
	if err != nil {
		return fmt.Errorf("create run directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(runDir) }()
	if err := extract(archive, runDir); err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}
	binary := filepath.Join(runDir, "buildkite-gha")
	cmd := exec.Command(binary, "--version")
	cmd.Env = c.env
	var versionOutput boundedBuffer
	cmd.Stdout = &versionOutput
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || versionOutput.overflow || versionOutput.String() != "buildkite-gha "+version+"\n" {
		return fmt.Errorf("installed binary version verification failed")
	}
	if downloaded {
		if err := publish(cache, archive, entry); err != nil {
			_, _ = fmt.Fprintf(c.stderr, "buildkite-gha-launcher: warning: cache publication failed: %v\n", err)
		}
	}
	_, _ = fmt.Fprintln(c.stdout, "~~~ :github: Prepare workflow")
	cmd = exec.Command(binary, "plugin")
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = c.env, c.stdin, c.stdout, c.stderr
	return runForwarding(cmd)
}

type boundedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if b.Len()+len(p) > maxVersionOut {
		p = p[:max(0, maxVersionOut-b.Len())]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	return written, nil
}

func runForwarding(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	signal.Stop(sigs)
	return err
}

func (c config) resolveLatest() (string, error) {
	u := c.repositoryURL + "/releases/latest"
	client := *c.client
	hosts, scheme := c.redirectHosts(map[string]bool{"github.com": true})
	client.CheckRedirect = redirectPolicy(hosts, scheme, 5)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("create latest request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest returned %s", resp.Status)
	}
	prefix := c.repositoryURL + "/releases/tag/"
	if !strings.HasPrefix(resp.Request.URL.String(), prefix) {
		return "", fmt.Errorf("latest resolved outside canonical release tags")
	}
	tag, err := parseSelector(strings.TrimPrefix(resp.Request.URL.String(), prefix))
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(resp.Request.URL.String(), prefix), "v") {
		return "", fmt.Errorf("latest resolved to invalid tag")
	}
	return tag, nil
}

func (c config) redirectHosts(production map[string]bool) (map[string]bool, string) {
	u, err := url.Parse(c.repositoryURL)
	if err == nil && c.repositoryURL != repositoryURL {
		return map[string]bool{strings.ToLower(u.Hostname()): true}, u.Scheme
	}
	return production, "https"
}

func redirectPolicy(hosts map[string]bool, scheme string, max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= max {
			return fmt.Errorf("too many redirects")
		}
		port := req.URL.Port()
		if req.URL.Scheme != scheme || req.URL.User != nil || scheme == "https" && port != "" && port != "443" || !hosts[strings.ToLower(req.URL.Hostname())] {
			return fmt.Errorf("redirect to untrusted URL")
		}
		return nil
	}
}

func (c config) fetch(tag, name string, limit int64) ([]byte, error) {
	u := c.repositoryURL + "/releases/download/" + url.PathEscape(tag) + "/" + name
	client := *c.client
	hosts, scheme := c.redirectHosts(map[string]bool{"github.com": true, "objects.githubusercontent.com": true, "github-releases.githubusercontent.com": true, "release-assets.githubusercontent.com": true})
	client.CheckRedirect = redirectPolicy(hosts, scheme, 8)
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", name, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds size limit", name)
	}
	return b, nil
}

func parseChecksum(data []byte, name string) (string, error) {
	var found []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			if len(f[0]) != 64 {
				return "", fmt.Errorf("invalid checksum for %s", name)
			}
			if _, err := hex.DecodeString(f[0]); err != nil {
				return "", fmt.Errorf("invalid checksum for %s", name)
			}
			found = append(found, strings.ToLower(f[0]))
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("checksums must contain exactly one entry for %s", name)
	}
	return found[0], nil
}
