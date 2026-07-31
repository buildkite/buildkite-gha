package runtime

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

type downloadStore struct {
	archive string
	path    string
	jobID   string
	extra   bool
}

func (s *downloadStore) UploadArtifactFrom(context.Context, string, string) error { return nil }
func (s *downloadStore) DownloadArtifact(_ context.Context, path, destination, jobID string) error {
	s.path = path
	s.jobID = jobID
	target := filepath.Join(destination, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(s.archive)
	if err != nil {
		return err
	}
	if err := os.WriteFile(target, b, 0o644); err != nil {
		return err
	}
	if s.extra {
		return os.WriteFile(filepath.Join(destination, "unexpected"), []byte("extra"), 0o644)
	}
	return nil
}

func testDownloadZIP(t *testing.T, names ...string) (string, int64, string) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "artifact.zip")
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for _, name := range names {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("payload")); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return filename, int64(len(b)), "sha256:" + hex.EncodeToString(sum[:])
}

func TestDownloadArtifactExactNeedAndDirectExtraction(t *testing.T) {
	archive, size, digest := testDownloadZIP(t, "nested/result.txt")
	store := &downloadStore{archive: archive}
	workspace := t.TempDir()
	processor := newCommandProcessor(io.Discard, io.Discard)
	need := plan.NeedArtifact{Name: "payload", ID: "42", Path: "buildkite-gha/v1/artifacts/" + strings.Repeat("a", 64) + ".zip", Digest: digest, Size: size, FileCount: 1, Producer: plan.NeedProducer{JobID: "11111111-1111-4111-8111-111111111111"}}
	result, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{need}}}, map[string]string{"name": "payload", "path": "out"})
	if err != nil {
		t.Fatal(err)
	}
	if store.path != need.Path || store.jobID != need.Producer.JobID {
		t.Fatalf("download identity = %q / %q", store.path, store.jobID)
	}
	if got, _ := os.ReadFile(filepath.Join(workspace, "out", "nested", "result.txt")); string(got) != "payload" {
		t.Fatalf("contents = %q", got)
	}
	want, _ := filepath.Abs(filepath.Join(workspace, "out"))
	if result.Outputs["download-path"] != want {
		t.Fatalf("download-path = %q", result.Outputs["download-path"])
	}
	if _, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{}, map[string]string{"name": "payload"}); err == nil || strings.Contains(err.Error(), "payload") {
		t.Fatal("missing artifact accepted")
	}
	if _, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{"a": {Artifacts: []plan.NeedArtifact{need}}, "b": {Artifacts: []plan.NeedArtifact{need}}}, map[string]string{"name": "payload"}); err == nil || strings.Contains(err.Error(), "payload") {
		t.Fatal("ambiguous artifact accepted")
	}
	if _, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{need}}}, map[string]string{"name": "Payload"}); err == nil || strings.Contains(err.Error(), "Payload") {
		t.Fatal("non-exact artifact name accepted")
	}
}

func TestDownloadArtifactRejectsMaskedNameWithoutDisclosure(t *testing.T) {
	const maskedName = "runtime-secret-artifact"
	processor := newCommandProcessor(io.Discard, io.Discard)
	processor.addMask(maskedName)
	_, err := (Runner{}).runDownloadArtifact(context.Background(), processor, t.TempDir(), nil, map[string]string{"name": maskedName})
	if err == nil || strings.Contains(err.Error(), maskedName) || !strings.Contains(err.Error(), "registered mask") {
		t.Fatalf("masked artifact lookup error = %v", err)
	}
}

func TestDownloadArtifactRejectsUnsafeZIPAndDigest(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "dir/../escape", "a\\b"} {
		t.Run(name, func(t *testing.T) {
			archive, _, _ := testDownloadZIP(t, name)
			if err := extractDownloadZIP(context.Background(), archive, t.TempDir(), ".", 1); err == nil {
				t.Fatal("unsafe member accepted")
			}
		})
	}
	archive, _, _ := testDownloadZIP(t, "A.txt", "a.txt")
	if err := extractDownloadZIP(context.Background(), archive, t.TempDir(), ".", 2); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("case-folding collision error = %v", err)
	}
	archive, size, _ := testDownloadZIP(t, "ok")
	if err := verifyDownloadDigest(context.Background(), archive, "sha256:"+strings.Repeat("0", 64), size); err == nil {
		t.Fatal("bad digest accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := verifyDownloadDigest(ctx, archive, "", size); err == nil {
		t.Fatal("cancellation ignored")
	}
	if err := extractDownloadZIP(ctx, archive, t.TempDir(), ".", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("extraction cancellation error = %v", err)
	}
}

func TestDownloadArtifactRejectsManifestAndDownloadMismatch(t *testing.T) {
	archive, size, digest := testDownloadZIP(t, "result.txt")
	base := plan.NeedArtifact{Name: "payload", Path: "buildkite-gha/v1/artifacts/" + strings.Repeat("b", 64) + ".zip", Digest: digest, Size: size, FileCount: 1, Producer: plan.NeedProducer{JobID: "11111111-1111-4111-8111-111111111111"}}
	for _, test := range []struct {
		name  string
		alter func(*plan.NeedArtifact, *downloadStore)
	}{
		{name: "size", alter: func(a *plan.NeedArtifact, _ *downloadStore) { a.Size++ }},
		{name: "file count", alter: func(a *plan.NeedArtifact, _ *downloadStore) { a.FileCount++ }},
		{name: "extra download", alter: func(_ *plan.NeedArtifact, s *downloadStore) { s.extra = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := base
			store := &downloadStore{archive: archive}
			test.alter(&artifact, store)
			_, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{artifact}}}, map[string]string{"name": "payload"})
			if err == nil {
				t.Fatal("mismatched download accepted")
			}
		})
	}
}

func TestDownloadArtifactDestinationCollisions(t *testing.T) {
	archive, _, _ := testDownloadZIP(t, "nested/result.txt")
	workspace := t.TempDir()
	regular := filepath.Join(workspace, "regular")
	if err := os.MkdirAll(filepath.Join(regular, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regular, "nested", "result.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractDownloadZIP(context.Background(), archive, workspace, "regular", 1); err != nil {
		t.Fatalf("replace regular file: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(regular, "nested", "result.txt")); err != nil || string(got) != "payload" {
		t.Fatalf("replaced contents = %q, %v", got, err)
	}

	for _, test := range []struct {
		name   string
		create func(string) error
	}{
		{name: "directory", create: func(name string) error { return os.MkdirAll(name, 0o755) }},
		{name: "symlink", create: func(name string) error { return os.Symlink("elsewhere", name) }},
		{name: "fifo", create: func(name string) error { return syscall.Mkfifo(name, 0o600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := filepath.Join(workspace, test.name)
			if err := os.MkdirAll(filepath.Join(destination, "nested"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := test.create(filepath.Join(destination, "nested", "result.txt")); err != nil {
				t.Fatal(err)
			}
			if err := extractDownloadZIP(context.Background(), archive, workspace, test.name, 1); err == nil {
				t.Fatal("non-regular destination accepted")
			}
		})
	}
}

func TestDownloadArtifactRejectsDestinationSymlinkEscape(t *testing.T) {
	archive, _, _ := testDownloadZIP(t, "result.txt")
	workspace, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := extractDownloadZIP(context.Background(), archive, workspace, "linked/out", 1); err == nil {
		t.Fatal("symlinked destination accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "out", "result.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside destination was mutated: %v", err)
	}
}

func TestDownloadArtifactRejectsUnsupportedMemberAndExpandedSize(t *testing.T) {
	for _, test := range []struct {
		name   string
		method uint16
		mode   os.FileMode
		size   uint64
	}{
		{name: "directory", method: zip.Store, mode: os.ModeDir | 0o755},
		{name: "symlink", method: zip.Store, mode: os.ModeSymlink | 0o777},
		{name: "method", method: 99, mode: 0o644},
		{name: "expanded size", method: zip.Store, mode: 0o644, size: uint64(transport.MaxResultArtifactSizeBytes) + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "raw.zip")
			out, err := os.Create(filename)
			if err != nil {
				t.Fatal(err)
			}
			zw := zip.NewWriter(out)
			header := &zip.FileHeader{Name: "entry", Method: test.method, UncompressedSize64: test.size, CompressedSize64: 0}
			header.SetMode(test.mode)
			if _, err := zw.CreateRaw(header); err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(zw.Close(), out.Close()); err != nil {
				t.Fatal(err)
			}
			if err := extractDownloadZIP(context.Background(), filename, t.TempDir(), ".", 1); err == nil {
				t.Fatal("unsupported ZIP member accepted")
			}
		})
	}
}

func TestDownloadArtifactAdapterBypassesVerifiedUpstreamLifecycle(t *testing.T) {
	archive, size, digest := testDownloadZIP(t, "result.txt")
	workspace := t.TempDir()
	workflowPath := ".github/workflows/download.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: download proof\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action.yml", "name: download artifact\nruns:\n  using: node20\n  pre: dist/pre.js\n  main: dist/main.js\n  post: dist/post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		writeFixtureFile(t, remote, "dist/"+phase+".js", "throw new Error('adapter must not execute upstream JavaScript')\n")
	}
	sourceDigest, err := source.DigestTree(remote)
	if err != nil {
		t.Fatal(err)
	}
	lockID := "a-0000000000000002"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "download", Kind: "uses", Uses: "actions/download-artifact@" + actionintegration.DownloadArtifactCommit,
		With:   map[string]string{"name": "payload", "path": "downloaded"},
		Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Schema = plan.SchemaV3
	job.RequiredCapabilities = []string{"network"}
	job.Outputs = map[string]string{"download_path": "${{ steps.download.outputs.download-path }}"}
	job.Actions = []plan.ActionLock{{
		ID: lockID, Source: "github", Repository: "actions/download-artifact", RequestedRef: actionintegration.DownloadArtifactCommit,
		Commit: actionintegration.DownloadArtifactCommit, SourceDigest: sourceDigest,
	}}
	artifact := plan.NeedArtifact{
		Name: "payload", ID: "42", Path: "buildkite-gha/v1/artifacts/" + strings.Repeat("c", 64) + ".zip",
		Digest: digest, Size: size, FileCount: 1,
		Producer: plan.NeedProducer{BuildID: "11111111-1111-4111-8111-111111111111", JobID: "22222222-2222-4222-8222-222222222222", StepKey: "gha-producer"},
	}
	job.Needs = map[string]plan.Need{"producer": {Result: "success", Artifacts: []plan.NeedArtifact{artifact}}}
	store := &downloadStore{archive: archive}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: sourceDigest}}
	result, err := (Runner{Actions: materializer, Artifacts: store}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["download_path"] != filepath.Join(workspace, "downloaded") {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if materializer.calls != 1 || store.path != artifact.Path || store.jobID != artifact.Producer.JobID {
		t.Fatalf("materializations/download = %d / %q / %q", materializer.calls, store.path, store.jobID)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "downloaded", "result.txt")); err != nil || string(got) != "payload" {
		t.Fatalf("downloaded contents = %q, %v", got, err)
	}

	job.Actions[0].Commit = strings.Repeat("b", 40)
	if _, err := (Runner{Actions: materializer, Artifacts: store}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), actionintegration.DownloadArtifactCommit) {
		t.Fatalf("unsupported runtime commit error = %v", err)
	}
}
