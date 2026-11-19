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
	node16LinuxAMD64Digest   = "8440cffda5a21bf7cfda43d2c396f79777585a4c5e03ed2801fe226953a7aa11"
	node20LinuxAMD64Digest   = "6295488653f0d93b0a157841746fef7e72cc4328cfb60c4bbe0ca2668a836ffd"
	node24LinuxAMD64Digest   = "41a74efb34cbde5c7632cdac0cf8bd1a14d0b8d73dc1e82755014d9a9ce70f5c"
	node16LinuxARM64Digest   = "d09c34f17626ab66c8906508a831ea91dab4e5982b20c4d7a567ceaed42121bb"
	node20LinuxARM64Digest   = "05a69ccdcb795f2a8b86c145e71a6a37cce84fccef5aaf25a8fe38bc9423e732"
	node24LinuxARM64Digest   = "6bf69d0eda41a12030d5f28d958cd09ce323bc0c13f1ab4d8bb426933aa08812"
	node16DarwinARM64Digest  = "83325958463d59cb0b16433eefab0a03fd1ce7d565a27e0274f507b1f3839a6e"
	node20DarwinARM64Digest  = "38de4fc456c0c439bac48c727d378f749abb4e31f4116703bb1ee9a746fccbb6"
	node24DarwinARM64Digest  = "ee6fb0e015284d83a91e8ec5213f43a157f8a392b58555301682892ba928c04a"
	node16WindowsAMD64Digest = "9f498591fd05b3aca332ce4ad966e77cd6e5c150751079ce8bad8ee499945b79"
	node20WindowsAMD64Digest = "56c1520ee33b801e8bdb92fb321cf2e98529735b6d12bd4a2a6dec0ac0bab937"
	node24WindowsAMD64Digest = "9a4eb5f1c29c6a2e93852ead46b999e284a6a5ca8bab4d4e241d587d025a52de"
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

func nodeVersion(major int) string {
	switch major {
	case 16:
		return Node16Version
	case 20:
		return Node20Version
	case 24:
		return Node24Version
	default:
		return ""
	}
}

func (r *jobRun) discoverNode(ctx context.Context, major int, explicit string) (string, error) {
	if explicit != "" || r.ManagedNodeRoot != "" {
		return discoverNodeContext(ctx, major, explicit, r.ManagedNodeRoot)
	}
	if nodeVersion(major) == "" {
		return "", errUnsupportedf("unsupported Node runtime major %d", major)
	}
	if r.nodeVerification != nil {
		r.nodeVerification.mu.Lock()
		defer r.nodeVerification.mu.Unlock()
		if path := r.nodeVerification.paths[major]; path != "" {
			return path, nil
		}
	}
	for _, root := range r.bundledNodeRoots() {
		name := "node"
		if runtime.GOOS == "windows" {
			name = "node.exe"
		}
		candidate := filepath.Join(root, fmt.Sprintf("node%d", major), "bin", name)
		if node, err := discoverNodeContext(ctx, major, candidate, ""); err == nil {
			if r.nodeVerification != nil {
				r.nodeVerification.paths[major] = node
			}
			return node, nil
		}
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
	}
	node, err := r.installNode(ctx, major)
	if err == nil && r.nodeVerification != nil {
		r.nodeVerification.paths[major] = node
	}
	return node, err
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
	case "linux/arm64":
		switch major {
		case 16:
			return node16LinuxARM64Digest
		case 20:
			return node20LinuxARM64Digest
		case 24:
			return node24LinuxARM64Digest
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
	case "windows/amd64":
		switch major {
		case 16:
			return node16WindowsAMD64Digest
		case 20:
			return node20WindowsAMD64Digest
		case 24:
			return node24WindowsAMD64Digest
		}
	default:
		return ""
	}
	return ""
}

func verifyManagedNodeExecutable(ctx context.Context, major int, path, want string) error {
	if want == "" {
		return errUnsupportedf("unsupported Node runtime major %d", major)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect Node %d executable: %w", major, err)
	}
	if !info.Mode().IsRegular() || info.Size() > nodeExecutableLimit {
		return fmt.Errorf("node %d executable is not a bounded regular file", major)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open Node %d executable: %w", major, err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, io.LimitReader(file, nodeExecutableLimit+1))
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
		return fmt.Errorf("verify exact Node %d executable: %w: %s", major, MarkWorkflowProcessTimeout(ctx, err), strings.TrimSpace(stderr.String()))
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
