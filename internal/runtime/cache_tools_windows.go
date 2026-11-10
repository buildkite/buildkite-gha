package runtime

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// zstd.exe from https://github.com/facebook/zstd/releases/download/v1.5.7/zstd-v1.5.7-win64.zip.
// This build imports only Windows KERNEL32.dll and msvcrt.dll, not installation-local DLLs.
const windowsCacheZstdDigest = "8076aae03feac7c66b319579e82172eed168deed2a3f25e5e2d3c60f55e84111"

func isolateCacheToolEnvironment(env map[string]string) (func(), error) {
	// The audited bundles derive tar paths from PROGRAMFILES and SYSTEMDRIVE.
	// Read installation paths from Windows, never workflow or agent environment
	// variables. Do not admit workspace tools or package-manager shims via PATH.
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return nil, fmt.Errorf("locate cache tools Program Files: %w", err)
	}
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, fmt.Errorf("locate cache tools System32: %w", err)
	}
	systemRoot := filepath.Dir(systemDirectory)
	// The stock tools directory permits unprivileged file creation. Never put
	// it on PATH: stage only known executable bytes, without sibling tools/DLLs.
	// Hashing the same bytes we write also makes source reparse points harmless.
	zstd, err := stageCacheZstd(filepath.Join(filepath.VolumeName(systemDirectory)+`\`, "tools", "zstd", "zstd.exe"))
	if err != nil {
		return nil, err
	}
	paths := []string{filepath.Join(programFiles, "Git", "usr", "bin"), filepath.Join(programFiles, "zstd")}
	if zstd != "" {
		paths = append(paths, zstd)
	}
	paths = append(paths, systemDirectory)
	mergeEnvironmentInto(env, map[string]string{
		"PROGRAMFILES":                       programFiles,
		"SYSTEMDRIVE":                        filepath.VolumeName(systemDirectory),
		"SYSTEMROOT":                         systemRoot,
		"WINDIR":                             systemRoot,
		"COMSPEC":                            filepath.Join(systemDirectory, "cmd.exe"),
		"PATHEXT":                            ".EXE",
		"NODEFAULTCURRENTDIRECTORYINEXEPATH": "1",
		"PATH":                               strings.Join(paths, ";"),
	})
	return func() {
		if zstd != "" {
			_ = os.RemoveAll(zstd)
		}
	}, nil
}

func stageCacheZstd(candidate string) (string, error) {
	file, err := os.Open(candidate)
	if err != nil {
		return "", nil // Missing or unreadable zstd retains upstream gzip fallback.
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 4<<20))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != windowsCacheZstdDigest {
		return "", nil
	}
	directory, err := os.MkdirTemp("", "buildkite-gha-cache-tools-")
	if err != nil {
		return "", fmt.Errorf("prepare cache zstd directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "zstd.exe"), data, 0o600); err != nil {
		_ = os.RemoveAll(directory)
		return "", fmt.Errorf("stage cache zstd: %w", err)
	}
	return directory, nil
}
