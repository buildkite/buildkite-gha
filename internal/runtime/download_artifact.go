package runtime

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

type downloadMember struct {
	file *zip.File
	name string
	size int64
}

func (r Runner) runDownloadArtifact(ctx context.Context, processor *commandProcessor, workspace string, needs map[string]plan.Need, inputs map[string]string) (Result, error) {
	result := newResult()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := actionintegration.ValidateDownloadArtifactInputs(inputs); err != nil {
		return result, fmt.Errorf("bounded download-artifact adapter: %w", err)
	}
	values := map[string]string{}
	for k, v := range inputs {
		values[strings.ToLower(k)] = v
	}
	name, destinationRelative := values["name"], "."
	for _, mask := range processor.maskValues() {
		if mask != "" && strings.Contains(name, mask) {
			return result, errors.New("artifact name contains a registered mask and cannot be downloaded")
		}
	}
	if p := values["path"]; p != "" {
		destinationRelative = filepath.FromSlash(p)
	}
	logicalWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return result, err
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(logicalWorkspace)
	if err != nil {
		return result, fmt.Errorf("resolve download workspace: %w", err)
	}
	absDestination := filepath.Join(logicalWorkspace, destinationRelative)
	var matches []plan.NeedArtifact
	for _, need := range needs {
		for _, artifact := range need.Artifacts {
			if artifact.Name == name {
				matches = append(matches, artifact)
			}
		}
	}
	if len(matches) != 1 {
		return result, fmt.Errorf("artifact lookup found %d verified matches across direct needs, want exactly one", len(matches))
	}
	if r.Artifacts == nil {
		return result, fmt.Errorf("native artifact store is not configured")
	}
	artifact := matches[0]
	temporary, err := os.MkdirTemp("", "buildkite-gha-download-")
	if err != nil {
		return result, err
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	if err := r.Artifacts.DownloadArtifact(ctx, artifact.Path, temporary, artifact.Producer.JobID); err != nil {
		return result, fmt.Errorf("download native artifact: %w", err)
	}
	archivePath := filepath.Join(temporary, filepath.FromSlash(artifact.Path))
	regular := 0
	if err := filepath.WalkDir(temporary, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || filename != archivePath {
			return fmt.Errorf("download contained an unexpected file %q", filename)
		}
		regular++
		return nil
	}); err != nil {
		return result, fmt.Errorf("download must contain exactly the expected archive: %w", err)
	}
	if regular != 1 {
		return result, fmt.Errorf("download contained %d regular files, want exactly the expected archive", regular)
	}
	info, err := os.Lstat(archivePath)
	if err != nil || !info.Mode().IsRegular() {
		return result, fmt.Errorf("downloaded archive is not the expected regular file")
	}
	if info.Size() != artifact.Size {
		return result, fmt.Errorf("downloaded archive size mismatch")
	}
	if err := verifyDownloadDigest(ctx, archivePath, artifact.Digest, artifact.Size); err != nil {
		return result, err
	}
	if err := extractDownloadZIP(ctx, archivePath, resolvedWorkspace, destinationRelative, artifact.FileCount); err != nil {
		return result, err
	}
	result.Outputs["download-path"] = absDestination
	return result, nil
}

func verifyDownloadDigest(ctx context.Context, filename, digest string, size int64) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(h, io.LimitReader(contextReader{ctx: ctx, reader: f}, transport.MaxResultArtifactSizeBytes+1))
	closeErr := f.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if n != size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		return fmt.Errorf("downloaded archive digest mismatch")
	}
	return ctx.Err()
}

func extractDownloadZIP(ctx context.Context, filename, workspace, destination string, expectedCount int) error {
	z, err := zip.OpenReader(filename)
	if err != nil {
		return fmt.Errorf("open artifact ZIP: %w", err)
	}
	defer func() { _ = z.Close() }()
	if len(z.File) != expectedCount || len(z.File) > transport.MaxResultArtifactFileCount {
		return fmt.Errorf("artifact ZIP file count mismatch")
	}
	seen, folded := map[string]bool{}, map[string]bool{}
	members := make([]downloadMember, 0, len(z.File))
	var expanded int64
	for _, f := range z.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := f.Name
		if !validArtifactMember(name) || seen[name] || folded[strings.ToLower(name)] {
			return fmt.Errorf("unsafe or duplicate artifact ZIP member %q", name)
		}
		if f.Method != zip.Store && f.Method != zip.Deflate || !f.Mode().IsRegular() || f.FileInfo().IsDir() {
			return fmt.Errorf("artifact ZIP member %q is not a supported regular file", name)
		}
		if f.UncompressedSize64 > uint64(transport.MaxResultArtifactSizeBytes) || expanded > transport.MaxResultArtifactSizeBytes-int64(f.UncompressedSize64) {
			return fmt.Errorf("artifact expanded bytes exceed 1 GiB")
		}
		expanded += int64(f.UncompressedSize64)
		seen[name], folded[strings.ToLower(name)] = true, true
		members = append(members, downloadMember{file: f, name: name, size: int64(f.UncompressedSize64)})
	}
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		return fmt.Errorf("open download workspace: %w", err)
	}
	defer func() { _ = workspaceRoot.Close() }()
	if err := validateDownloadDirectory(workspaceRoot, destination); err != nil {
		return err
	}
	if err := workspaceRoot.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	if err := validateDownloadDirectory(workspaceRoot, destination); err != nil {
		return err
	}
	root, err := workspaceRoot.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := preflightDestination(root, members); err != nil {
		return err
	}
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		if dir := path.Dir(member.name); dir != "." {
			if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
				return err
			}
			if err := validateDownloadDirectory(root, filepath.FromSlash(dir)); err != nil {
				return err
			}
		}
		in, err := member.file.Open()
		if err != nil {
			return err
		}
		out, err := root.OpenFile(filepath.FromSlash(member.name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
		if err != nil {
			_ = in.Close()
			return fmt.Errorf("open artifact destination %q: %w", member.name, err)
		}
		info, statErr := out.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			return errors.Join(fmt.Errorf("artifact destination %q is not a regular file", member.name), statErr, out.Close(), in.Close())
		}
		if err := out.Chmod(0o644); err != nil {
			return errors.Join(err, out.Close(), in.Close())
		}
		n, copyErr := io.Copy(out, io.LimitReader(contextReader{ctx: ctx, reader: in}, member.size+1))
		err = errors.Join(copyErr, out.Close(), in.Close())
		if err != nil {
			return err
		}
		if n != member.size {
			return fmt.Errorf("artifact ZIP member %q size mismatch", member.name)
		}
	}
	return ctx.Err()
}

func validArtifactMember(name string) bool {
	if name == "" || name == "." || len(name) > actionintegration.MaxUploadArtifactPathBytes || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") || path.Clean(name) != name || strings.HasPrefix(name, "/") {
		return false
	}
	if len(strings.Split(name, "/")) > 256 {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validateDownloadDirectory(root *os.Root, directory string) error {
	if directory == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(filepath.ToSlash(directory), "/") {
		current = path.Join(current, component)
		info, err := root.Lstat(filepath.FromSlash(current))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("artifact path component %q is not a real directory", current)
		}
	}
	return nil
}

func preflightDestination(root *os.Root, members []downloadMember) error {
	for _, member := range members {
		current := ""
		parts := strings.Split(member.name, "/")
		for i, part := range parts {
			current = path.Join(current, part)
			info, err := root.Lstat(filepath.FromSlash(current))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if i < len(parts)-1 {
				if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					return fmt.Errorf("artifact path component %q is not a directory", current)
				}
			} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("artifact destination collision %q is not a regular file", current)
			}
		}
	}
	return nil
}
