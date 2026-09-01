package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
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

func (r *jobRun) runUploadArtifact(ctx context.Context, processor *commandProcessor, workspace string, inputs map[string]string) (Result, error) {
	return r.runUploadArtifactCommit(ctx, processor, workspace, actionintegration.UploadArtifactCommit, inputs)
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
	r := newJobRun(Runner{Artifacts: uploader})
	result, err := r.runUploadArtifact(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "out", "name": "logs", "compression-level": "0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(uploader.uploads) != 1 || result.Outputs["artifact-id"] == "" || len(result.Outputs["artifact-digest"]) != 64 || result.Outputs["artifact-url"] != "" || len(result.Artifacts) != 1 {
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

func TestUploadArtifactRuntimeVersionMatrix(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "payload", "versioned")
	for _, test := range []struct {
		name        string
		commit      string
		inputs      map[string]string
		wantOutputs bool
	}{
		{name: "v1.0.0", commit: actionintegration.UploadArtifactV1Commit, inputs: map[string]string{"name": "v1", "path": "payload"}},
		{name: "v2.3.1 defaults", commit: actionintegration.UploadArtifactV2Commit, inputs: map[string]string{"path": "payload"}},
		{name: "v3.2.1 defaults", commit: actionintegration.UploadArtifactV3Commit, inputs: map[string]string{"path": "payload"}},
		{name: "v4.6.2 defaults", commit: actionintegration.UploadArtifactCommit, inputs: map[string]string{"path": "payload"}, wantOutputs: true},
		{name: "v5.0.0 defaults", commit: actionintegration.UploadArtifactV5Commit, inputs: map[string]string{"path": "./payload"}, wantOutputs: true},
		{name: "v6.0.0 defaults", commit: actionintegration.UploadArtifactV6Commit, inputs: map[string]string{"path": "./payload", "retention-days": "0"}, wantOutputs: true},
		{name: "v7.0.1 ZIP", commit: actionintegration.UploadArtifactV7Commit, inputs: map[string]string{"path": "payload", "archive": " true ", "name": "v7"}, wantOutputs: true},
		{name: "unknown commit v7 fallback", commit: strings.Repeat("0", 40), inputs: map[string]string{"path": "payload", "archive": "true"}, wantOutputs: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			uploader := &captureArtifactUploader{}
			r := newJobRun(Runner{Artifacts: uploader})
			result, err := r.runUploadArtifactCommit(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, test.commit, test.inputs)
			if err != nil || len(uploader.uploads) != 1 || len(result.Artifacts) != 1 {
				t.Fatalf("runtime matrix result = %#v, uploads = %d, error = %v", result, len(uploader.uploads), err)
			}
			if test.wantOutputs && (result.Outputs["artifact-id"] == "" || result.Outputs["artifact-digest"] == "" || result.Outputs["artifact-url"] != "") {
				t.Fatalf("runtime matrix outputs = %#v", result.Outputs)
			}
			if !test.wantOutputs && len(result.Outputs) != 0 {
				t.Fatalf("legacy runtime outputs = %#v, want none", result.Outputs)
			}
			reader, err := zip.NewReader(bytes.NewReader(uploader.uploads[0].data), int64(len(uploader.uploads[0].data)))
			if err != nil {
				t.Fatal(err)
			}
			if len(reader.File) != 1 || reader.File[0].Method != zip.Deflate {
				t.Fatalf("default ZIP entries = %#v", reader.File)
			}
		})
	}

	r := newJobRun(Runner{Artifacts: &captureArtifactUploader{}})
	if _, err := r.runUploadArtifactCommit(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, actionintegration.UploadArtifactCommit, map[string]string{"path": "payload", "archive": "true"}); err == nil || !strings.Contains(err.Error(), "only in actions/upload-artifact v7") {
		t.Fatalf("runtime v4 archive mismatch error = %v", err)
	}
}

func TestUploadArtifactLegacyReleaseBehavior(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "payload/visible.txt", "visible")
	writeFixtureFile(t, workspace, "payload/.hidden.txt", "hidden")
	if err := os.Mkdir(filepath.Join(workspace, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		commit string
		inputs map[string]string
		want   map[string]string
	}{
		{name: "v1 includes hidden files", commit: actionintegration.UploadArtifactV1Commit, inputs: map[string]string{"name": "v1", "path": "payload"}, want: map[string]string{".hidden.txt": "hidden", "visible.txt": "visible"}},
		{name: "v2 includes hidden files", commit: actionintegration.UploadArtifactV2Commit, inputs: map[string]string{"path": "payload"}, want: map[string]string{".hidden.txt": "hidden", "visible.txt": "visible"}},
		{name: "v3 excludes hidden files", commit: actionintegration.UploadArtifactV3Commit, inputs: map[string]string{"path": "payload"}, want: map[string]string{"visible.txt": "visible"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			uploader := &captureArtifactUploader{}
			r := newJobRun(Runner{Artifacts: uploader})
			result, err := r.runUploadArtifactCommit(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, test.commit, test.inputs)
			if err != nil || len(uploader.uploads) != 1 || len(result.Artifacts) != 1 || len(result.Outputs) != 0 {
				t.Fatalf("legacy upload result = %#v, uploads = %d, error = %v", result, len(uploader.uploads), err)
			}
			if got := readUploadZIP(t, uploader.uploads[0].data); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("archive entries = %#v, want %#v", got, test.want)
			}
		})
	}

	r := newJobRun(Runner{Artifacts: &captureArtifactUploader{}})
	if _, err := r.runUploadArtifactCommit(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, actionintegration.UploadArtifactV1Commit, map[string]string{"name": "missing", "path": "missing"}); err == nil || !strings.Contains(err.Error(), "No files were found") {
		t.Fatalf("v1 missing path error = %v", err)
	}

	uploader := &captureArtifactUploader{}
	r = newJobRun(Runner{Artifacts: uploader})
	result, err := r.runUploadArtifactCommit(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, actionintegration.UploadArtifactV1Commit, map[string]string{"name": "empty", "path": "empty"})
	if err != nil || len(uploader.uploads) != 1 || len(result.Artifacts) != 1 || result.Artifacts[0].FileCount != 0 || len(readUploadZIP(t, uploader.uploads[0].data)) != 0 {
		t.Fatalf("v1 empty directory result = %#v, uploads = %d, error = %v", result, len(uploader.uploads), err)
	}
}

func TestUploadArtifactBoundedGlobAndAdvisoryRetention(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "tests/first.log", "first")
	writeFixtureFile(t, workspace, "tests/second.txt", "second")
	writeFixtureFile(t, workspace, "tests/.hidden.log", "hidden")
	var stdout bytes.Buffer
	uploader := &captureArtifactUploader{}
	r := newJobRun(Runner{Artifacts: uploader})
	result, err := r.runUploadArtifact(t.Context(), newCommandProcessor(&stdout, io.Discard), workspace, map[string]string{
		"path": "tests/*.log", "retention-days": "7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := readUploadZIP(t, uploader.uploads[0].data); !reflect.DeepEqual(got, map[string]string{"first.log": "first"}) {
		t.Fatalf("archive entries = %#v", got)
	}
	if !strings.Contains(stdout.String(), "retention-days: 7 is advisory") || len(result.Artifacts) != 1 {
		t.Fatalf("retention warning = %q, result = %#v", stdout.String(), result)
	}

	if err := os.Mkdir(filepath.Join(workspace, "tests", "directory.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.runUploadArtifact(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{
		"path": "tests/*.log", "name": "directory-match",
	}); err != nil {
		t.Fatal(err)
	}
	if got := readUploadZIP(t, uploader.uploads[1].data); !reflect.DeepEqual(got, map[string]string{"first.log": "first"}) {
		t.Fatalf("directory-matching glob entries = %#v", got)
	}
}

func TestUploadArtifactNormalizesFailurePathDirectories(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "log/test.log", "log")
	writeFixtureFile(t, workspace, "tmp/capybara/failure.png", "screenshot")
	for _, test := range []struct {
		name, path string
		want       map[string]string
	}{
		{name: "logs", path: "log/", want: map[string]string{"test.log": "log"}},
		{name: "screenshots", path: "tmp/capybara/", want: map[string]string{"failure.png": "screenshot"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			uploader := &captureArtifactUploader{}
			r := newJobRun(Runner{Artifacts: uploader})
			_, err := r.runUploadArtifactCommit(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, actionintegration.UploadArtifactV6Commit, map[string]string{
				"name": test.name, "path": test.path,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := readUploadZIP(t, uploader.uploads[0].data); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("archive entries = %#v, want %#v", got, test.want)
			}
		})
	}

	writeFixtureFile(t, workspace, "report.txt", "not a directory")
	uploader := &captureArtifactUploader{}
	r := newJobRun(Runner{Artifacts: uploader})
	if _, err := r.runUploadArtifactCommit(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, actionintegration.UploadArtifactV6Commit, map[string]string{
		"name": "directory-only", "path": "report.txt/", "if-no-files-found": "error",
	}); err == nil || !strings.Contains(err.Error(), "No files were found") || len(uploader.uploads) != 0 {
		t.Fatalf("trailing-slash file match error = %v, uploads = %d", err, len(uploader.uploads))
	}
}

func TestUploadArtifactZIPIsDeterministicAtSupportedCompressionLevels(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "payload/a.txt", strings.Repeat("compressible", 100))
	writeFixtureFile(t, workspace, "payload/b.txt", "second")
	files, err := collectUploadFiles(t.Context(), workspace, []string{"payload"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range []int{0, 1, 6, 9} {
		t.Run(strconv.Itoa(level), func(t *testing.T) {
			first, second := filepath.Join(t.TempDir(), "first.zip"), filepath.Join(t.TempDir(), "second.zip")
			firstDigest, firstSize, err := writeUploadZIP(t.Context(), first, workspace, files, level)
			if err != nil {
				t.Fatal(err)
			}
			secondDigest, secondSize, err := writeUploadZIP(t.Context(), second, workspace, files, level)
			if err != nil {
				t.Fatal(err)
			}
			firstBytes, _ := os.ReadFile(first)
			secondBytes, _ := os.ReadFile(second)
			if firstDigest != secondDigest || firstSize != secondSize || !bytes.Equal(firstBytes, secondBytes) {
				t.Fatalf("level %d ZIP changed across identical writes", level)
			}
			reader, err := zip.NewReader(bytes.NewReader(firstBytes), int64(len(firstBytes)))
			if err != nil {
				t.Fatal(err)
			}
			wantMethod := uint16(zip.Deflate)
			if level == 0 {
				wantMethod = zip.Store
			}
			for _, file := range reader.File {
				if file.Method != wantMethod {
					t.Fatalf("level %d ZIP method = %d, want %d", level, file.Method, wantMethod)
				}
			}
		})
	}
}

func TestUploadArtifactCanIncludeHiddenFiles(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "out/.hidden", "included")
	uploader := &captureArtifactUploader{}
	r := newJobRun(Runner{Artifacts: uploader})
	if _, err := r.runUploadArtifact(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "out", "include-hidden-files": "TRUE"}); err != nil {
		t.Fatal(err)
	}
	if got := readUploadZIP(t, uploader.uploads[0].data); !reflect.DeepEqual(got, map[string]string{".hidden": "included"}) {
		t.Fatalf("archive entries = %#v", got)
	}
}

func TestCollectUploadFilesExpandsBoundedGlobs(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "core/build/reports/check.html", "check")
	writeFixtureFile(t, workspace, "core/build/reports/check.txt", "not selected")
	writeFixtureFile(t, workspace, ".hidden/build/reports/hidden.html", "hidden")
	writeFixtureFile(t, workspace, "clients/build/reports/tests/unit/index.html", "unit")

	files, err := collectUploadFiles(t.Context(), workspace, []string{"**/build/**/*.html"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range files {
		names = append(names, file.name)
	}
	want := []string{"clients/build/reports/tests/unit/index.html", "core/build/reports/check.html"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("recursive glob files = %#v, want %#v", names, want)
	}

	files, err = collectUploadFiles(t.Context(), workspace, []string{"**/build/reports/tests/*"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].name != "clients/build/reports/tests/unit/index.html" {
		t.Fatalf("directory-matching glob files = %#v", files)
	}
}

func TestCollectUploadFilesGlobRejectsMatchedSymlink(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "outside/report.html", "report")
	if err := os.Symlink(filepath.Join(workspace, "outside", "report.html"), filepath.Join(workspace, "report.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := collectUploadFiles(t.Context(), workspace, []string{"*.html"}, false); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("matched glob symlink error = %v", err)
	}
}

func TestUploadArtifactRejectsPathsTheConsumerCannotExtract(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "control\tcharacter", "unsafe")
	if _, err := collectUploadFiles(t.Context(), workspace, []string{"control\tcharacter"}, false); err == nil || !strings.Contains(err.Error(), "forbidden characters") {
		t.Fatalf("control character error = %v", err)
	}

	components := make([]string, 258)
	components[0] = "deep"
	for i := 1; i < len(components)-1; i++ {
		components[i] = "d"
	}
	components[len(components)-1] = "file"
	writeFixtureFile(t, workspace, filepath.Join(components...), "too deep")
	if _, err := collectUploadFiles(t.Context(), workspace, []string{"deep"}, false); err == nil || !strings.Contains(err.Error(), "forbidden characters") {
		t.Fatalf("deep path error = %v", err)
	}
}

func TestUploadArtifactArchiveRootUsesOnlyMatchedRoots(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "out/result.txt", "matched")
	uploader := &captureArtifactUploader{}
	r := newJobRun(Runner{Artifacts: uploader})
	if _, err := r.runUploadArtifact(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "out\nmissing"}); err != nil {
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
	r := newJobRun(Runner{Artifacts: uploader})
	r.runnerTemp = sharedRunnerTemp
	if _, err := r.runUploadArtifact(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "result.txt"}); err != nil {
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
			r := newJobRun(Runner{})
			result, err := r.runUploadArtifact(t.Context(), processor, t.TempDir(), map[string]string{"path": "missing", "if-no-files-found": mode})
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
	r := newJobRun(Runner{})
	if _, err := r.runUploadArtifact(t.Context(), processor, t.TempDir(), map[string]string{"path": "missing", "if-no-files-found": "error"}); err == nil || err.Error() != message {
		t.Fatalf("if-no-files-found error = %v", err)
	}
	_, _, commandErrors, _ := processor.workflowCommandAnnotations()
	if !strings.Contains(commandErrors, message) || stdout.String() != "error: "+message+"\n" {
		t.Fatalf("error output = %q, annotation = %q", stdout.String(), commandErrors)
	}
}

func TestUploadArtifactScrubsExpressionDerivedPathsFromErrors(t *testing.T) {
	const maskedPath = "runtime-secret-path"
	processor := newCommandProcessor(io.Discard, io.Discard)
	processor.addMask(maskedPath)
	r := newJobRun(Runner{})
	if _, err := r.runUploadArtifact(t.Context(), processor, t.TempDir(), map[string]string{
		"path": maskedPath, "if-no-files-found": "error",
	}); err == nil || strings.Contains(err.Error(), maskedPath) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("masked no-file error = %v", err)
	}

	const quotedMask = `runtime"secret`
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, quotedMask, "masked")
	processor = newCommandProcessor(io.Discard, io.Discard)
	processor.addMask(quotedMask)
	if _, err := r.runUploadArtifact(t.Context(), processor, workspace, map[string]string{"path": quotedMask}); err == nil || strings.Contains(err.Error(), quotedMask) || strings.Contains(err.Error(), `runtime\"secret`) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("quoted masked path error = %v", err)
	}
}

func TestUploadArtifactRejectsSymlinksMasksAndDuplicateNamesBeforeUpload(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "real/file", "x")
	if err := os.Symlink("real", filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	uploader := &captureArtifactUploader{}
	r := newJobRun(Runner{Artifacts: uploader})
	processor := newCommandProcessor(io.Discard, io.Discard)
	if _, err := r.runUploadArtifact(t.Context(), processor, workspace, map[string]string{"path": "link/file"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
	processor.addMask("secret")
	if _, err := r.runUploadArtifact(t.Context(), processor, workspace, map[string]string{"path": "real/file", "name": "prefix-secret"}); err == nil || !strings.Contains(err.Error(), "registered mask") {
		t.Fatalf("masked-name error = %v", err)
	}
	processor = newCommandProcessor(io.Discard, io.Discard)
	if _, err := r.runUploadArtifact(t.Context(), processor, workspace, map[string]string{"path": "real/file", "name": "same"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.runUploadArtifact(t.Context(), processor, workspace, map[string]string{"path": "real/file", "name": "SAME"}); err == nil || len(uploader.uploads) != 1 {
		t.Fatalf("duplicate error = %v, uploads=%d", err, len(uploader.uploads))
	}
}

func TestUploadArtifactUploadFailureReleasesName(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "file", "x")
	uploader := &captureArtifactUploader{err: errors.New("upload failed")}
	r := newJobRun(Runner{Artifacts: uploader})
	if _, err := r.runUploadArtifact(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "file"}); err == nil {
		t.Fatal("upload failure was ignored")
	}
	if len(r.artifactRegistry.names) != 0 {
		t.Fatalf("failed upload retained registry reservation: %#v", r.artifactRegistry.names)
	}
}

func TestUploadArtifactCancellationStopsEveryArchiveStage(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "payload", strings.Repeat("x", 128*1024))
	files, err := collectUploadFiles(t.Context(), workspace, []string{"payload"}, false)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "payload.zip")
	digest, size, err := writeUploadZIP(t.Context(), archive, workspace, files, 0)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
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
	r := newJobRun(Runner{Artifacts: uploader})
	if _, err := r.runUploadArtifact(ctx, newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "payload"}); !errors.Is(err, context.Canceled) || len(uploader.uploads) != 0 {
		t.Fatalf("adapter cancellation = %v, uploads = %d", err, len(uploader.uploads))
	}

	readCtx, cancelRead := context.WithCancel(t.Context())
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
	files, err := collectUploadFiles(t.Context(), workspace, []string{"payload"}, false)
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(workspace, "payload")
	if err := os.Remove(payload); err != nil {
		t.Fatal(err)
	}
	if err := testMkfifo(payload, 0o600); errors.Is(err, errors.ErrUnsupported) {
		t.Skip("FIFOs unsupported")
	} else if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
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

func TestUploadArtifactRejectsSameSizeFileReplacement(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "payload", "before")
	files, err := collectUploadFiles(t.Context(), workspace, []string{"payload"}, false)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, workspace, "replacement", "after!")
	if err := os.Rename(filepath.Join(workspace, "replacement"), filepath.Join(workspace, "payload")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeUploadZIP(t.Context(), filepath.Join(t.TempDir(), "payload.zip"), workspace, files, 0); err == nil || !strings.Contains(err.Error(), "changed while archiving") {
		t.Fatalf("same-size replacement error = %v", err)
	}
}

func TestUploadArtifactRejectsSymlinkComponentReplacementToSelectedInode(t *testing.T) {
	for _, test := range []struct {
		name, path, link, target string
	}{
		{name: "file", path: "payload", link: "payload", target: "moved"},
		{name: "directory", path: "selected/payload", link: "selected", target: "moved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, test.path, "selected")
			files, err := collectUploadFiles(t.Context(), workspace, []string{test.path}, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(workspace, test.link), filepath.Join(workspace, test.target)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(test.target, filepath.Join(workspace, test.link)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := writeUploadZIP(t.Context(), filepath.Join(t.TempDir(), "payload.zip"), workspace, files, 0); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("symlink replacement error = %v", err)
			}
		})
	}
}

type cancellationArtifactUploader struct {
	started chan<- struct{}
}

func (cancellationArtifactUploader) DownloadArtifact(context.Context, string, string, string) error {
	return errors.New("unexpected artifact download")
}

func (u cancellationArtifactUploader) UploadArtifactFrom(ctx context.Context, _, _ string) error {
	close(u.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestUploadArtifactCancellationStopsAgentUpload(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "payload", "upload")
	started := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	r := newJobRun(Runner{Artifacts: cancellationArtifactUploader{started: started}})
	done := make(chan error, 1)
	go func() {
		_, err := r.runUploadArtifact(ctx, newCommandProcessor(io.Discard, io.Discard), workspace, map[string]string{"path": "payload"})
		done <- err
	}()
	select {
	case <-started:
		cancel()
	case err := <-done:
		t.Fatalf("upload returned before cancellation: %v", err)
	case <-ctx.Done():
		t.Fatal("agent upload did not start")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("agent upload cancellation = %v", err)
	}
	if len(r.artifactRegistry.names) != 0 {
		t.Fatalf("cancelled upload retained artifact reservation: %#v", r.artifactRegistry.names)
	}
}

func TestUploadArtifactSourceCollectionHasNoSizePolicy(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	large := filepath.Join(workspace, "large")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(6 << 30); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := collectUploadFiles(t.Context(), workspace, []string{"large"}, false)
	if err != nil || len(files) != 1 || files[0].size != 6<<30 {
		t.Fatalf("large source collection = %#v, %v", files, err)
	}
}

func TestUploadArtifactSourceFileCountBound(t *testing.T) {
	t.Parallel()

	many := t.TempDir()
	if err := os.Mkdir(filepath.Join(many, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= transport.MaxResultArtifactFileCount; i++ {
		name := filepath.Join(many, "files", fmt.Sprintf("%05d", i))
		file, err := os.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := collectUploadFiles(t.Context(), many, []string{"files"}, false); err == nil || !strings.Contains(err.Error(), "more than 10000") {
		t.Fatalf("file-count bound error = %v", err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestUploadArtifactAdapterBypassesVerifiedUpstreamLifecycle(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/upload.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: upload proof\n")
	writeFixtureFile(t, workspace, "payload/result.txt", "payload")
	remote := canonicalTempDir(t)
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
	job.Schema = plan.Schema
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
	result, err := (Runner{Actions: materializer, Artifacts: uploader}).runTestJob(t.Context(), job, workspace)
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
	maskedResult, err := (Runner{Actions: materializer, Artifacts: &captureArtifactUploader{}}).runTestJob(t.Context(), maskedJob, workspace)
	if err == nil || !strings.Contains(err.Error(), "artifact name contains a registered secret") || len(maskedResult.Artifacts) != 0 || maskedResult.Conclusion != "failure" {
		t.Fatalf("late artifact-name mask result = %#v, error = %v", maskedResult, err)
	}

	job.Actions[0].Commit = strings.Repeat("b", 40)
	fallbackResult, err := (Runner{Actions: materializer, Artifacts: &captureArtifactUploader{}}).runTestJob(t.Context(), job, workspace)
	if err != nil || fallbackResult.Conclusion != "success" || fallbackResult.Outputs["artifact_id"] == "" || fallbackResult.Outputs["artifact_digest"] == "" {
		t.Fatalf("unknown commit fallback result = %#v, error = %v", fallbackResult, err)
	}

	job.Actions[0].Commit = strings.Repeat("b", 39)
	if _, err := (Runner{Actions: materializer, Artifacts: &captureArtifactUploader{}}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "invalid GitHub identity") {
		t.Fatalf("malformed runtime commit error = %v", err)
	}
}

func TestUploadArtifactLegacyManifestsUseNativeAdapter(t *testing.T) {
	for _, test := range []struct {
		name, commit, manifest string
		inputs                 map[string]string
	}{
		{
			name: "v1 runner plugin", commit: actionintegration.UploadArtifactV1Commit,
			manifest: "name: upload artifact\ninputs:\n  name:\n    required: true\n  path:\n    required: true\nruns:\n  plugin: publish\n",
			inputs:   map[string]string{"name": "legacy-v1", "path": "payload"},
		},
		{
			name: "v2 node12", commit: actionintegration.UploadArtifactV2Commit,
			manifest: "name: upload artifact\ninputs:\n  path:\n    required: true\nruns:\n  using: node12\n  main: dist/index.js\n",
			inputs:   map[string]string{"path": "payload"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/upload.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: legacy upload proof\n")
			writeFixtureFile(t, workspace, "payload/result.txt", "payload")
			remote := canonicalTempDir(t)
			writeFixtureFile(t, remote, "action.yml", test.manifest)
			digest, err := source.DigestTree(remote)
			if err != nil {
				t.Fatal(err)
			}
			const lockID = "a-0000000000000001"
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
				ID: "upload", Kind: "uses", Uses: "actions/upload-artifact@" + test.commit,
				With: test.inputs, Action: &plan.ActionSelector{Lock: lockID},
			}})
			job.Schema = plan.Schema
			job.RequiredCapabilities = []string{"network"}
			job.Actions = []plan.ActionLock{{
				ID: lockID, Source: "github", Repository: "actions/upload-artifact", RequestedRef: test.commit,
				Commit: test.commit, SourceDigest: digest,
			}}
			materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: digest}}
			uploader := &captureArtifactUploader{}
			result, err := (Runner{Actions: materializer, Artifacts: uploader}).runTestJob(t.Context(), job, workspace)
			if err != nil || result.Conclusion != "success" || len(result.Artifacts) != 1 || len(uploader.uploads) != 1 || materializer.calls != 1 {
				t.Fatalf("legacy RunJob() result = %#v, uploads = %d, materializations = %d, error = %v", result, len(uploader.uploads), materializer.calls, err)
			}
		})
	}
}

func TestUploadArtifactV6ConditionalMatrixAndExpressionName(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/upload.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: conditional upload proof\n")
	writeFixtureFile(t, workspace, "artifacts.tar.gz", "archive")
	remote := canonicalTempDir(t)
	writeFixtureFile(t, remote, "action.yml", "name: upload artifact\nruns:\n  using: node24\n  main: dist/upload/index.js\n")
	writeFixtureFile(t, remote, "dist/upload/index.js", "throw new Error('adapter must not execute upstream JavaScript')\n")
	digest, err := source.DigestTree(remote)
	if err != nil {
		t.Fatal(err)
	}
	const lockID = "a-0000000000000006"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID: "upload", Kind: "uses", Uses: "actions/upload-artifact@" + actionintegration.UploadArtifactV6Commit,
		Condition: "matrix.mode == 'test'",
		With: map[string]string{
			"name": "${{ github.sha }}", "path": "./artifacts.tar.gz", "retention-days": "0",
		},
		Action: &plan.ActionSelector{Lock: lockID},
	}})
	job.Schema = plan.Schema
	job.RequiredCapabilities = []string{"network"}
	job.Event.SHA = strings.Repeat("a", 40)
	job.Actions = []plan.ActionLock{{
		ID: lockID, Source: "github", Repository: "actions/upload-artifact", RequestedRef: actionintegration.UploadArtifactV6Commit,
		Commit: actionintegration.UploadArtifactV6Commit, SourceDigest: digest,
	}}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: digest}}

	job.Matrix = map[string]any{"mode": "production"}
	uploader := &captureArtifactUploader{}
	result, err := (Runner{Actions: materializer, Artifacts: uploader}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || len(uploader.uploads) != 0 || len(result.Artifacts) != 0 {
		t.Fatalf("production matrix result = %#v, uploads = %d, error = %v", result, len(uploader.uploads), err)
	}

	job.Matrix = map[string]any{"mode": "test"}
	uploader = &captureArtifactUploader{}
	result, err = (Runner{Actions: materializer, Artifacts: uploader}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || len(uploader.uploads) != 1 || len(result.Artifacts) != 1 || result.Artifacts[0].Name != strings.Repeat("a", 40) {
		t.Fatalf("test matrix result = %#v, uploads = %d, error = %v", result, len(uploader.uploads), err)
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
