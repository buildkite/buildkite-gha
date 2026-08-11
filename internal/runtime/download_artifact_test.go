package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"golang.org/x/sys/unix"
)

type downloadStore struct {
	archive     string
	path        string
	jobID       string
	destination string
	extra       bool
	download    func(context.Context, string) error
}

type countingReaderAt struct {
	reader io.ReaderAt
	reads  []readAtCall
}

type readAtCall struct {
	offset int64
	size   int
}

func (r *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.reads = append(r.reads, readAtCall{offset: off, size: len(p)})
	return r.reader.ReadAt(p, off)
}

func (s *downloadStore) UploadArtifactFrom(context.Context, string, string) error { return nil }
func (s *downloadStore) DownloadArtifact(ctx context.Context, path, destination, jobID string) error {
	s.path = path
	s.jobID = jobID
	s.destination = destination
	if s.download != nil {
		return s.download(ctx, destination)
	}
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

func verifyDownloadDigest(ctx context.Context, filename, digest string, size int64) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	err = verifyDownloadDigestFile(ctx, f, digest, size)
	return errors.Join(err, f.Close())
}

func extractDownloadZIP(ctx context.Context, filename, workspace, destination string, expectedCount int) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	err = extractDownloadZIPFile(ctx, f, info.Size(), workspace, destination, expectedCount)
	return errors.Join(err, f.Close())
}

func TestDownloadArtifactExactNeedAndDirectExtraction(t *testing.T) {
	archive, size, digest := testDownloadZIP(t, "nested/result.txt")
	store := &downloadStore{archive: archive}
	workspace := t.TempDir()
	processor := newCommandProcessor(io.Discard, io.Discard)
	need := plan.NeedArtifact{Name: "payload", ID: "42", Path: "buildkite-gha/v1/artifacts/" + strings.Repeat("a", 64) + ".zip", Digest: digest, Size: size, FileCount: 1, Producer: plan.NeedProducer{JobID: "11111111-1111-4111-8111-111111111111"}}
	result, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{need}}}, actionintegration.DownloadArtifactCommit, map[string]string{"name": "payload", "path": "out"})
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
	if _, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{}, actionintegration.DownloadArtifactCommit, map[string]string{"name": "payload"}); err == nil || strings.Contains(err.Error(), "payload") {
		t.Fatal("missing artifact accepted")
	}
	if _, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{"a": {Artifacts: []plan.NeedArtifact{need}}, "b": {Artifacts: []plan.NeedArtifact{need}}}, actionintegration.DownloadArtifactCommit, map[string]string{"name": "payload"}); err == nil || strings.Contains(err.Error(), "payload") {
		t.Fatal("ambiguous artifact accepted")
	}
	if _, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{"matrix": {Artifacts: []plan.NeedArtifact{need, need}}}, actionintegration.DownloadArtifactCommit, map[string]string{"name": "payload"}); err == nil {
		t.Fatal("duplicate matrix artifact name accepted")
	}
	if _, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{need}}}, actionintegration.DownloadArtifactCommit, map[string]string{"name": "Payload"}); err == nil || strings.Contains(err.Error(), "Payload") {
		t.Fatal("non-exact artifact name accepted")
	}
}

func TestDownloadArtifactSupportsAuditedCommitsAndNonASCIIExactName(t *testing.T) {
	archive, size, digest := testDownloadZIP(t, "結果.txt")
	artifact := plan.NeedArtifact{
		Name: "成果物", ID: "42", Path: "buildkite-gha/v1/artifacts/" + strings.Repeat("f", 64) + ".zip",
		Digest: digest, Size: size, FileCount: 1,
		Producer: plan.NeedProducer{JobID: "11111111-1111-4111-8111-111111111111"},
	}
	for _, commit := range actionintegration.DownloadArtifactCommits() {
		t.Run(commit[:7], func(t *testing.T) {
			workspace := t.TempDir()
			inputs := map[string]string{"name": " 成果物 ", "path": ""}
			if commit == actionintegration.DownloadArtifactV8Commit || commit == actionintegration.DownloadArtifactV801Commit {
				inputs["skip-decompress"] = "False"
				inputs["digest-mismatch"] = "error"
			}
			result, err := (Runner{Artifacts: &downloadStore{archive: archive}}).runDownloadArtifact(context.Background(), newCommandProcessor(io.Discard, io.Discard), workspace, map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{artifact}}}, commit, inputs)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(workspace, "結果.txt")); err != nil || string(got) != "payload" {
				t.Fatalf("non-ASCII output = %q, %v", got, err)
			}
			want, _ := filepath.Abs(workspace)
			if result.Outputs["download-path"] != want {
				t.Fatalf("default download-path = %q, want %q", result.Outputs["download-path"], want)
			}
		})
	}
}

func TestNativeArtifactRoundTripTrimsNameAndAcceptsHighCompression(t *testing.T) {
	uploadWorkspace := t.TempDir()
	contents := make([]byte, 2<<20)
	if err := os.WriteFile(filepath.Join(uploadWorkspace, "zeros.bin"), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	uploader := &captureArtifactUploader{}
	uploadRunner := Runner{Artifacts: uploader, artifactRegistry: &artifactRegistry{names: map[string]bool{}}}
	upload, err := uploadRunner.runUploadArtifact(context.Background(), newCommandProcessor(io.Discard, io.Discard), uploadWorkspace, map[string]string{"name": " payload ", "path": "zeros.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(upload.Artifacts) != 1 || upload.Artifacts[0].Name != "payload" || len(uploader.uploads) != 1 {
		t.Fatalf("normalized upload = %#v, captures = %#v", upload, uploader.uploads)
	}
	archive := filepath.Join(t.TempDir(), "artifact.zip")
	if err := os.WriteFile(archive, uploader.uploads[0].data, 0o644); err != nil {
		t.Fatal(err)
	}
	record := upload.Artifacts[0]
	artifact := plan.NeedArtifact{
		Name: record.Name, ID: record.ID, Path: record.Path, Digest: record.Digest,
		Size: record.Size, FileCount: record.FileCount,
		Producer: plan.NeedProducer{JobID: "11111111-1111-4111-8111-111111111111"},
	}
	downloadWorkspace := t.TempDir()
	_, err = (Runner{Artifacts: &downloadStore{archive: archive}}).runDownloadArtifact(context.Background(), newCommandProcessor(io.Discard, io.Discard), downloadWorkspace, map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{artifact}}}, actionintegration.DownloadArtifactV801Commit, map[string]string{"name": " payload "})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(downloadWorkspace, "zeros.bin"))
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("high-compression roundtrip bytes = %d, error = %v", len(got), err)
	}
}

func TestDownloadArtifactRejectsMaskedNameWithoutDisclosure(t *testing.T) {
	const maskedName = "runtime-secret-artifact"
	processor := newCommandProcessor(io.Discard, io.Discard)
	processor.addMask(maskedName)
	_, err := (Runner{}).runDownloadArtifact(context.Background(), processor, t.TempDir(), nil, actionintegration.DownloadArtifactCommit, map[string]string{"name": maskedName})
	if err == nil || strings.Contains(err.Error(), maskedName) || !strings.Contains(err.Error(), "registered mask") {
		t.Fatalf("masked artifact lookup error = %v", err)
	}
}

func TestDownloadArtifactScrubsMaskedMemberFromDestinationErrors(t *testing.T) {
	const maskedMember = "runtime-secret-path"
	archive, size, digest := testDownloadZIP(t, maskedMember)
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, maskedMember), 0o755); err != nil {
		t.Fatal(err)
	}
	processor := newCommandProcessor(io.Discard, io.Discard)
	processor.addMask(maskedMember)
	artifact := plan.NeedArtifact{
		Name: "payload", Path: "buildkite-gha/v1/artifacts/" + strings.Repeat("d", 64) + ".zip",
		Digest: digest, Size: size, FileCount: 1,
		Producer: plan.NeedProducer{JobID: "11111111-1111-4111-8111-111111111111"},
	}
	_, err := (Runner{Artifacts: &downloadStore{archive: archive}}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{artifact}}}, actionintegration.DownloadArtifactCommit, map[string]string{"name": "payload"})
	if err == nil || strings.Contains(err.Error(), maskedMember) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("masked destination error = %v", err)
	}
}

func TestDownloadArtifactScrubsQuotedMaskedDestinationFromErrors(t *testing.T) {
	const maskedDestination = `runtime"secret`
	archive, size, digest := testDownloadZIP(t, "result.txt")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, maskedDestination), []byte("collision"), 0o644); err != nil {
		t.Fatal(err)
	}
	processor := newCommandProcessor(io.Discard, io.Discard)
	processor.addMask(maskedDestination)
	artifact := plan.NeedArtifact{
		Name: "payload", Path: "buildkite-gha/v1/artifacts/" + strings.Repeat("e", 64) + ".zip",
		Digest: digest, Size: size, FileCount: 1,
		Producer: plan.NeedProducer{JobID: "11111111-1111-4111-8111-111111111111"},
	}
	_, err := (Runner{Artifacts: &downloadStore{archive: archive}}).runDownloadArtifact(context.Background(), processor, workspace, map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{artifact}}}, actionintegration.DownloadArtifactCommit, map[string]string{"name": "payload", "path": maskedDestination})
	if err == nil || strings.Contains(err.Error(), maskedDestination) || strings.Contains(err.Error(), `runtime\"secret`) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("quoted masked destination error = %v", err)
	}
}

func TestDownloadArtifactRejectsUnsafeZIPAndDigest(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "dir/../escape", "a\\b", "C:/drive", `\\server\share`} {
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

func TestDownloadArtifactRejectsRawCorruptAndDuplicateArchivesWithoutDestinationMutation(t *testing.T) {
	workspace := t.TempDir()
	for _, test := range []struct {
		name      string
		archive   func(*testing.T) string
		fileCount int
	}{
		{name: "raw", archive: func(t *testing.T) string {
			name := filepath.Join(t.TempDir(), "raw")
			if err := os.WriteFile(name, []byte("not a ZIP"), 0o644); err != nil {
				t.Fatal(err)
			}
			return name
		}, fileCount: 1},
		{name: "duplicate", archive: func(t *testing.T) string {
			name, _, _ := testDownloadZIP(t, "same", "same")
			return name
		}, fileCount: 2},
		{name: "file before child", archive: func(t *testing.T) string {
			name, _, _ := testDownloadZIP(t, "same", "same/child")
			return name
		}, fileCount: 2},
		{name: "child before file", archive: func(t *testing.T) string {
			name, _, _ := testDownloadZIP(t, "same/child", "same")
			return name
		}, fileCount: 2},
		{name: "lexically separated file and child", archive: func(t *testing.T) string {
			name, _, _ := testDownloadZIP(t, "same", "same.", "same/child")
			return name
		}, fileCount: 3},
		{name: "corrupt member", archive: func(t *testing.T) string {
			name, _, _ := testDownloadZIP(t, "first", "second")
			zr, err := zip.OpenReader(name)
			if err != nil {
				t.Fatal(err)
			}
			offset, err := zr.File[0].DataOffset()
			if err != nil {
				t.Fatal(err)
			}
			if err := zr.Close(); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if offset < 0 || offset >= int64(len(contents)) {
				t.Fatal("ZIP member offset is out of bounds")
			}
			contents[offset] ^= 0xff
			if err := os.WriteFile(name, contents, 0o644); err != nil {
				t.Fatal(err)
			}
			return name
		}, fileCount: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := filepath.Join(workspace, strings.ReplaceAll(test.name, " ", "-"))
			if err := extractDownloadZIP(context.Background(), test.archive(t), workspace, filepath.Base(destination), test.fileCount); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("failed extraction mutated destination: %v", err)
			}
		})
	}
}

func TestDownloadArtifactPreflightsCentralDirectoryBeforeZIPAllocation(t *testing.T) {
	name := filepath.Join(t.TempDir(), "many.zip")
	out, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for i := 0; i <= transport.MaxResultArtifactFileCount; i++ {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: strings.Repeat("x", i%8+1) + string(rune(0x1000+i)), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(zw.Close(), out.Close()); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := preflightZIPDirectory(f, info.Size()); err == nil {
		t.Fatal("oversized central directory accepted")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	eocd := len(contents) - 22
	if eocd < 0 || binary.LittleEndian.Uint32(contents[eocd:]) != 0x06054b50 {
		t.Fatal("test ZIP has no terminal EOCD")
	}
	binary.LittleEndian.PutUint16(contents[eocd+8:], 1)
	binary.LittleEndian.PutUint16(contents[eocd+10:], 1)
	if err := os.WriteFile(name, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := preflightZIPDirectory(f, int64(len(contents))); err == nil {
		t.Fatal("forged central-directory count accepted")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	valid, _, _ := testDownloadZIP(t, "result.txt")
	contents, err = os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	eocd = len(contents) - 22
	binary.LittleEndian.PutUint16(contents[eocd+20:], 22)
	forgedEnd := make([]byte, 22)
	binary.LittleEndian.PutUint32(forgedEnd, 0x06054b50)
	contents = append(contents, forgedEnd...)
	if err := os.WriteFile(valid, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := preflightZIPDirectory(f, int64(len(contents))); err == nil {
		t.Fatal("ambiguous terminal EOCD accepted")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadArtifactPreflightRejectsPerEntryZIP64(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset int
		width  int
	}{
		{name: "compressed size", offset: 20, width: 4},
		{name: "uncompressed size", offset: 24, width: 4},
		{name: "disk start", offset: 34, width: 2},
		{name: "local header offset", offset: 42, width: 4},
	} {
		t.Run(test.name+" sentinel", func(t *testing.T) {
			name, _, _ := testDownloadZIP(t, "result.txt")
			contents, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			eocd := len(contents) - 22
			central := int(binary.LittleEndian.Uint32(contents[eocd+16:]))
			if central < 0 || central+46 > eocd || binary.LittleEndian.Uint32(contents[central:]) != 0x02014b50 {
				t.Fatal("test ZIP central directory is malformed")
			}
			if test.width == 2 {
				binary.LittleEndian.PutUint16(contents[central+test.offset:], 1<<16-1)
			} else {
				binary.LittleEndian.PutUint32(contents[central+test.offset:], 1<<32-1)
			}
			if err := os.WriteFile(name, contents, 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := preflightZIPDirectory(f, int64(len(contents))); err == nil || !strings.Contains(err.Error(), "ZIP64") {
				t.Fatalf("per-entry ZIP64 sentinel error = %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("nonzero disk start", func(t *testing.T) {
		name, _, _ := testDownloadZIP(t, "result.txt")
		contents, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		eocd := len(contents) - 22
		central := int(binary.LittleEndian.Uint32(contents[eocd+16:]))
		binary.LittleEndian.PutUint16(contents[central+34:], 1)
		if err := preflightZIPDirectory(bytes.NewReader(contents), int64(len(contents))); err == nil {
			t.Fatal("nonzero per-entry disk accepted")
		}
	})

	for _, test := range []struct {
		name  string
		extra []byte
		want  string
	}{
		{name: "chained ZIP64 extra", extra: []byte{0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00}, want: "ZIP64"},
		{name: "truncated extra header", extra: []byte{0x02, 0x00, 0x00}, want: "malformed"},
		{name: "extra size overrun", extra: []byte{0x02, 0x00, 0x04, 0x00, 0x00}, want: "malformed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "zip64-extra.zip")
			out, err := os.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			zw := zip.NewWriter(out)
			header := &zip.FileHeader{Name: "result.txt", Method: zip.Store, Extra: test.extra}
			member, err := zw.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := member.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(zw.Close(), out.Close()); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := preflightZIPDirectory(f, info.Size()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("per-entry extra error = %v, want %q", err, test.want)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDownloadArtifactPreflightRejectsLocalZIP64(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset int
	}{
		{name: "compressed size", offset: 18},
		{name: "uncompressed size", offset: 22},
	} {
		t.Run(test.name+" sentinel", func(t *testing.T) {
			name, _, _ := testDownloadZIP(t, "result.txt")
			contents, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			eocd := len(contents) - 22
			central := int(binary.LittleEndian.Uint32(contents[eocd+16:]))
			local := int(binary.LittleEndian.Uint32(contents[central+42:]))
			binary.LittleEndian.PutUint32(contents[local+test.offset:], 1<<32-1)
			if err := preflightZIPDirectory(bytes.NewReader(contents), int64(len(contents))); err == nil || !strings.Contains(err.Error(), "ZIP64") {
				t.Fatalf("local ZIP64 sentinel error = %v", err)
			}
		})
	}

	t.Run("extra field", func(t *testing.T) {
		name := filepath.Join(t.TempDir(), "local-zip64-extra.zip")
		out, err := os.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		zw := zip.NewWriter(out)
		member, err := zw.CreateHeader(&zip.FileHeader{Name: "result.txt", Method: zip.Store, Extra: []byte{0x01, 0x00, 0x00, 0x00}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write([]byte("payload")); err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(zw.Close(), out.Close()); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		eocd := len(contents) - 22
		central := int(binary.LittleEndian.Uint32(contents[eocd+16:]))
		centralExtra := central + 46 + int(binary.LittleEndian.Uint16(contents[central+28:]))
		binary.LittleEndian.PutUint16(contents[centralExtra:], 2)
		if err := preflightZIPDirectory(bytes.NewReader(contents), int64(len(contents))); err == nil || !strings.Contains(err.Error(), "ZIP64") {
			t.Fatalf("local ZIP64 extra error = %v", err)
		}
	})
}

func TestDownloadArtifactPreflightBoundsCentralMetadata(t *testing.T) {
	for _, test := range []struct {
		name    string
		header  zip.FileHeader
		wantErr string
	}{
		{name: "oversized member name", header: zip.FileHeader{Name: strings.Repeat("x", actionintegration.MaxUploadArtifactPathBytes+1), Method: zip.Store}, wantErr: "metadata"},
		{name: "member comment", header: zip.FileHeader{Name: "result.txt", Method: zip.Store, Comment: "comment"}, wantErr: "metadata"},
		{name: "oversized extra", header: zip.FileHeader{Name: "result.txt", Method: zip.Store, Extra: append([]byte{0x02, 0x00, 61, 0x00}, make([]byte, 61)...)}, wantErr: "metadata"},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "metadata.zip")
			out, err := os.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			zw := zip.NewWriter(out)
			member, err := zw.CreateHeader(&test.header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := member.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(zw.Close(), out.Close()); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := preflightZIPDirectory(bytes.NewReader(contents), int64(len(contents))); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("central metadata error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestDownloadArtifactPreflightRejectsEOCDZIP64Forms(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset int
		width  int
		value  uint32
	}{
		{name: "entry count sentinel", offset: 10, width: 2, value: 1<<16 - 1},
		{name: "directory size with ZIP64 locator", offset: 12, width: 4, value: 1<<16 - 1},
		{name: "directory size ZIP64 sentinel", offset: 12, width: 4, value: 1<<32 - 1},
		{name: "directory offset sentinel", offset: 16, width: 4, value: 1<<32 - 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			name, _, _ := testDownloadZIP(t, "result.txt")
			contents, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			eocd := len(contents) - 22
			if test.width == 2 {
				binary.LittleEndian.PutUint16(contents[eocd+test.offset:], uint16(test.value))
			} else {
				binary.LittleEndian.PutUint32(contents[eocd+test.offset:], test.value)
			}
			if test.name == "directory size with ZIP64 locator" {
				locator := contents[eocd-20 : eocd]
				clear(locator)
				binary.LittleEndian.PutUint32(locator, 0x07064b50)
				binary.LittleEndian.PutUint32(locator[16:], 1)
			}
			if err := preflightZIPDirectory(bytes.NewReader(contents), int64(len(contents))); err == nil || !strings.Contains(err.Error(), "ZIP64") {
				t.Fatalf("EOCD ZIP64 error = %v", err)
			}
		})
	}
}

func TestDownloadArtifactPreflightAcceptsOrdinary65535ByteCentralDirectory(t *testing.T) {
	name := filepath.Join(t.TempDir(), "ordinary-65535.zip")
	out, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for i := range 100 {
		length := 600
		if i == 99 {
			length = 635
		}
		prefix := fmt.Sprintf("%03d-", i)
		header := &zip.FileHeader{
			Name: prefix + strings.Repeat("x", length-len(prefix)), Method: zip.Store,
			Modified: time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC),
		}
		member, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write([]byte("payload")); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(zw.Close(), out.Close()); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	eocd := len(contents) - 22
	if got := binary.LittleEndian.Uint32(contents[eocd+12:]); got != 1<<16-1 {
		t.Fatalf("central directory size = %d, want 65535", got)
	}
	if err := preflightZIPDirectory(bytes.NewReader(contents), int64(len(contents))); err != nil {
		t.Fatalf("ordinary 65,535-byte central directory rejected: %v", err)
	}
}

func TestDownloadArtifactPreflightRejectsShortCentralDirectoryResidue(t *testing.T) {
	name, _, _ := testDownloadZIP(t, "result.txt")
	contents, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	eocd := len(contents) - 22
	binary.LittleEndian.PutUint32(contents[eocd+12:], 1)
	binary.LittleEndian.PutUint32(contents[eocd+16:], uint32(eocd-1))
	reader := &countingReaderAt{reader: bytes.NewReader(contents)}
	if err := preflightZIPDirectory(reader, int64(len(contents))); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("short central directory error = %v", err)
	}
	if len(reader.reads) != 1 {
		t.Fatalf("ReaderAt calls = %#v, want only the EOCD tail read", reader.reads)
	}
}

func TestDownloadArtifactPreflightUsesBoundedMetadataReads(t *testing.T) {
	name := filepath.Join(t.TempDir(), "many-extras.zip")
	out, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	extra := bytes.Repeat([]byte{0x02, 0x00, 0x00, 0x00}, 10)
	member, err := zw.CreateHeader(&zip.FileHeader{Name: "result.txt", Method: zip.Store, Extra: extra})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := member.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(zw.Close(), out.Close()); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	reader := &countingReaderAt{reader: bytes.NewReader(contents)}
	if err := preflightZIPDirectory(reader, int64(len(contents))); err != nil {
		t.Fatal(err)
	}
	if len(reader.reads) != 5 {
		t.Fatalf("ReaderAt calls = %#v, want tail plus bounded central and local header/metadata reads", reader.reads)
	}
	eocd := len(contents) - 22
	central := int64(binary.LittleEndian.Uint32(contents[eocd+16:]))
	for _, call := range reader.reads[1:3] {
		if call.offset < central || call.offset+int64(call.size) > int64(eocd) {
			t.Fatalf("central-directory read %#v is outside [%d, %d)", call, central, eocd)
		}
	}
	for _, call := range reader.reads[3:] {
		if call.offset < 0 || call.offset+int64(call.size) > central {
			t.Fatalf("local-header read %#v is outside [0, %d)", call, central)
		}
	}
}

func TestDownloadArtifactExtractionUsesVerifiedArchiveDescriptor(t *testing.T) {
	name, size, digest := testDownloadZIP(t, "result.txt")
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := verifyDownloadDigestFile(context.Background(), f, digest, size); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(name, name+".verified"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := extractDownloadZIPFile(context.Background(), f, size, workspace, ".", 1); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "result.txt")); err != nil || string(got) != "payload" {
		t.Fatalf("descriptor-pinned extraction = %q, %v", got, err)
	}
}

func TestDownloadArtifactInstallUsesPinnedDestinationDescriptor(t *testing.T) {
	workspace := t.TempDir()
	destination := filepath.Join(workspace, "destination")
	staging := filepath.Join(workspace, "staging")
	alternate := filepath.Join(workspace, "alternate")
	for _, directory := range []string{destination, staging, alternate} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(staging, "result.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	destinationFD, err := unix.Open(destination, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(destinationFD) }()
	stagingFD, err := unix.Open(staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(stagingFD) }()
	moved := destination + "-moved"
	if err := os.Rename(destination, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(alternate, destination); err != nil {
		t.Fatal(err)
	}
	if err := installDownloadMembersAt(context.Background(), stagingFD, destinationFD, []downloadMember{{name: "result.txt", size: 7}}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(moved, "result.txt")); err != nil || string(got) != "payload" {
		t.Fatalf("pinned destination contents = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(alternate, "result.txt")); !os.IsNotExist(err) {
		t.Fatalf("replacement destination target was mutated: %v", err)
	}
}

func TestDownloadArtifactCancellationCleansPartialAgentDownload(t *testing.T) {
	archive, size, digest := testDownloadZIP(t, "result.txt")
	artifact := plan.NeedArtifact{
		Name: "payload", Path: "buildkite-gha/v1/artifacts/" + strings.Repeat("9", 64) + ".zip",
		Digest: digest, Size: size, FileCount: 1,
		Producer: plan.NeedProducer{JobID: "11111111-1111-4111-8111-111111111111"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	store := &downloadStore{archive: archive, download: func(ctx context.Context, destination string) error {
		if err := os.WriteFile(filepath.Join(destination, "partial"), []byte("partial"), 0o644); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	}}
	_, err := (Runner{Artifacts: store}).runDownloadArtifact(ctx, newCommandProcessor(io.Discard, io.Discard), t.TempDir(), map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{artifact}}}, actionintegration.DownloadArtifactCommit, map[string]string{"name": "payload"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, err := os.Lstat(store.destination); !os.IsNotExist(err) {
		t.Fatalf("partial agent download was not cleaned: %v", err)
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
			_, err := (Runner{Artifacts: store}).runDownloadArtifact(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), map[string]plan.Need{"producer": {Artifacts: []plan.NeedArtifact{artifact}}}, actionintegration.DownloadArtifactCommit, map[string]string{"name": "payload"})
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
	workspace := canonicalTempDir(t)
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

func TestDownloadArtifactMatrixConsumersEvaluateNameAndNormalizeRootPath(t *testing.T) {
	archive, size, digest := testDownloadZIP(t, "result.txt")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action.yml", "name: download artifact\nruns:\n  using: node24\n  main: dist/main.js\n")
	writeFixtureFile(t, remote, "dist/main.js", "throw new Error('adapter must not execute upstream JavaScript')\n")
	sourceDigest, err := source.DigestTree(remote)
	if err != nil {
		t.Fatal(err)
	}
	const eventSHA = "7d9b24bba24eb46a23a207882718feb61138fada"
	artifact := plan.NeedArtifact{
		Name: eventSHA, ID: "42", Path: "buildkite-gha/v1/artifacts/" + strings.Repeat("c", 64) + ".zip",
		Digest: digest, Size: size, FileCount: 1,
		Producer: plan.NeedProducer{BuildID: "11111111-1111-4111-8111-111111111111", JobID: "22222222-2222-4222-8222-222222222222", StepKey: "gha-producer-one"},
	}
	store := &downloadStore{archive: archive}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: sourceDigest}}
	for _, shard := range []string{"one", "two"} {
		t.Run(shard, func(t *testing.T) {
			workspace := canonicalTempDir(t)
			workflowPath := ".github/workflows/download.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: matrix download proof\n")
			lockID := "a-0000000000000002"
			job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
				ID: "download", Kind: "uses", Uses: "actions/download-artifact@" + actionintegration.DownloadArtifactV7Commit,
				With: map[string]string{"name": "${{ github.sha }}", "path": "./"}, Action: &plan.ActionSelector{Lock: lockID},
			}})
			job.Schema = plan.SchemaV3
			job.RequiredCapabilities = []string{"network"}
			job.Event.SHA = eventSHA
			job.Matrix = map[string]any{"shard": shard}
			job.Outputs = map[string]string{"download_path": "${{ steps.download.outputs.download-path }}"}
			job.Actions = []plan.ActionLock{{
				ID: lockID, Source: "github", Repository: "actions/download-artifact", RequestedRef: actionintegration.DownloadArtifactV7Commit,
				Commit: actionintegration.DownloadArtifactV7Commit, SourceDigest: sourceDigest,
			}}
			job.Needs = map[string]plan.Need{"producer": {Result: "success", Artifacts: []plan.NeedArtifact{artifact}}}

			result, err := (Runner{Actions: materializer, Artifacts: store}).RunJob(context.Background(), job, workspace)
			wantPath, absErr := filepath.Abs(workspace)
			if absErr != nil {
				t.Fatal(absErr)
			}
			if err != nil || result.Conclusion != "success" || result.Outputs["download_path"] != wantPath || !filepath.IsAbs(result.Outputs["download_path"]) {
				t.Fatalf("RunJob() result = %#v, error = %v, want download path %q", result, err, wantPath)
			}
			if got, err := os.ReadFile(filepath.Join(workspace, "result.txt")); err != nil || string(got) != "payload" {
				t.Fatalf("workspace-root contents = %q, %v", got, err)
			}
		})
	}
	if materializer.calls != 2 || store.path != artifact.Path || store.jobID != artifact.Producer.JobID {
		t.Fatalf("materializations/download = %d / %q / %q", materializer.calls, store.path, store.jobID)
	}
}
