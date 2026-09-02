package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// This file owns pinned Node runtime selection, installation, and verification
// for JavaScript actions.

const (
	Node16Version = "16.20.2"
	Node20Version = "20.20.2"
	Node24Version = "24.18.0"
	// Digests are for bin/node in the official platform release archives.
	node16LinuxAMD64Digest  = "8440cffda5a21bf7cfda43d2c396f79777585a4c5e03ed2801fe226953a7aa11"
	node20LinuxAMD64Digest  = "6295488653f0d93b0a157841746fef7e72cc4328cfb60c4bbe0ca2668a836ffd"
	node24LinuxAMD64Digest  = "41a74efb34cbde5c7632cdac0cf8bd1a14d0b8d73dc1e82755014d9a9ce70f5c"
	node16DarwinARM64Digest = "83325958463d59cb0b16433eefab0a03fd1ce7d565a27e0274f507b1f3839a6e"
	node20DarwinARM64Digest = "38de4fc456c0c439bac48c727d378f749abb4e31f4116703bb1ee9a746fccbb6"
	node24DarwinARM64Digest = "ee6fb0e015284d83a91e8ec5213f43a157f8a392b58555301682892ba928c04a"
)

type managedNodeVerification struct {
	mu    sync.Mutex
	paths map[int]string
}

func (r Runner) explicitNode(major int) string {
	switch major {
	case 16:
		return r.Node16
	case 20:
		return r.Node20
	case 24:
		return r.Node24
	default:
		return ""
	}
}

func (r *Runner) setExplicitNode(major int, path string) {
	switch major {
	case 16:
		r.Node16 = path
	case 20:
		r.Node20 = path
	case 24:
		r.Node24 = path
	}
}

func nodeTool(major int) string {
	switch major {
	case 16:
		return "core:node@" + Node16Version
	case 20:
		return "core:node@" + Node20Version
	case 24:
		return "core:node@" + Node24Version
	default:
		return ""
	}
}

func (r *jobRun) discoverNode(ctx context.Context, major int, explicit string) (string, error) {
	if explicit != "" || r.ManagedNodeRoot != "" {
		return discoverNodeContext(ctx, major, explicit, r.ManagedNodeRoot)
	}
	tool := nodeTool(major)
	if tool == "" {
		return "", errUnsupportedf("unsupported Node runtime major %d", major)
	}
	if r.Mise == "" {
		return "", fmt.Errorf("mise is required to run JavaScript actions; no pinned runtime path was configured")
	}
	return r.installAndVerifyMiseNode(ctx, major, r.Mise)
}

func (r *jobRun) resolveMiseNodePath(ctx context.Context, major int) (string, error) {
	return r.discoverNode(ctx, major, "")
}

func (r Runner) miseEnv() map[string]string {
	if r.MiseDataDir == "" {
		return nil
	}
	return map[string]string{"MISE_DATA_DIR": r.MiseDataDir}
}

func (r *jobRun) installAndVerifyMiseNode(ctx context.Context, major int, mise string) (string, error) {
	if r.nodeVerification != nil {
		r.nodeVerification.mu.Lock()
		defer r.nodeVerification.mu.Unlock()
		if path := r.nodeVerification.paths[major]; path != "" {
			return path, nil
		}
	}
	tool := nodeTool(major)
	if err := r.installMiseNode(ctx, mise, tool); err != nil {
		return "", err
	}
	installation, node, err := r.miseNodeInstallation(ctx, major, mise)
	if err == nil {
		err = verifyManagedNodeExecutable(ctx, major, node, r.nodeDigest(major))
	}
	if err != nil && r.MiseDataDir != "" {
		if removeErr := removeManagedNodeInstallation(r.MiseDataDir, installation); removeErr != nil {
			return "", errors.Join(fmt.Errorf("cached Node %d failed validation: %w", major, err), removeErr)
		}
		if installErr := r.installMiseNode(ctx, mise, tool); installErr != nil {
			return "", fmt.Errorf("replace invalid cached %s: %w", tool, installErr)
		}
		_, node, err = r.miseNodeInstallation(ctx, major, mise)
		if err == nil {
			err = verifyManagedNodeExecutable(ctx, major, node, r.nodeDigest(major))
		}
		if err != nil {
			return "", fmt.Errorf("replacement %s failed validation: %w", tool, err)
		}
	}
	if err != nil {
		return "", err
	}
	if r.nodeVerification != nil {
		r.nodeVerification.paths[major] = node
	}
	return node, nil
}

func (r Runner) installMiseNode(ctx context.Context, mise, tool string) error {
	cmd := exec.CommandContext(ctx, mise, "--no-config", "install", tool)
	cmd.Env = processEnv(r.miseEnv())
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("install exact %s with mise: %w: %s", tool, err, strings.TrimSpace(output.String()))
	}
	return nil
}

func (r Runner) nodeDigest(major int) string {
	if digest := r.nodeDigests[major]; digest != "" {
		return digest
	}
	return nodeDigest(runtime.GOOS, runtime.GOARCH, major)
}

func nodeDigest(goos, goarch string, major int) string {
	switch goos + "/" + goarch {
	case "linux/amd64":
		switch major {
		case 16:
			return node16LinuxAMD64Digest
		case 20:
			return node20LinuxAMD64Digest
		case 24:
			return node24LinuxAMD64Digest
		}
	case "darwin/arm64":
		switch major {
		case 16:
			return node16DarwinARM64Digest
		case 20:
			return node20DarwinARM64Digest
		case 24:
			return node24DarwinARM64Digest
		}
	default:
		return ""
	}
	return ""
}

func (r Runner) miseNodeInstallation(ctx context.Context, major int, mise string) (string, string, error) {
	tool := nodeTool(major)
	cmd := exec.CommandContext(ctx, mise, "--no-config", "where", tool)
	cmd.Env = processEnv(r.miseEnv())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("resolve cached %s installation: %w: %s", tool, err, strings.TrimSpace(stderr.String()))
	}
	installation, err := filepath.Abs(strings.TrimSpace(string(out)))
	if err != nil {
		return "", "", fmt.Errorf("resolve %s installation path: %w", tool, err)
	}
	if r.MiseDataDir != "" {
		_, installation, err = canonicalPathWithinRealRoot(r.MiseDataDir, installation)
		if err != nil {
			return "", "", fmt.Errorf("validate mise-resolved %s installation: %w", tool, err)
		}
	}
	node := filepath.Join(installation, "bin", "node")
	if r.MiseDataDir != "" {
		_, node, err = canonicalPathWithinRealRoot(r.MiseDataDir, node)
		if err != nil {
			return installation, node, fmt.Errorf("validate mise-resolved %s executable: %w", tool, err)
		}
	}
	info, err := os.Lstat(node)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return installation, node, fmt.Errorf("mise-resolved %s executable is not a regular file", tool)
	}
	resolvedNode, err := filepath.EvalSymlinks(node)
	if err != nil {
		return installation, node, fmt.Errorf("mise-resolved %s executable contains a symlink", tool)
	}
	return installation, resolvedNode, nil
}

func removeManagedNodeInstallation(dataDir, installation string) error {
	_, installation, err := canonicalPathWithinRealRoot(dataDir, installation)
	if err != nil {
		return fmt.Errorf("refusing to remove Node installation: %w", err)
	}
	if err := os.RemoveAll(installation); err != nil {
		return fmt.Errorf("remove invalid cached Node installation: %w", err)
	}
	return nil
}

func canonicalPathWithinRealRoot(root, target string) (string, string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve root: %w", err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", "", fmt.Errorf("inspect root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("root is not a non-symlink directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("canonicalize root: %w", err)
	}
	canonicalRootInfo, err := os.Stat(resolvedRoot)
	if err != nil || !os.SameFile(rootInfo, canonicalRootInfo) {
		return "", "", fmt.Errorf("root changed while canonicalizing")
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", "", fmt.Errorf("resolve path: %w", err)
	}
	lexicalRoot := root
	relative, relativeErr := filepath.Rel(root, target)
	if relativeErr != nil || !pathWithinDirectory(relative) {
		lexicalRoot = resolvedRoot
		relative, relativeErr = filepath.Rel(resolvedRoot, target)
	}
	if relativeErr != nil || !pathWithinDirectory(relative) {
		return "", "", fmt.Errorf("path is outside root")
	}
	current := lexicalRoot
	for component := range strings.SplitSeq(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return "", "", fmt.Errorf("inspect path: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf("path component %q is a symlink", current)
		}
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", "", fmt.Errorf("canonicalize path: %w", err)
	}
	relative, err = filepath.Rel(resolvedRoot, resolvedTarget)
	if err != nil || !pathWithinDirectory(relative) {
		return "", "", fmt.Errorf("path is outside root")
	}
	return resolvedRoot, resolvedTarget, nil
}

func pathWithinDirectory(relative string) bool {
	return relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func verifyManagedNodeExecutable(ctx context.Context, major int, path, want string) error {
	if want == "" {
		return errUnsupportedf("unsupported Node runtime major %d", major)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open Node %d executable: %w", major, err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("hash Node %d executable: %w", major, err)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != want {
		return fmt.Errorf("node %d executable digest %s does not match expected digest %s", major, got, want)
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = processEnv(nil)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("verify exact Node %d executable: %w: %s", major, err, strings.TrimSpace(stderr.String()))
	}
	var wantVersion string
	switch major {
	case 16:
		wantVersion = "v" + Node16Version
	case 20:
		wantVersion = "v" + Node20Version
	case 24:
		wantVersion = "v" + Node24Version
	default:
		wantVersion = fmt.Sprintf("v%d", major)
	}
	if strings.TrimSpace(string(out)) != wantVersion {
		return fmt.Errorf("node %d executable reported %q, want %q", major, strings.TrimSpace(string(out)), wantVersion)
	}
	return nil
}

// discoverNodeContext resolves an explicit Node binary or a binary in the
// managed runtime root, and rejects binaries that do not report the requested
// major. It deliberately does not fall back to PATH.
func discoverNodeContext(ctx context.Context, major int, explicit, managedRoot string) (string, error) {
	var candidates []string
	switch {
	case explicit != "":
		candidates = append(candidates, explicit)
	case managedRoot != "":
		name := "node"
		if runtime.GOOS == "windows" {
			name = "node.exe"
		}
		version := fmt.Sprintf("%d", major)
		candidates = append(candidates,
			filepath.Join(managedRoot, "node"+version, "bin", name),
			filepath.Join(managedRoot, "node", version, "bin", name),
			filepath.Join(managedRoot, "bin", name),
		)
	default:
		return "", fmt.Errorf("node %d is not configured: set the matching Runner.Node field or Runner.ManagedNodeRoot", major)
	}

	var failures []string
	for _, candidate := range candidates {
		command := exec.CommandContext(ctx, candidate, "--version")
		command.Env = processEnv(nil)
		output, err := command.CombinedOutput()
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return "", fmt.Errorf("node %d discovery for %s: %w", major, candidate, cause)
			}
			failures = append(failures, fmt.Sprintf("%s: %v", candidate, err))
			continue
		}
		version := strings.TrimSpace(string(output))
		if strings.HasPrefix(version, fmt.Sprintf("v%d.", major)) {
			return candidate, nil
		}
		failures = append(failures, fmt.Sprintf("%s: reported %q", candidate, version))
	}
	return "", fmt.Errorf("node %d discovery failed: %s", major, strings.Join(failures, "; "))
}
