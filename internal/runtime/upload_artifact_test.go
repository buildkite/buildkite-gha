package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

type capturedArtifactUpload struct {
	root string
	path string
	data []byte
}

type captureArtifactUploader struct {
	uploads []capturedArtifactUpload
	err     error
}

func (u *captureArtifactUploader) DownloadArtifact(context.Context, string, string, string) error {
	return errors.New("unexpected artifact download")
}

func (u *captureArtifactUploader) UploadArtifactFrom(_ context.Context, root, path string) error {
	if u.err != nil {
		return u.err
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return err
	}
	u.uploads = append(u.uploads, capturedArtifactUpload{root: root, path: path, data: data})
	return nil
}

func TestUploadArtifactArchiveAndOutputs(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "out/visible.txt", "hello")
	writeFixtureFile(t, workspace, "out/nested/result.txt", "nested")
	writeFixtureFile(t, workspace, "out/.hidden", "secret")
	uploader := &captureArtifactUploader{}
	r := Runner{Artifacts: uploader, artifactRegistry: &artifactRegistry{names: map[string]bool{}}}
	result, err := r.runUploadArtifact(context.Background(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "out", "name": "logs", "compression-level": "0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(uploader.uploads) != 1 || result.Outputs["artifact-id"] == "" || len(result.Outputs["artifact-digest"]) != 64 || len(result.Artifacts) != 1 {
		t.Fatalf("unexpected upload result: %#v, uploads %#v", result, uploader.uploads)
	}
	upload := uploader.uploads[0]
	archiveDigest := sha256.Sum256(upload.data)
	if got := hex.EncodeToString(archiveDigest[:]); result.Outputs["artifact-digest"] != got || result.Artifacts[0].Digest != "sha256:"+got {
		t.Fatalf("artifact digest outputs = %#v, record = %#v, want %s", result.Outputs, result.Artifacts[0], got)
	}
	if result.Artifacts[0].Path != upload.path || result.Artifacts[0].Size != int64(len(upload.data)) || result.Artifacts[0].FileCount != 2 {
		t.Fatalf("artifact record = %#v, upload = %#v", result.Artifacts[0], upload)
	}
	if !strings.HasPrefix(upload.path, "buildkite-gha/v1/artifacts/") || !strings.HasSuffix(upload.path, ".zip") {
		t.Fatalf("native path = %q", upload.path)
	}
	entries := readUploadZIP(t, upload.data)
	want := map[string]string{"nested/result.txt": "nested", "visible.txt": "hello"}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("archive entries = %#v, want %#v", entries, want)
	}
}

func TestUploadArtifactCanIncludeHiddenFiles(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "out/.hidden", "included")
	uploader := &captureArtifactUploader{}
	r := Runner{Artifacts: uploader, artifactRegistry: &artifactRegistry{names: map[string]bool{}}}
	if _, err := r.runUploadArtifact(context.Background(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "out", "include-hidden-files": "TRUE"}); err != nil {
		t.Fatal(err)
	}
	if got := readUploadZIP(t, uploader.uploads[0].data); !reflect.DeepEqual(got, map[string]string{".hidden": "included"}) {
		t.Fatalf("archive entries = %#v", got)
	}
}

func TestUploadArtifactRejectsPathsTheConsumerCannotExtract(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "control\tcharacter", "unsafe")
	if _, err := collectUploadFiles(context.Background(), workspace, []string{"control\tcharacter"}, false); err == nil || !strings.Contains(err.Error(), "forbidden characters") {
		t.Fatalf("control character error = %v", err)
	}

	components := make([]string, 258)
	components[0] = "deep"
	for i := 1; i < len(components)-1; i++ {
		components[i] = "d"
	}
	components[len(components)-1] = "file"
	writeFixtureFile(t, workspace, filepath.Join(components...), "too deep")
	if _, err := collectUploadFiles(context.Background(), workspace, []string{"deep"}, false); err == nil || !strings.Contains(err.Error(), "forbidden characters") {
		t.Fatalf("deep path error = %v", err)
	}
}

func TestUploadArtifactArchiveRootUsesOnlyMatchedRoots(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "out/result.txt", "matched")
	uploader := &captureArtifactUploader{}
	r := Runner{Artifacts: uploader, artifactRegistry: &artifactRegistry{names: map[string]bool{}}}
	if _, err := r.runUploadArtifact(context.Background(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "out\nmissing"}); err != nil {
		t.Fatal(err)
	}
	if got := readUploadZIP(t, uploader.uploads[0].data); !reflect.DeepEqual(got, map[string]string{"result.txt": "matched"}) {
		t.Fatalf("archive entries = %#v", got)
	}
}

func TestUploadArtifactDoesNotStageInContainerWritableRunnerTemp(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "result.txt", "private staging")
	sharedRunnerTemp := t.TempDir()
	uploader := &captureArtifactUploader{}
	r := Runner{
		Artifacts: uploader, runnerTemp: sharedRunnerTemp,
		artifactRegistry: &artifactRegistry{names: map[string]bool{}},
	}
	if _, err := r.runUploadArtifact(context.Background(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "result.txt"}); err != nil {
		t.Fatal(err)
	}
	if len(uploader.uploads) != 1 {
		t.Fatalf("uploads = %#v, want one", uploader.uploads)
	}
	stagingParent, err := filepath.EvalSymlinks(filepath.Dir(uploader.uploads[0].root))
	if err != nil {
		t.Fatal(err)
	}
	tempDir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if uploader.uploads[0].root == sharedRunnerTemp || stagingParent != tempDir {
		t.Fatalf("upload staging root = %q, shared runner temp = %q", uploader.uploads[0].root, sharedRunnerTemp)
	}
}

func TestUploadArtifactNoFilesModes(t *testing.T) {
	const message = "No files were found with the provided path: missing. No artifacts will be uploaded."
	for _, mode := range []string{"warn", "ignore"} {
		t.Run(mode, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			processor := newCommandProcessor(&stdout, &stderr)
			r := Runner{artifactRegistry: &artifactRegistry{names: map[string]bool{}}}
			result, err := r.runUploadArtifact(context.Background(), processor, t.TempDir(), map[string]string{"path": "missing", "if-no-files-found": mode})
			if err != nil || len(result.Outputs) != 0 || len(result.Artifacts) != 0 || len(r.artifactRegistry.names) != 0 {
				t.Fatalf("runUploadArtifact() result = %#v, registry = %#v, error = %v", result, r.artifactRegistry.names, err)
			}
			warnings, _, commandErrors, _ := processor.workflowCommandAnnotations()
			if mode == "warn" && (!strings.Contains(warnings, message) || stdout.String() != "warning: "+message+"\n") {
				t.Fatalf("warn output = %q, annotation = %q", stdout.String(), warnings)
			}
			if mode == "ignore" && (warnings != "" || stdout.String() != message+"\n") {
				t.Fatalf("ignore output = %q, annotation = %q", stdout.String(), warnings)
			}
			if commandErrors != "" || stderr.Len() != 0 {
				t.Fatalf("unexpected no-file errors: output = %q, annotation = %q", stderr.String(), commandErrors)
			}
		})
	}
	var stdout bytes.Buffer
	processor := newCommandProcessor(&stdout, io.Discard)
	r := Runner{artifactRegistry: &artifactRegistry{names: map[string]bool{}}}
	if _, err := r.runUploadArtifact(context.Background(), processor, t.TempDir(), map[string]string{"path": "missing", "if-no-files-found": "error"}); err == nil || err.Error() != message {
		t.Fatalf("if-no-files-found error = %v", err)
	}
	_, _, commandErrors, _ := processor.workflowCommandAnnotations()
	if !strings.Contains(commandErrors, message) || stdout.String() != "error: "+message+"\n" {
		t.Fatalf("error output = %q, annotation = %q", stdout.String(), commandErrors)
	}
}

func TestUploadArtifactRejectsSymlinksMasksAndDuplicateNamesBeforeUpload(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "real/file", "x")
	if err := os.Symlink("real", filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	uploader := &captureArtifactUploader{}
	r := Runner{Artifacts: uploader, artifactRegistry: &artifactRegistry{names: map[string]bool{}}}
	processor := newCommandProcessor(io.Discard, io.Discard)
	if _, err := r.runUploadArtifact(context.Background(), processor, workspace, map[string]string{"path": "link/file"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
	processor.addMask("secret")
	if _, err := r.runUploadArtifact(context.Background(), processor, workspace, map[string]string{"path": "real/file", "name": "prefix-secret"}); err == nil || !strings.Contains(err.Error(), "registered mask") {
		t.Fatalf("masked-name error = %v", err)
	}
	processor = newCommandProcessor(io.Discard, io.Discard)
	if _, err := r.runUploadArtifact(context.Background(), processor, workspace, map[string]string{"path": "real/file", "name": "same"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.runUploadArtifact(context.Background(), processor, workspace, map[string]string{"path": "real/file", "name": "SAME"}); err == nil || len(uploader.uploads) != 1 {
		t.Fatalf("duplicate error = %v, uploads=%d", err, len(uploader.uploads))
	}
}

func TestUploadArtifactUploadFailureReleasesName(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "file", "x")
	uploader := &captureArtifactUploader{err: errors.New("upload failed")}
	r := Runner{Artifacts: uploader, artifactRegistry: &artifactRegistry{names: map[string]bool{}}}
	if _, err := r.runUploadArtifact(context.Background(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "file"}); err == nil {
		t.Fatal("upload failure was ignored")
	}
	if len(r.artifactRegistry.names) != 0 {
		t.Fatalf("failed upload retained registry reservation: %#v", r.artifactRegistry.names)
	}
}

func TestUploadArtifactCancellationStopsEveryArchiveStage(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "payload", strings.Repeat("x", 128*1024))
	files, err := collectUploadFiles(context.Background(), workspace, []string{"payload"}, false)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "payload.zip")
	digest, size, err := writeUploadZIP(context.Background(), archive, workspace, files, 0)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := collectUploadFiles(ctx, workspace, []string{"payload"}, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("collect cancellation = %v", err)
	}
	if _, _, err := writeUploadZIP(ctx, filepath.Join(t.TempDir(), "cancelled.zip"), workspace, files, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ZIP cancellation = %v", err)
	}
	if err := verifyUploadZIP(ctx, archive, digest, size); !errors.Is(err, context.Canceled) {
		t.Fatalf("verification cancellation = %v", err)
	}
	uploader := &captureArtifactUploader{}
	r := Runner{Artifacts: uploader, artifactRegistry: &artifactRegistry{names: map[string]bool{}}}
	if _, err := r.runUploadArtifact(ctx, newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "payload"}); !errors.Is(err, context.Canceled) || len(uploader.uploads) != 0 {
		t.Fatalf("adapter cancellation = %v, uploads = %d", err, len(uploader.uploads))
	}

	readCtx, cancelRead := context.WithCancel(context.Background())
	reader := contextReader{ctx: readCtx, reader: readerFunc(func(p []byte) (int, error) {
		cancelRead()
		return copy(p, "chunk"), nil
	})}
	var copied bytes.Buffer
	if n, err := io.Copy(&copied, reader); n != int64(len("chunk")) || !errors.Is(err, context.Canceled) || copied.String() != "chunk" {
		t.Fatalf("copy-loop cancellation = %d, %v, %q", n, err, copied.String())
	}
}

func TestUploadArtifactRejectsFIFOReplacementWithoutBlockingPastCancellation(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "payload", "regular before collection")
	files, err := collectUploadFiles(context.Background(), workspace, []string{"payload"}, false)
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(workspace, "payload")
	if err := os.Remove(payload); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(payload, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	archive := filepath.Join(t.TempDir(), "fifo.zip")
	go func() {
		_, _, err := writeUploadZIP(ctx, archive, workspace, files, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "non-regular file") {
			t.Fatalf("FIFO replacement error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("FIFO replacement blocked past cancellation")
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestUploadArtifactAdapterBypassesVerifiedUpstreamLifecycle(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/upload.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: upload proof\n")
	writeFixtureFile(t, workspace, "payload/result.txt", "payload")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action.yml", "name: upload artifact\nruns:\n  using: node24\n  pre: dist/pre.js\n  main: dist/main.js\n  post: dist/post.js\n")
	for _, phase := range []string{"pre", "main", "post"} {
		writeFixtureFile(t, remote, "dist/"+phase+".js", "throw new Error('adapter must not execute upstream JavaScript')\n")
	}
	digest, err := source.DigestTree(remote)
	if err != nil {
		t.Fatal(err)
	}
	lockID := "a-0000000000000001"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "upload", Kind: "uses", Uses: "actions/upload-artifact@" + actionintegration.UploadArtifactCommit,
		With:   map[string]string{"name": "payload", "path": "payload/result.txt", "if-no-files-found": "error"},
		Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Schema = plan.SchemaV3
	job.RequiredCapabilities = []string{"network"}
	job.Outputs = map[string]string{
		"artifact_id":     "${{ steps.upload.outputs.artifact-id }}",
		"artifact_digest": "${{ steps.upload.outputs.artifact-digest }}",
	}
	job.Actions = []plan.ActionLock{{
		ID: lockID, Source: "github", Repository: "actions/upload-artifact", RequestedRef: actionintegration.UploadArtifactCommit,
		Commit: actionintegration.UploadArtifactCommit, SourceDigest: digest,
	}}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: digest}}
	uploader := &captureArtifactUploader{}
	result, err := (Runner{Actions: materializer, Artifacts: uploader}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" || len(result.Artifacts) != 1 || result.Outputs["artifact_id"] == "" || len(result.Outputs["artifact_digest"]) != 64 {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if materializer.calls != 1 || len(uploader.uploads) != 1 {
		t.Fatalf("materializations = %d, uploads = %d", materializer.calls, len(uploader.uploads))
	}

	maskedJob := job
	maskedJob.Steps = append(append([]plan.Step(nil), job.Steps...), plan.Step{
		ID: "mask-after-upload", Kind: "run", Shell: "bash",
		Command: `echo "::add-mask::load"`,
	})
	maskedResult, err := (Runner{Actions: materializer, Artifacts: &captureArtifactUploader{}}).RunJob(context.Background(), maskedJob, workspace)
	if err == nil || !strings.Contains(err.Error(), "artifact name contains a registered secret") || len(maskedResult.Artifacts) != 0 || maskedResult.Conclusion != "failure" {
		t.Fatalf("late artifact-name mask result = %#v, error = %v", maskedResult, err)
	}

	job.Actions[0].Commit = strings.Repeat("b", 40)
	if _, err := (Runner{Actions: materializer, Artifacts: &captureArtifactUploader{}}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), actionintegration.UploadArtifactCommit) {
		t.Fatalf("unsupported runtime commit error = %v", err)
	}
}

func readUploadZIP(t *testing.T, data []byte) map[string]string {
	t.Helper()
	reader, err := zip.NewReader(strings.NewReader(string(data)), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]string, len(reader.File))
	for _, file := range reader.File {
		contents, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(contents)
		closeErr := contents.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read ZIP entry %q: %v, close: %v", file.Name, readErr, closeErr)
		}
		entries[file.Name] = string(data)
	}
	return entries
}
