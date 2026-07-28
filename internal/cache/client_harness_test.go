package cache

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	actionsCacheV3Commit = "6f8efc29b200d32929f49075959781ed54ec270c"
	actionsCacheV4Commit = "0057852bfaa89a56745cba8c7296529d2fc39830"
)

// TestPinnedActionsCacheBundles is an opt-in network-free live-client harness.
// Point either environment variable at an existing checkout of the exact
// commit; the test never downloads or vendors action bundles. Each client
// saves through one directory-backend instance and restores through a fresh
// instance rooted at the same persistent directory.
func TestPinnedActionsCacheBundles(t *testing.T) {
	clients := []struct {
		name, environment, commit, nodeEnvironment string
	}{
		{name: "v3", environment: "BUILDKITE_GHA_ACTIONS_CACHE_V3", commit: actionsCacheV3Commit, nodeEnvironment: "BUILDKITE_GHA_NODE20"},
		{name: "v4", environment: "BUILDKITE_GHA_ACTIONS_CACHE_V4", commit: actionsCacheV4Commit, nodeEnvironment: "BUILDKITE_GHA_NODE24"},
	}
	ran := false
	for _, client := range clients {
		root := os.Getenv(client.environment)
		if root == "" {
			continue
		}
		ran = true
		t.Run(client.name, func(t *testing.T) {
			runPinnedClient(t, root, client.commit, client.name, client.nodeEnvironment)
		})
	}
	if !ran {
		t.Skip("set BUILDKITE_GHA_ACTIONS_CACHE_V3 and/or BUILDKITE_GHA_ACTIONS_CACHE_V4 to exact local checkouts")
	}
}

func runPinnedClient(t *testing.T, root, wantCommit, name, nodeEnvironment string) {
	t.Helper()
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != wantCommit {
		t.Fatalf("checkout HEAD = %q, %v, want %s", strings.TrimSpace(string(head)), err, wantCommit)
	}
	node := os.Getenv(nodeEnvironment)
	if node == "" {
		node, err = exec.LookPath("node")
	}
	if err != nil {
		t.Skip("node is not available")
	}
	for _, tool := range []string{"tar", "zstd"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available", tool)
		}
	}

	workspace := t.TempDir()
	source := filepath.Join(workspace, "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 256<<10)
	for index := range payload {
		payload[index] = byte(index*31 + 17)
	}
	if err := os.WriteFile(filepath.Join(source, "payload.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	runBundle := func(environment []string, path string) {
		t.Helper()
		command := exec.Command(node, filepath.Join(root, path))
		command.Dir = workspace
		command.Env = environment
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		if err := command.Run(); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, output.String())
		}
	}

	cacheRoot := t.TempDir()
	var operations []string
	runService := func(run func([]string)) {
		t.Helper()
		backend, err := NewExperimentalDirectoryBackend(cacheRoot)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(nil)
		baseURL := "http://" + server.Listener.Addr().String() + "/"
		handler, err := NewHandler(backend, Config{
			Token: "live-client-token", Session: "live-client-" + name, BaseURL: baseURL,
			TempDir: t.TempDir(), Namespace: Namespace{"organization", "cluster", "pipeline"},
			ReadScopes: []Scope{"branch", "default"}, WriteScope: "branch",
			MaxArchive: 16 << 20, MaxChunk: 2 << 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		recorder := &protocolRecorder{next: handler}
		server.Config.Handler = recorder
		server.Start()
		defer func() {
			server.Close()
			if err := handler.Close(); err != nil {
				t.Error(err)
			}
			operations = append(operations, recorder.operations()...)
		}()
		run(liveClientEnvironment(t, baseURL, workspace, source, "live-client-"+name))
	}

	runService(func(environment []string) {
		runBundle(environment, "dist/restore-only/index.js")
		runBundle(environment, "dist/save-only/index.js")
	})
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	runService(func(environment []string) {
		runBundle(environment, "dist/restore-only/index.js")
	})
	restored, err := os.ReadFile(filepath.Join(source, "payload.bin"))
	if err != nil || !bytes.Equal(restored, payload) {
		t.Fatalf("restored payload matches = %v, error = %v", bytes.Equal(restored, payload), err)
	}

	wantInOrder := []string{
		"GET /_apis/artifactcache/cache",
		"GET /_apis/artifactcache/caches",
		"POST /_apis/artifactcache/caches",
		"PATCH /_apis/artifactcache/caches/",
		"POST /_apis/artifactcache/caches/",
		"GET /_apis/artifactcache/cache",
		"GET /downloads/",
	}
	position := 0
	for _, operation := range operations {
		if position < len(wantInOrder) && strings.HasPrefix(operation, wantInOrder[position]) {
			position++
		}
	}
	if position != len(wantInOrder) {
		t.Fatalf("protocol operations = %q, missing ordered suffix %q", operations, wantInOrder[position:])
	}
}

func liveClientEnvironment(t *testing.T, baseURL, workspace, source, key string) []string {
	t.Helper()
	environment := slices.DeleteFunc(os.Environ(), func(value string) bool {
		name, _, _ := strings.Cut(value, "=")
		return name == "ACTIONS_CACHE_SERVICE_V2" || name == "ACTIONS_RESULTS_URL"
	})
	files := map[string]string{}
	for _, name := range []string{"GITHUB_ENV", "GITHUB_OUTPUT", "GITHUB_PATH", "GITHUB_STATE", "GITHUB_STEP_SUMMARY"} {
		path := filepath.Join(t.TempDir(), strings.ToLower(name))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		files[name] = path
	}
	environment = append(environment,
		"ACTIONS_CACHE_URL="+baseURL,
		"ACTIONS_RUNTIME_TOKEN=live-client-token",
		"ACTIONS_STEP_DEBUG=true",
		"GITHUB_ACTIONS=true",
		"GITHUB_EVENT_NAME=push",
		"GITHUB_REF=refs/heads/main",
		"GITHUB_REPOSITORY=buildkite/buildkite-gha",
		"GITHUB_SERVER_URL=https://github.com",
		"GITHUB_SHA=0123456789abcdef0123456789abcdef01234567",
		"GITHUB_WORKSPACE="+workspace,
		"INPUT_KEY="+key,
		"INPUT_PATH="+source,
		"INPUT_RESTORE-KEYS=",
		"INPUT_UPLOAD-CHUNK-SIZE=65536",
		"RUNNER_ARCH=X64",
		"RUNNER_DEBUG=1",
		"RUNNER_OS=Linux",
		"RUNNER_TEMP="+t.TempDir(),
	)
	for name, path := range files {
		environment = append(environment, name+"="+path)
	}
	return environment
}

type protocolRecorder struct {
	next       http.Handler
	mu         sync.Mutex
	transcript []string
}

func (r *protocolRecorder) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	r.transcript = append(r.transcript, request.Method+" "+request.URL.Path)
	r.mu.Unlock()
	r.next.ServeHTTP(w, request)
}

func (r *protocolRecorder) operations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.transcript)
}
