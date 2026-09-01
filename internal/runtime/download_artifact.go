package runtime

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

type downloadMember struct {
	file   *zip.File
	info   os.FileInfo
	name   string
	staged string
	size   int64
}

type downloadStage struct {
	root        *os.Root
	members     []downloadMember
	memberIndex map[string]int
	foldedNames map[string]string
	stagedFiles int
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
	name, pattern := values["name"], values["pattern"]
	var patterns []string
	var err error
	if pattern != "" {
		patterns, err = actionintegration.DownloadArtifactPatterns(pattern)
		if err != nil {
			return result, fmt.Errorf("bounded download-artifact adapter: %w", err)
		}
	}
	destinationSlash, err := actionintegration.NormalizeDownloadArtifactPath(values["path"])
	if err != nil {
		return result, fmt.Errorf("bounded download-artifact adapter: %w", err)
	}
	destinationRelative := filepath.FromSlash(destinationSlash)
	selector := name
	if selector == "" {
		selector = pattern
	}
	for _, mask := range processor.maskValues() {
		if mask != "" && strings.Contains(selector, mask) {
			return result, errors.New("artifact selector contains a registered mask and cannot be downloaded")
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
			matched := artifact.Name == name
			for _, candidate := range patterns {
				matched, err = doublestar.Match(candidate, artifact.Name)
				if err != nil {
					return result, fmt.Errorf("match artifact pattern: %w", err)
				}
				if matched {
					break
				}
			}
			if matched {
				matches = append(matches, artifact)
			}
		}
	}
	if pattern == "" && len(matches) != 1 {
		return result, fmt.Errorf("artifact lookup found %d verified matches across direct needs, want exactly one", len(matches))
	}
	if pattern != "" && len(matches) == 0 {
		return result, fmt.Errorf("artifact pattern found no verified matches across direct needs")
	}
	if len(matches) > transport.MaxResultArtifacts {
		return result, fmt.Errorf("artifact lookup found %d verified matches, maximum is %d", len(matches), transport.MaxResultArtifacts)
	}
	if pattern != "" {
		names := make(map[string]struct{}, len(matches))
		for _, artifact := range matches {
			key := strings.ToLower(artifact.Name)
			if _, exists := names[key]; exists {
				return result, fmt.Errorf("artifact pattern matched duplicate artifact names across direct needs")
			}
			names[key] = struct{}{}
		}
	}
	if r.Artifacts == nil {
		return result, fmt.Errorf("native artifact store is not configured")
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Name != matches[j].Name {
			return matches[i].Name < matches[j].Name
		}
		if matches[i].Producer.JobID != matches[j].Producer.JobID {
			return matches[i].Producer.JobID < matches[j].Producer.JobID
		}
		return matches[i].Path < matches[j].Path
	})
	remainingFiles := transport.MaxResultArtifactFileCount
	staging, stagingRoot, err := openDownloadStaging(resolvedWorkspace)
	if err != nil {
		return result, err
	}
	defer func() {
		_ = stagingRoot.Close()
		_ = os.RemoveAll(staging)
	}()
	stage := &downloadStage{
		root:        stagingRoot,
		memberIndex: make(map[string]int),
		foldedNames: make(map[string]string),
	}
	for _, artifact := range matches {
		if artifact.FileCount <= 0 || artifact.Size <= 0 || artifact.FileCount > remainingFiles {
			return result, fmt.Errorf("matched artifacts exceed aggregate download limits")
		}
		if err := r.downloadNeedArtifact(ctx, artifact, stage); err != nil {
			return result, err
		}
		remainingFiles -= artifact.FileCount
	}
	sort.Slice(stage.members, func(i, j int) bool { return downloadPathLess(stage.members[i].name, stage.members[j].name) })
	if err := installDownloadMembers(ctx, resolvedWorkspace, stagingRoot, destinationRelative, stage.members); err != nil {
		return result, err
	}
	result.Outputs["download-path"] = absDestination
	return result, nil
}

func (r Runner) downloadNeedArtifact(ctx context.Context, artifact plan.NeedArtifact, stage *downloadStage) error {
	temporary, err := os.MkdirTemp("", "buildkite-gha-download-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	if err := r.Artifacts.DownloadArtifact(ctx, artifact.Path, temporary, artifact.Producer.JobID); err != nil {
		return fmt.Errorf("download native artifact: %w", err)
	}
	archivePath := filepath.Join(temporary, filepath.FromSlash(artifact.Path))
	regular := 0
	var archiveInfo os.FileInfo
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
		archiveInfo = info
		regular++
		return nil
	}); err != nil {
		return fmt.Errorf("download must contain exactly the expected archive: %w", err)
	}
	if regular != 1 {
		return fmt.Errorf("download contained %d regular files, want exactly the expected archive", regular)
	}
	temporaryRoot, err := openPinnedDownloadRoot(temporary)
	if err != nil {
		return fmt.Errorf("open downloaded artifact root: %w", err)
	}
	defer func() { _ = temporaryRoot.Close() }()
	archive, err := temporaryRoot.OpenFile(filepath.FromSlash(artifact.Path), os.O_RDONLY|nonBlockingOpenFlag, 0)
	if err != nil {
		return fmt.Errorf("downloaded archive is not the expected regular file")
	}
	defer func() { _ = archive.Close() }()
	info, err := archive.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(archiveInfo, info) {
		return fmt.Errorf("downloaded archive is not the expected regular file")
	}
	if info.Size() != artifact.Size {
		return fmt.Errorf("downloaded archive size mismatch")
	}
	if err := verifyDownloadDigestFile(ctx, archive, artifact.Digest, artifact.Size); err != nil {
		return err
	}
	if err := stageDownloadZIPFile(ctx, archive, artifact.Size, artifact.FileCount, stage); err != nil {
		return err
	}
	return nil
}

func verifyDownloadDigestFile(ctx context.Context, f *os.File, digest string, size int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx: ctx, reader: f})
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
	staging, stagingRoot, err := openDownloadStaging(workspace)
	if err != nil {
		return err
	}
	defer func() {
		_ = stagingRoot.Close()
		_ = os.RemoveAll(staging)
	}()
	stage := &downloadStage{
		root:        stagingRoot,
		memberIndex: make(map[string]int),
		foldedNames: make(map[string]string),
	}
	if err := stageDownloadZIPFile(ctx, f, size, expectedCount, stage); err != nil {
		return err
	}
	return installDownloadMembers(ctx, workspace, stagingRoot, destination, stage.members)
}

func openDownloadStaging(workspace string) (string, *os.Root, error) {
	workspaceRoot, err := openPinnedDownloadRoot(workspace)
	if err != nil {
		return "", nil, fmt.Errorf("open download workspace: %w", err)
	}
	defer func() { _ = workspaceRoot.Close() }()
	staging, err := os.MkdirTemp(workspace, ".buildkite-gha-extract-")
	if err != nil {
		return "", nil, fmt.Errorf("create artifact staging directory: %w", err)
	}
	stagingInfo, err := os.Lstat(staging)
	if err != nil || !stagingInfo.IsDir() || stagingInfo.Mode()&os.ModeSymlink != 0 {
		_ = os.RemoveAll(staging)
		return "", nil, fmt.Errorf("artifact staging path is not the created directory")
	}
	stagingRoot, err := workspaceRoot.OpenRoot(filepath.Base(staging))
	if err != nil {
		_ = os.RemoveAll(staging)
		return "", nil, fmt.Errorf("open artifact staging directory: %w", err)
	}
	openedInfo, err := stagingRoot.Stat(".")
	if err != nil || !os.SameFile(stagingInfo, openedInfo) {
		_ = stagingRoot.Close()
		_ = os.RemoveAll(staging)
		return "", nil, fmt.Errorf("artifact staging directory changed while opening")
	}
	return staging, stagingRoot, nil
}

func openPinnedDownloadRoot(directory string) (*os.Root, error) {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("path is not a non-symlink directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	openedInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("directory changed while opening")
	}
	return root, nil
}

func stageDownloadZIPFile(ctx context.Context, f *os.File, size int64, expectedCount int, stage *downloadStage) error {
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
		if f.UncompressedSize64 > math.MaxInt64 {
			return fmt.Errorf("artifact ZIP member %q size is not representable", name)
		}
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
	allFoldedNames := make([]string, 0, len(stage.foldedNames)+len(members))
	for folded := range stage.foldedNames {
		allFoldedNames = append(allFoldedNames, folded)
	}
	for _, member := range members {
		folded := strings.ToLower(member.name)
		if existing, ok := stage.foldedNames[folded]; ok && existing != member.name {
			return fmt.Errorf("duplicate or colliding matched artifact member paths")
		}
		if _, ok := stage.foldedNames[folded]; !ok {
			allFoldedNames = append(allFoldedNames, folded)
		}
	}
	sort.Slice(allFoldedNames, func(i, j int) bool { return downloadPathLess(allFoldedNames[i], allFoldedNames[j]) })
	for i := 1; i < len(allFoldedNames); i++ {
		if strings.HasPrefix(allFoldedNames[i], allFoldedNames[i-1]+"/") {
			return fmt.Errorf("duplicate or colliding matched artifact member paths")
		}
	}
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stage.root.MkdirAll(".members", 0o700); err != nil {
			return err
		}
		in, err := member.file.Open()
		if err != nil {
			return err
		}
		staged := filepath.Join(".members", fmt.Sprintf("%d", stage.stagedFiles))
		stage.stagedFiles++
		index, replace := stage.memberIndex[member.name]
		if !replace {
			if err := reserveDownloadMemberPath(stage.root, member.name); err != nil {
				_ = in.Close()
				return fmt.Errorf("reserve staged artifact member %q: %w", member.name, err)
			}
		}
		out, err := stage.root.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			_ = in.Close()
			return fmt.Errorf("open staged artifact member %q: %w", member.name, err)
		}
		if err := out.Chmod(0o644); err != nil {
			return errors.Join(err, out.Close(), in.Close())
		}
		n, copyErr := io.Copy(out, contextReader{ctx: ctx, reader: in})
		info, statErr := out.Stat()
		err = errors.Join(copyErr, statErr, out.Close(), in.Close())
		if err != nil {
			return err
		}
		if n != member.size {
			return fmt.Errorf("artifact ZIP member %q size mismatch", member.name)
		}
		installed := downloadMember{info: info, name: member.name, staged: staged, size: member.size}
		stage.foldedNames[strings.ToLower(member.name)] = member.name
		if replace {
			stage.members[index] = installed
		} else {
			stage.memberIndex[member.name] = len(stage.members)
			stage.members = append(stage.members, installed)
		}
	}
	return ctx.Err()
}

func reserveDownloadMemberPath(root *os.Root, name string) error {
	reserved := path.Join(".paths", name)
	if directory := path.Dir(reserved); directory != "." {
		if err := root.MkdirAll(filepath.FromSlash(directory), 0o700); err != nil {
			return err
		}
	}
	file, err := root.OpenFile(filepath.FromSlash(reserved), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func downloadPathLess(left, right string) bool {
	for i := range min(len(left), len(right)) {
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

// preflightZIPDirectory relies on archive/zip for ordinary and ZIP64 directory
// parsing, then applies format and resource guards that do not cap artifact bytes.
func preflightZIPDirectory(reader io.ReaderAt, size int64) error {
	z, err := zip.NewReader(reader, size)
	if err != nil {
		return err
	}
	if len(z.File) == 0 || len(z.File) > transport.MaxResultArtifactFileCount {
		return fmt.Errorf("artifact ZIP file count is out of bounds")
	}
	for _, file := range z.File {
		if !validArtifactMember(file.Name) {
			return fmt.Errorf("artifact ZIP member name is unsafe")
		}
		if file.Method != zip.Store && file.Method != zip.Deflate || !file.Mode().IsRegular() || file.FileInfo().IsDir() {
			return fmt.Errorf("artifact ZIP member %q is not a supported regular file", file.Name)
		}
		if file.UncompressedSize64 > math.MaxInt64 {
			return fmt.Errorf("artifact ZIP member %q size is not representable", file.Name)
		}
	}
	return nil
}

func installDownloadMembers(ctx context.Context, workspace string, stagingRoot *os.Root, destination string, members []downloadMember) error {
	workspaceRoot, err := openPinnedDownloadRoot(workspace)
	if err != nil {
		return fmt.Errorf("open pinned download workspace: %w", err)
	}
	defer func() { _ = workspaceRoot.Close() }()
	destinationRoot, err := openDownloadDirectoryAt(workspaceRoot, filepath.ToSlash(destination), true)
	if err != nil {
		return fmt.Errorf("open pinned artifact destination %q: %w", destination, err)
	}
	defer func() { _ = destinationRoot.Close() }()
	return installDownloadMembersAt(ctx, stagingRoot, destinationRoot, members)
}

func installDownloadMembersAt(ctx context.Context, stagingRoot, destinationRoot *os.Root, members []downloadMember) error {
	for _, member := range members {
		if err := preflightDownloadMemberAt(destinationRoot, member.name); err != nil {
			return err
		}
	}
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		directory, base := path.Split(member.name)
		source, err := stagingRoot.OpenFile(member.staged, os.O_RDONLY|nonBlockingOpenFlag, 0)
		if err != nil {
			return err
		}
		sourceInfo, err := source.Stat()
		if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Size() != member.size || member.info == nil || !os.SameFile(member.info, sourceInfo) {
			_ = source.Close()
			return fmt.Errorf("staged artifact member %q changed before installation", member.name)
		}
		destinationParent, err := openDownloadDirectoryAt(destinationRoot, strings.TrimSuffix(directory, "/"), true)
		if err != nil {
			_ = source.Close()
			return err
		}
		installErr := preflightDownloadBaseAt(destinationParent, base)
		var temporary string
		if installErr == nil {
			var output *os.File
			output, temporary, installErr = createDownloadDestinationTemp(destinationParent)
			if installErr == nil {
				copied, copyErr := io.Copy(output, io.LimitReader(contextReader{ctx: ctx, reader: source}, member.size+1))
				if copied != member.size && copyErr == nil {
					copyErr = fmt.Errorf("staged artifact member %q size mismatch", member.name)
				}
				installErr = errors.Join(copyErr, output.Chmod(0o644), output.Close(), source.Close())
			} else {
				installErr = errors.Join(installErr, source.Close())
			}
		} else {
			installErr = errors.Join(installErr, source.Close())
		}
		if installErr == nil {
			installErr = preflightDownloadBaseAt(destinationParent, base)
		}
		if installErr == nil {
			installErr = destinationParent.Rename(temporary, base)
			if installErr == nil {
				temporary = ""
			}
		}
		if temporary != "" {
			installErr = errors.Join(installErr, destinationParent.Remove(temporary))
		}
		if err := errors.Join(installErr, destinationParent.Close()); err != nil {
			return fmt.Errorf("install artifact destination %q: %w", member.name, err)
		}
	}
	return ctx.Err()
}

func createDownloadDestinationTemp(root *os.Root) (*os.File, string, error) {
	for range 10 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := ".buildkite-gha-download-" + hex.EncodeToString(random[:])
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, name, err
	}
	return nil, "", fmt.Errorf("create unique artifact destination staging file")
}

func preflightDownloadMemberAt(root *os.Root, name string) error {
	directory, base := path.Split(name)
	parent, err := openDownloadDirectoryAt(root, strings.TrimSuffix(directory, "/"), false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = parent.Close() }()
	return preflightDownloadBaseAt(parent, base)
}

func preflightDownloadBaseAt(parent *os.Root, base string) error {
	info, err := parent.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact destination collision %q is not a regular file", base)
	}
	return nil
}

func openDownloadDirectoryAt(root *os.Root, directory string, create bool) (*os.Root, error) {
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if directory == "" || directory == "." {
		return current, nil
	}
	for component := range strings.SplitSeq(directory, "/") {
		if component == "" || component == "." || component == ".." {
			_ = current.Close()
			return nil, fmt.Errorf("invalid artifact directory component")
		}
		info, statErr := current.Lstat(component)
		if errors.Is(statErr, os.ErrNotExist) && create {
			if err := current.Mkdir(component, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				_ = current.Close()
				return nil, err
			}
			info, statErr = current.Lstat(component)
		}
		if statErr != nil {
			_ = current.Close()
			return nil, statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			_ = current.Close()
			return nil, fmt.Errorf("artifact directory component %q is not a non-symlink directory", component)
		}
		next, openErr := current.OpenRoot(component)
		if openErr != nil {
			_ = current.Close()
			return nil, openErr
		}
		openedInfo, statErr := next.Stat(".")
		if statErr != nil || !os.SameFile(info, openedInfo) {
			_ = current.Close()
			_ = next.Close()
			if statErr != nil {
				return nil, statErr
			}
			return nil, fmt.Errorf("artifact directory component %q changed while opening", component)
		}
		_ = current.Close()
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
