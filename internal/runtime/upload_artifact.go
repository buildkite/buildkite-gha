package runtime

import (
	"archive/zip"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// ArtifactUploader is the narrow native storage boundary used by the adapter.
type ArtifactUploader interface {
	UploadArtifactFrom(context.Context, string, string) error
}

type artifactRegistry struct {
	mu    sync.Mutex
	names map[string]bool
}

func (r *artifactRegistry) reserve(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.ToLower(name)
	if r.names[key] {
		return fmt.Errorf("artifact name is already reserved in this job")
	}
	if len(r.names) == transport.MaxResultArtifacts {
		return fmt.Errorf("job already has the maximum of %d artifacts", transport.MaxResultArtifacts)
	}
	r.names[key] = true
	return nil
}

func (r *artifactRegistry) release(name string) {
	r.mu.Lock()
	delete(r.names, strings.ToLower(name))
	r.mu.Unlock()
}

type uploadOptions struct {
	name, noFiles, searchPath string
	paths                     []string
	hidden                    bool
	level                     int
}

func parseUploadOptions(inputs map[string]string) (uploadOptions, error) {
	if err := actionintegration.ValidateUploadArtifactInputs(inputs); err != nil {
		return uploadOptions{}, err
	}
	values := map[string]string{}
	for k, v := range inputs {
		values[strings.ToLower(k)] = v
	}
	o := uploadOptions{name: "artifact", noFiles: "warn", level: 6}
	if v, ok := values["name"]; ok {
		o.name = v
	}
	if err := actionintegration.ValidateUploadArtifactName(o.name); err != nil {
		return o, err
	}
	if v, ok := values["if-no-files-found"]; ok {
		o.noFiles = v
	}
	if o.noFiles != "warn" && o.noFiles != "error" && o.noFiles != "ignore" {
		return o, fmt.Errorf("if-no-files-found must be warn, error, or ignore")
	}
	var err error
	if v, ok := values["include-hidden-files"]; ok {
		o.hidden, err = strconv.ParseBool(v)
		if err != nil {
			return o, fmt.Errorf("include-hidden-files must be a toolkit boolean")
		}
	}
	if v, ok := values["compression-level"]; ok {
		o.level, err = strconv.Atoi(v)
		if err != nil || o.level < 0 || o.level > 9 {
			return o, fmt.Errorf("compression-level must be an integer from 0 to 9")
		}
	}
	if v, ok := values["overwrite"]; ok {
		b, e := strconv.ParseBool(v)
		if e != nil || b {
			return o, fmt.Errorf("overwrite may only be omitted or false; Phase 6 is required")
		}
	}
	if v, ok := values["archive"]; ok {
		b, e := strconv.ParseBool(v)
		if e != nil || !b {
			return o, fmt.Errorf("archive may only be omitted or true; Phase 6 is required")
		}
	}
	o.searchPath = strings.TrimSpace(values["path"])
	o.paths, err = actionintegration.UploadArtifactPaths(o.searchPath)
	if err != nil {
		return o, err
	}
	return o, nil
}

type archiveFile struct {
	disk, name string
	size       int64
}

func (r Runner) runUploadArtifact(ctx context.Context, processor *commandProcessor, workspace string, inputs map[string]string) (Result, error) {
	result := newResult()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	o, err := parseUploadOptions(inputs)
	if err != nil {
		return result, fmt.Errorf("bounded upload-artifact adapter: %w", err)
	}
	for _, mask := range processor.maskValues() {
		if mask != "" && strings.Contains(o.name, mask) {
			return result, fmt.Errorf("artifact name contains a registered mask and cannot be uploaded")
		}
	}
	if err := r.artifactRegistry.reserve(o.name); err != nil {
		return result, err
	}
	success := false
	defer func() {
		if !success {
			r.artifactRegistry.release(o.name)
		}
	}()
	files, err := collectUploadFiles(ctx, workspace, o.paths, o.hidden)
	if err != nil {
		return result, err
	}
	if len(files) == 0 {
		message := fmt.Sprintf("No files were found with the provided path: %s. No artifacts will be uploaded.", o.searchPath)
		switch o.noFiles {
		case "error":
			_ = processor.process(processor.stdout, "::error::"+escapeWorkflowCommandData(message))
			return result, errors.New(message)
		case "warn":
			_ = processor.process(processor.stdout, "::warning::"+escapeWorkflowCommandData(message))
		case "ignore":
			_ = processor.process(processor.stdout, message)
		}
		return result, nil
	}
	if r.Artifacts == nil {
		return result, fmt.Errorf("native artifact uploader is not configured")
	}
	root, err := os.MkdirTemp("", "buildkite-gha-upload-")
	if err != nil {
		return result, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	temporary, err := os.CreateTemp(root, "upload-artifact-*.zip")
	if err != nil {
		return result, err
	}
	tmp := temporary.Name()
	if err := temporary.Close(); err != nil {
		return result, err
	}
	defer func() { _ = os.Remove(tmp) }()
	digest, size, err := writeUploadZIP(ctx, tmp, workspace, files, o.level)
	if err != nil {
		return result, err
	}
	storageIdentity := make([]byte, 0, len(o.name)+1+len(digest))
	storageIdentity = append(storageIdentity, o.name...)
	storageIdentity = append(storageIdentity, 0)
	storageIdentity = append(storageIdentity, digest...)
	storageSum := sha256.Sum256(storageIdentity)
	storage := hex.EncodeToString(storageSum[:])
	rel := "buildkite-gha/v1/artifacts/" + storage + ".zip"
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return result, err
	}
	if err := os.Rename(tmp, abs); err != nil {
		return result, err
	}
	if err := verifyUploadZIP(ctx, abs, digest, size); err != nil {
		return result, err
	}
	if err := r.Artifacts.UploadArtifactFrom(ctx, root, rel); err != nil {
		return result, fmt.Errorf("upload native artifact: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	idNumber := binary.BigEndian.Uint64(storageSum[:8])
	if idNumber == 0 {
		idNumber = 1
	}
	// The future download adapter resolves this opaque ID through the result manifest.
	id := strconv.FormatUint(idNumber, 10)
	result.Outputs["artifact-id"] = id
	result.Outputs["artifact-digest"] = digest
	result.Artifacts = []transport.ResultArtifact{{Name: o.name, ID: id, Path: rel, Digest: "sha256:" + digest, Size: size, FileCount: len(files)}}
	success = true
	return result, nil
}

func verifyUploadZIP(ctx context.Context, path, digest string, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, io.LimitReader(contextReader{ctx: ctx, reader: file}, transport.MaxResultArtifactSizeBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("native artifact ZIP changed before upload")
	}
	return nil
}

func collectUploadFiles(ctx context.Context, workspace string, roots []string, hidden bool) ([]archiveFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve upload workspace: %w", err)
	}
	var files []archiveFile
	var matchedRoots []string
	var bytes int64
	add := func(disk string, info os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		bytes += info.Size()
		if bytes > transport.MaxResultArtifactSizeBytes {
			return fmt.Errorf("artifact source bytes exceed 1 GiB")
		}
		files = append(files, archiveFile{disk: disk, size: info.Size()})
		if len(files) > transport.MaxResultArtifactFileCount {
			return fmt.Errorf("artifact has more than 10000 selected files")
		}
		return nil
	}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if hiddenPath(root) && !hidden {
			continue
		}
		before := len(files)
		if err := rejectUploadSymlinkComponents(ctx, workspace, root); err != nil {
			return nil, err
		}
		disk := filepath.Join(workspace, root)
		info, err := os.Lstat(disk)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("visible symlink %q is unsupported", root)
		}
		if info.Mode().IsRegular() {
			if err := add(disk, info); err != nil {
				return nil, err
			}
			matchedRoots = append(matchedRoots, root)
			continue
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("non-regular path %q is unsupported", root)
		}
		err = filepath.Walk(disk, func(p string, i os.FileInfo, e error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e != nil {
				return e
			}
			rel, _ := filepath.Rel(workspace, p)
			if p != disk && hiddenPath(rel) && !hidden {
				if i.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if i.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("visible symlink %q is unsupported", rel)
			}
			if i.IsDir() {
				return nil
			}
			if !i.Mode().IsRegular() {
				return fmt.Errorf("non-regular path %q is unsupported", rel)
			}
			return add(p, i)
		})
		if err != nil {
			return nil, err
		}
		if len(files) > before {
			matchedRoots = append(matchedRoots, root)
		}
	}
	if len(files) == 0 {
		return files, nil
	}
	base := commonArchiveRoot(workspace, matchedRoots)
	seen := make(map[string]string, len(files))
	for i := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(filepath.Join(workspace, base), files[i].disk)
		if err != nil {
			return nil, err
		}
		name := filepath.ToSlash(relative)
		if name == "." {
			name = filepath.Base(files[i].disk)
		}
		lower := strings.ToLower(name)
		if old, ok := seen[lower]; ok {
			return nil, fmt.Errorf("duplicate or case-colliding archive paths %q and %q", old, name)
		}
		if len(name) > actionintegration.MaxUploadArtifactPathBytes || !utf8.ValidString(name) || strings.ContainsAny(name, `":<>|*?`+"\r\n\\") {
			return nil, fmt.Errorf("archive path %q contains forbidden characters", name)
		}
		seen[lower] = name
		files[i].name = name
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

func rejectUploadSymlinkComponents(ctx context.Context, workspace, relative string) error {
	current := workspace
	for _, component := range strings.Split(filepath.FromSlash(relative), string(filepath.Separator)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("visible symlink %q is unsupported", relative)
		}
	}
	return nil
}

func hiddenPath(p string) bool {
	for _, s := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.HasPrefix(s, ".") && s != "." && s != ".." {
			return true
		}
	}
	return false
}
func commonArchiveRoot(workspace string, roots []string) string {
	if len(roots) == 1 {
		p := filepath.Clean(roots[0])
		if i, e := os.Stat(filepath.Join(workspace, p)); e == nil && i.IsDir() {
			return p
		}
		return filepath.Dir(p)
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(roots[0])), "/")
	for _, r := range roots[1:] {
		q := strings.Split(filepath.ToSlash(filepath.Clean(r)), "/")
		n := 0
		for n < len(parts) && n < len(q) && parts[n] == q[n] {
			n++
		}
		parts = parts[:n]
	}
	if len(parts) == 0 {
		return "."
	}
	return filepath.FromSlash(strings.Join(parts, "/"))
}

func writeUploadZIP(ctx context.Context, path, workspace string, files []archiveFile, level int) (_ string, _ int64, err error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if e != nil {
		return "", 0, e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	workspace, e = filepath.EvalSymlinks(workspace)
	if e != nil {
		return "", 0, fmt.Errorf("resolve upload workspace: %w", e)
	}
	root, e := os.OpenRoot(workspace)
	if e != nil {
		return "", 0, fmt.Errorf("open upload workspace: %w", e)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	h := sha256.New()
	w := io.MultiWriter(f, h)
	z := zip.NewWriter(w)
	if level > 0 {
		z.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) { return flate.NewWriter(out, level) })
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		method := uint16(zip.Deflate)
		if level == 0 {
			method = zip.Store
		}
		hdr := &zip.FileHeader{Name: file.name, Method: method, Modified: time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)}
		hdr.SetMode(0o644)
		zw, e := z.CreateHeader(hdr)
		if e != nil {
			return "", 0, e
		}
		relative, e := filepath.Rel(workspace, file.disk)
		if e != nil {
			return "", 0, e
		}
		in, e := root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if e != nil {
			return "", 0, e
		}
		if e = ctx.Err(); e != nil {
			return "", 0, errors.Join(e, in.Close())
		}
		info, statErr := in.Stat()
		if statErr != nil {
			return "", 0, errors.Join(statErr, in.Close())
		}
		if !info.Mode().IsRegular() {
			return "", 0, errors.Join(fmt.Errorf("artifact source %q changed to a non-regular file", file.name), in.Close())
		}
		if info.Size() != file.size {
			return "", 0, errors.Join(fmt.Errorf("artifact source %q changed while archiving", file.name), in.Close())
		}
		copied, copyErr := io.CopyN(zw, contextReader{ctx: ctx, reader: in}, file.size+1)
		closeErr := in.Close()
		if copyErr != nil && !errors.Is(copyErr, io.EOF) {
			return "", 0, errors.Join(copyErr, closeErr)
		}
		if copyErr != io.EOF || copied != file.size {
			return "", 0, fmt.Errorf("artifact source %q changed while archiving", file.name)
		}
		if closeErr != nil {
			return "", 0, closeErr
		}
	}
	if e = z.Close(); e != nil {
		return "", 0, e
	}
	if e = ctx.Err(); e != nil {
		return "", 0, e
	}
	info, e := f.Stat()
	if e != nil {
		return "", 0, e
	}
	if info.Size() > transport.MaxResultArtifactSizeBytes {
		return "", 0, fmt.Errorf("final ZIP exceeds 1 GiB")
	}
	return hex.EncodeToString(h.Sum(nil)), info.Size(), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if contextErr := r.ctx.Err(); contextErr != nil {
		err = contextErr
	}
	return n, err
}

func escapeWorkflowCommandData(value string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(value)
}
