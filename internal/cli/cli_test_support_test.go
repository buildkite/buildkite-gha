package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/transport"
)

const (
	cliTestBuildID       = "11111111-1111-4111-8111-111111111111"
	cliTestJobID         = "22222222-2222-4222-8222-222222222222"
	cliTestProducerJobID = "33333333-3333-4333-8333-333333333333"
)

func TestMain(m *testing.M) {
	// Ordinary CLI tests must never reach the ambient Agent API that a
	// Buildkite job inherits; tests that need it set these explicitly.
	for _, name := range []string{"BUILDKITE", "BUILDKITE_JOB_ID", "BUILDKITE_AGENT_ENDPOINT", "BUILDKITE_AGENT_ACCESS_TOKEN"} {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}

func requireImporterHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the importer requires linux/amd64")
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeUploadWorkflowRepository(t *testing.T, sources map[string]string) string {
	t.Helper()
	repository := canonicalTempDir(t)
	workflowDirectory := filepath.Join(repository, ".github", "workflows")
	if err := os.MkdirAll(workflowDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, source := range sources {
		if err := os.WriteFile(filepath.Join(workflowDirectory, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", repository}, {"-C", repository, "add", ".github/workflows"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	return repository
}

func writeUploadEvent(t *testing.T, directory, event, ref string, payload map[string]any) string {
	t.Helper()
	source, err := json.Marshal(map[string]any{
		"provider": "github",
		"event":    event,
		"repository": map[string]any{
			"owner": "buildkite",
			"name":  "buildkite-gha",
		},
		"ref":     ref,
		"sha":     strings.Repeat("a", 40),
		"actor":   "octocat",
		"payload": payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, event+".json")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFakeNode(t *testing.T, root string, major int) string {
	t.Helper()
	path := filepath.Join(root, fmt.Sprintf("node-%d-%d", major, len(root)))
	contents := fmt.Sprintf("#!/bin/sh\nprintf 'v%d.0.0\\n'\n", major)
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

type cliCommand struct {
	dir   string
	name  string
	args  []string
	stdin []byte
}

type cliCaptureRunner struct {
	commands       []cliCommand
	failAt         int
	failMetadata   bool
	failAnnotation bool
	gitOutput      []byte
	gitErr         error
	webhook        []byte
	webhookErr     error
	jobByStep      map[string]string
	dataByPath     map[string][]byte
	uploaded       map[string][]byte
	contextErrors  []error
}

type cliActionSourceTokenProvider struct {
	mu         sync.Mutex
	token      string
	err        error
	repository string
	calls      int
}

func (p *cliActionSourceTokenProvider) ActionSourceToken(_ context.Context, repository string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.repository = repository
	return p.token, p.err
}

type cliRedactor struct {
	mu     sync.Mutex
	values []string
	err    error
}

func (r *cliRedactor) AddRedaction(_ context.Context, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, value)
	return r.err
}

func (r *cliCaptureRunner) Run(ctx context.Context, dir, name string, args []string, stdin []byte) ([]byte, error) {
	r.commands = append(r.commands, cliCommand{dir: dir, name: name, args: append([]string(nil), args...), stdin: bytes.Clone(stdin)})
	r.contextErrors = append(r.contextErrors, ctx.Err())
	if r.failAt != 0 && len(r.commands) == r.failAt {
		return nil, errors.New("injected failure")
	}
	if name == "git" && slices.Equal(args, []string{"rev-parse", "HEAD"}) {
		return bytes.Clone(r.gitOutput), r.gitErr
	}
	if slices.Equal(args, []string{"meta-data", "get", "buildkite:webhook"}) {
		if r.webhookErr != nil {
			return nil, r.webhookErr
		}
		if r.webhook == nil {
			return nil, transport.ErrMetadataUnavailable
		}
		return bytes.Clone(r.webhook), nil
	}
	if len(args) >= 2 && args[0] == "artifact" && args[1] == "search" {
		return []byte(r.jobByStep[args[4]] + "\n"), nil
	}
	if len(args) >= 2 && args[0] == "artifact" && args[1] == "download" {
		contents, ok := r.dataByPath[args[2]]
		if !ok {
			return nil, errors.New("missing fixture artifact")
		}
		path := filepath.Join(args[3], filepath.FromSlash(args[2]))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(path, contents, 0o600)
	}
	if len(args) >= 3 && args[0] == "artifact" && args[1] == "upload" {
		if r.uploaded == nil {
			r.uploaded = map[string][]byte{}
		}
		if args[2] == ".buildkite-gha/**/*" {
			err := filepath.WalkDir(filepath.Join(dir, ".buildkite-gha"), func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				relative, err := filepath.Rel(dir, path)
				if err != nil {
					return err
				}
				contents, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				r.uploaded[filepath.ToSlash(relative)] = contents
				return nil
			})
			if err != nil {
				return nil, err
			}
		} else {
			contents, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(args[2])))
			if err != nil {
				return nil, err
			}
			r.uploaded[args[2]] = contents
		}
	}
	if r.failMetadata && len(args) >= 2 && args[0] == "meta-data" && args[1] == "set" {
		return nil, errors.New("metadata unavailable")
	}
	if r.failAnnotation && len(args) > 0 && args[0] == "annotate" {
		return nil, errors.New("annotation unavailable")
	}
	return nil, nil
}

var _ transport.Runner = (*cliCaptureRunner)(nil)

func cliTestRuntimeDigest() string {
	digest, err := executableDigest()
	if err != nil {
		panic(err)
	}
	return digest
}
