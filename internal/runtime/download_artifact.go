package runtime

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"golang.org/x/sys/unix"
)

type downloadMember struct {
	file *zip.File
	name string
	size int64
}

func (r Runner) runDownloadArtifact(ctx context.Context, processor *commandProcessor, workspace string, needs map[string]plan.Need, commit string, inputs map[string]string) (result Result, returnErr error) {
	result = newResult()
	defer func() { returnErr = processor.scrubError(returnErr) }()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := actionintegration.ValidateDownloadArtifactRuntimeInputs(commit, inputs); err != nil {
		return result, fmt.Errorf("bounded download-artifact adapter: %w", err)
	}
	values := map[string]string{}
	for k, v := range inputs {
		values[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	name := values["name"]
	destinationSlash, err := actionintegration.NormalizeDownloadArtifactPath(values["path"])
	if err != nil {
		return result, fmt.Errorf("bounded download-artifact adapter: %w", err)
	}
	destinationRelative := filepath.FromSlash(destinationSlash)
	for _, mask := range processor.maskValues() {
		if mask != "" && strings.Contains(name, mask) {
			return result, errors.New("artifact name contains a registered mask and cannot be downloaded")
		}
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
	archive, err := os.OpenFile(archivePath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return result, fmt.Errorf("downloaded archive is not the expected regular file")
	}
	defer func() { _ = archive.Close() }()
	info, err := archive.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return result, fmt.Errorf("downloaded archive is not the expected regular file")
	}
	if info.Size() != artifact.Size {
		return result, fmt.Errorf("downloaded archive size mismatch")
	}
	if err := verifyDownloadDigestFile(ctx, archive, artifact.Digest, artifact.Size); err != nil {
		return result, err
	}
	if err := extractDownloadZIPFile(ctx, archive, artifact.Size, resolvedWorkspace, destinationRelative, artifact.FileCount); err != nil {
		return result, err
	}
	result.Outputs["download-path"] = absDestination
	return result, nil
}

func verifyDownloadDigestFile(ctx context.Context, f *os.File, digest string, size int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(contextReader{ctx: ctx, reader: f}, transport.MaxResultArtifactSizeBytes+1))
	if err != nil {
		return err
	}
	if n != size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		return fmt.Errorf("downloaded archive digest mismatch")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return ctx.Err()
}

func extractDownloadZIPFile(ctx context.Context, f *os.File, size int64, workspace, destination string, expectedCount int) error {
	if err := preflightZIPDirectory(f, size); err != nil {
		return fmt.Errorf("preflight artifact ZIP: %w", err)
	}
	z, err := zip.NewReader(f, size)
	if err != nil {
		return fmt.Errorf("open artifact ZIP: %w", err)
	}
	if len(z.File) != expectedCount || len(z.File) > transport.MaxResultArtifactFileCount {
		return fmt.Errorf("artifact ZIP file count mismatch")
	}
	members := make([]downloadMember, 0, len(z.File))
	foldedNames := make([]string, 0, len(z.File))
	var expanded int64
	for _, f := range z.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := f.Name
		if !validArtifactMember(name) {
			return fmt.Errorf("unsafe artifact ZIP member %q", name)
		}
		if f.Method != zip.Store && f.Method != zip.Deflate || !f.Mode().IsRegular() || f.FileInfo().IsDir() {
			return fmt.Errorf("artifact ZIP member %q is not a supported regular file", name)
		}
		if f.UncompressedSize64 > uint64(transport.MaxResultArtifactSizeBytes) || expanded > transport.MaxResultArtifactSizeBytes-int64(f.UncompressedSize64) {
			return fmt.Errorf("artifact expanded bytes exceed 1 GiB")
		}
		expanded += int64(f.UncompressedSize64)
		foldedNames = append(foldedNames, strings.ToLower(name))
		members = append(members, downloadMember{file: f, name: name, size: int64(f.UncompressedSize64)})
	}
	sort.Slice(foldedNames, func(i, j int) bool { return downloadPathLess(foldedNames[i], foldedNames[j]) })
	for i := 1; i < len(foldedNames); i++ {
		previous, current := foldedNames[i-1], foldedNames[i]
		if current == previous || strings.HasPrefix(current, previous+"/") {
			return fmt.Errorf("duplicate or colliding artifact ZIP member paths")
		}
	}
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		return fmt.Errorf("open download workspace: %w", err)
	}
	defer func() { _ = workspaceRoot.Close() }()
	staging, err := os.MkdirTemp(workspace, ".buildkite-gha-extract-")
	if err != nil {
		return fmt.Errorf("create artifact staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	stagingName := filepath.Base(staging)
	stagingRoot, err := workspaceRoot.OpenRoot(stagingName)
	if err != nil {
		return fmt.Errorf("open artifact staging directory: %w", err)
	}
	defer func() { _ = stagingRoot.Close() }()
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		if dir := path.Dir(member.name); dir != "." {
			if err := stagingRoot.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
				return err
			}
		}
		in, err := member.file.Open()
		if err != nil {
			return err
		}
		out, err := stagingRoot.OpenFile(filepath.FromSlash(member.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
		if err != nil {
			_ = in.Close()
			return fmt.Errorf("open staged artifact member %q: %w", member.name, err)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	return installDownloadMembers(ctx, workspace, staging, destination, members)
}

func downloadPathLess(left, right string) bool {
	for i := 0; i < min(len(left), len(right)); i++ {
		if left[i] == right[i] {
			continue
		}
		if left[i] == '/' {
			return true
		}
		if right[i] == '/' {
			return false
		}
		return left[i] < right[i]
	}
	return len(left) < len(right)
}

func preflightZIPDirectory(reader io.ReaderAt, size int64) error {
	const (
		eocdSize      = 22
		maxZIPComment = 1<<16 - 1
		centralHeader = 46
		eocdSignature = 0x06054b50
		centralSig    = 0x02014b50
		zip16Sentinel = 1<<16 - 1
		zip32Sentinel = 1<<32 - 1
	)
	if size < eocdSize || size > transport.MaxResultArtifactSizeBytes {
		return fmt.Errorf("artifact ZIP size is out of bounds")
	}
	tailSize := min(size, int64(eocdSize+maxZIPComment))
	tail := make([]byte, tailSize)
	if _, err := reader.ReadAt(tail, size-tailSize); err != nil {
		return err
	}
	eocd := -1
	for i := len(tail) - eocdSize; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != eocdSignature {
			continue
		}
		comment := int(binary.LittleEndian.Uint16(tail[i+20:]))
		if i+eocdSize+comment == len(tail) {
			eocd = i
			break
		}
	}
	if eocd < 0 {
		return fmt.Errorf("artifact ZIP end record is missing")
	}
	for i := eocd + 1; i <= len(tail)-eocdSize; i++ {
		if binary.LittleEndian.Uint32(tail[i:]) != eocdSignature {
			continue
		}
		comment := int(binary.LittleEndian.Uint16(tail[i+20:]))
		if i+eocdSize+comment <= len(tail) {
			return fmt.Errorf("artifact ZIP has an ambiguous end record")
		}
	}
	record := tail[eocd:]
	entriesDisk := binary.LittleEndian.Uint16(record[8:])
	entries := binary.LittleEndian.Uint16(record[10:])
	centralSize := binary.LittleEndian.Uint32(record[12:])
	centralOffset := binary.LittleEndian.Uint32(record[16:])
	if binary.LittleEndian.Uint16(record[4:]) != 0 || binary.LittleEndian.Uint16(record[6:]) != 0 || entriesDisk != entries || entries == zip16Sentinel || centralSize == zip32Sentinel || centralOffset == zip32Sentinel {
		return fmt.Errorf("multi-disk or ZIP64 artifact is unsupported")
	}
	if entries == 0 || int(entries) > transport.MaxResultArtifactFileCount {
		return fmt.Errorf("artifact ZIP file count is out of bounds")
	}
	position, end := int64(centralOffset), int64(centralOffset)+int64(centralSize)
	eocdOffset := size - tailSize + int64(eocd)
	if position < 0 || end < position || end != eocdOffset {
		return fmt.Errorf("artifact ZIP central directory is out of bounds")
	}
	count := 0
	for position < end {
		var header [centralHeader]byte
		if _, err := reader.ReadAt(header[:], position); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(header[:]) != centralSig {
			return fmt.Errorf("artifact ZIP central directory is malformed")
		}
		if binary.LittleEndian.Uint32(header[20:]) == zip32Sentinel || binary.LittleEndian.Uint32(header[24:]) == zip32Sentinel || binary.LittleEndian.Uint16(header[34:]) == zip16Sentinel || binary.LittleEndian.Uint32(header[42:]) == zip32Sentinel {
			return fmt.Errorf("ZIP64 artifact member is unsupported")
		}
		nameSize := int64(binary.LittleEndian.Uint16(header[28:]))
		extraSize := int64(binary.LittleEndian.Uint16(header[30:]))
		commentSize := int64(binary.LittleEndian.Uint16(header[32:]))
		extraPosition, extraEnd := position+centralHeader+nameSize, position+centralHeader+nameSize+extraSize
		if extraPosition < position || extraEnd < extraPosition || extraEnd > end {
			return fmt.Errorf("artifact ZIP central directory exceeds bounds")
		}
		for extraPosition < extraEnd {
			if extraEnd-extraPosition < 4 {
				return fmt.Errorf("artifact ZIP central directory extra field is malformed")
			}
			var extraHeader [4]byte
			if _, err := reader.ReadAt(extraHeader[:], extraPosition); err != nil {
				return err
			}
			fieldSize := int64(binary.LittleEndian.Uint16(extraHeader[2:]))
			if binary.LittleEndian.Uint16(extraHeader[:]) == 0x0001 {
				return fmt.Errorf("ZIP64 artifact member is unsupported")
			}
			extraPosition += 4 + fieldSize
			if extraPosition > extraEnd {
				return fmt.Errorf("artifact ZIP central directory extra field is malformed")
			}
		}
		position += centralHeader + nameSize + extraSize + commentSize
		count++
		if count > transport.MaxResultArtifactFileCount || position > end {
			return fmt.Errorf("artifact ZIP central directory exceeds bounds")
		}
	}
	if position != end || count != int(entries) {
		return fmt.Errorf("artifact ZIP central directory count mismatch")
	}
	return nil
}

func installDownloadMembers(ctx context.Context, workspace, staging, destination string, members []downloadMember) error {
	workspaceFD, err := unix.Open(workspace, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open pinned download workspace: %w", err)
	}
	defer func() { _ = unix.Close(workspaceFD) }()
	stagingFD, err := unix.Open(staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open pinned artifact staging directory: %w", err)
	}
	defer func() { _ = unix.Close(stagingFD) }()
	destinationFD, err := openDownloadDirectoryAt(workspaceFD, filepath.ToSlash(destination), true)
	if err != nil {
		return fmt.Errorf("open pinned artifact destination %q: %w", destination, err)
	}
	defer func() { _ = unix.Close(destinationFD) }()
	return installDownloadMembersAt(ctx, stagingFD, destinationFD, members)
}

func installDownloadMembersAt(ctx context.Context, stagingFD, destinationFD int, members []downloadMember) error {
	for _, member := range members {
		if err := preflightDownloadMemberAt(destinationFD, member.name); err != nil {
			return err
		}
	}
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		directory, base := path.Split(member.name)
		sourceParent, err := openDownloadDirectoryAt(stagingFD, strings.TrimSuffix(directory, "/"), false)
		if err != nil {
			return err
		}
		destinationParent, err := openDownloadDirectoryAt(destinationFD, strings.TrimSuffix(directory, "/"), true)
		if err != nil {
			_ = unix.Close(sourceParent)
			return err
		}
		installErr := preflightDownloadBaseAt(destinationParent, base)
		if installErr == nil {
			installErr = unix.Renameat(sourceParent, base, destinationParent, base)
		}
		closeErr := errors.Join(unix.Close(sourceParent), unix.Close(destinationParent))
		if err := errors.Join(installErr, closeErr); err != nil {
			return fmt.Errorf("install artifact destination %q: %w", member.name, err)
		}
	}
	return ctx.Err()
}

func preflightDownloadMemberAt(root int, name string) error {
	directory, base := path.Split(name)
	parent, err := openDownloadDirectoryAt(root, strings.TrimSuffix(directory, "/"), false)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer func() { _ = unix.Close(parent) }()
	return preflightDownloadBaseAt(parent, base)
}

func preflightDownloadBaseAt(parent int, base string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(parent, base, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("artifact destination collision %q is not a regular file", base)
	}
	return nil
}

func openDownloadDirectoryAt(root int, directory string, create bool) (int, error) {
	current, err := unix.Dup(root)
	if err != nil {
		return -1, err
	}
	if directory == "" || directory == "." {
		return current, nil
	}
	for _, component := range strings.Split(directory, "/") {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(current)
			return -1, fmt.Errorf("invalid artifact directory component")
		}
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			if err := unix.Mkdirat(current, component, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(current)
				return -1, err
			}
			next, openErr = unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		_ = unix.Close(current)
		if openErr != nil {
			return -1, openErr
		}
		current = next
	}
	return current, nil
}

func validArtifactMember(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "../") || len(name) > actionintegration.MaxUploadArtifactPathBytes || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") || path.Clean(name) != name || strings.HasPrefix(name, "/") {
		return false
	}
	if len(name) >= 2 && ((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) && name[1] == ':' {
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
