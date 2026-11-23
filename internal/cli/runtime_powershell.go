package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

const runtimePowerShellVersion = "7.6.9"

// Each job gets a private installation, including PowerShell's supporting DLLs.
// Do not reuse a writable installation left by a previous workflow.
func runtimePowerShellResolver(stderr io.Writer, clientVersion string) (func(context.Context) (string, error), func()) {
	var once sync.Once
	var root, executable string
	var installErr error
	resolve := func(ctx context.Context) (string, error) {
		once.Do(func() {
			root, installErr = os.MkdirTemp("", "buildkite-gha-pwsh-")
			if installErr == nil {
				executable, installErr = installRuntimePowerShell(ctx, root, stderr, clientVersion)
			}
		})
		return executable, installErr
	}
	return resolve, func() {
		if root != "" {
			_ = os.RemoveAll(root)
		}
	}
}

func installRuntimePowerShell(ctx context.Context, root string, stderr io.Writer, clientVersion string) (string, error) {
	asset, digest, err := runtimePowerShellAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(stderr, "~~~ :powershell: Install PowerShell %s (pwsh is missing from PATH)\n", runtimePowerShellVersion)
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" || len(via) >= 10 {
				return errors.New("invalid PowerShell download redirect")
			}
			return nil
		},
	}
	url := "https://github.com/PowerShell/PowerShell/releases/download/v" + runtimePowerShellVersion + "/" + asset
	return installRuntimePowerShellFrom(ctx, root, client, url, asset, digest, clientVersion)
}

func runtimePowerShellAsset(goos, goarch string) (string, string, error) {
	var platform, digest string
	switch goos + "/" + goarch {
	case "linux/amd64":
		platform, digest = "linux-x64", "1da6d6d6035d651f20e70ddee7c4c15ac6ab8b9fb40ee92240d91263f28a884b"
	case "linux/arm64":
		platform, digest = "linux-arm64", "02eb9991af447c7e7fe881acde195961afd397c40b8eb1c2b3856b84ec2d85f2"
	case "darwin/arm64":
		platform, digest = "osx-arm64", "a1c7e98ac403da4be5ba75c1c5855af272de7787a91d478c4267466fe58fc1af"
	case "windows/amd64":
		return "PowerShell-" + runtimePowerShellVersion + "-win-x64.zip", "ce7484804c0f0ad4dda10c958e71acdaf7eebfb22505c1b480f89e0e9f1da94d", nil
	default:
		return "", "", fmt.Errorf("managed PowerShell is unavailable on %s/%s; install pwsh on PATH", goos, goarch)
	}
	return "powershell-" + runtimePowerShellVersion + "-" + platform + ".tar.gz", digest, nil
}

func installRuntimePowerShellFrom(ctx context.Context, root string, client *http.Client, url, asset, digest, clientVersion string) (string, error) {
	archive := filepath.Join(root, asset)
	if err := downloadRuntimeArchive(ctx, client, url, archive, digest, clientVersion); err != nil {
		return "", fmt.Errorf("install PowerShell: %w", err)
	}
	defer func() { _ = os.Remove(archive) }()
	// All supported hosts provide tar (bsdtar on Windows also accepts ZIP).
	// Extract only after verifying the complete official release archive.
	command := exec.Command("tar", "-xf", archive, "-C", root)
	if stdout, stderr, err := (gharuntime.Runner{}).RunCapturedCommand(ctx, command); err != nil {
		return "", fmt.Errorf("extract PowerShell with tar: %w: %s", gharuntime.MarkWorkflowProcessTimeout(ctx, err), strings.TrimSpace(stdout+stderr))
	}
	executable := filepath.Join(root, "pwsh")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	if err := os.Chmod(executable, 0o755); err != nil {
		return "", fmt.Errorf("make PowerShell executable: %w", err)
	}
	command = exec.Command(executable, "--version")
	stdout, stderr, err := (gharuntime.Runner{}).RunCapturedCommand(ctx, command)
	if err != nil {
		return "", fmt.Errorf("validate PowerShell: %w: %s", gharuntime.MarkWorkflowProcessTimeout(ctx, err), strings.TrimSpace(stdout+stderr))
	}
	if strings.TrimSpace(stdout) != "PowerShell "+runtimePowerShellVersion {
		return "", fmt.Errorf("PowerShell reported %q, want %s", strings.TrimSpace(stdout), runtimePowerShellVersion)
	}
	return executable, nil
}
