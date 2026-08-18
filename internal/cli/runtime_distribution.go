package cli

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/buildkite/buildkite-gha/internal/compiler"
)

const runtimeDistributionLimit = 256 << 20

func importerPlatform(goos, goarch string) (compiler.Platform, error) {
	switch {
	case goos == "linux" && goarch == "amd64":
		return compiler.PlatformLinuxAMD64, nil
	case goos == "darwin" && goarch == "arm64":
		return compiler.PlatformDarwinARM64, nil
	default:
		return compiler.Platform{}, fmt.Errorf("importer requires linux/amd64 or darwin/arm64, running on %s/%s", goos, goarch)
	}
}

type runtimeDistribution struct {
	contents []byte
	digest   string
}

func loadRuntimeDistributions(paths map[compiler.Platform]string) (map[compiler.Platform]runtimeDistribution, error) {
	distributions := make(map[compiler.Platform]runtimeDistribution, len(paths))
	for platform, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("runtime distribution for %s must use an absolute path", platform)
		}
		pathInfo, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect runtime distribution for %s: %w", platform, err)
		}
		if pathInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("runtime distribution for %s is not a non-symlink executable regular file", platform)
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open runtime distribution for %s: %w", platform, err)
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("inspect opened runtime distribution for %s: %w", platform, err)
		}
		if !os.SameFile(pathInfo, info) {
			_ = file.Close()
			return nil, fmt.Errorf("runtime distribution for %s changed while being opened", platform)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			_ = file.Close()
			return nil, fmt.Errorf("runtime distribution for %s is not a non-symlink executable regular file", platform)
		}
		if info.Size() <= 0 || info.Size() > runtimeDistributionLimit {
			_ = file.Close()
			return nil, fmt.Errorf("runtime distribution for %s must be between 1 and %d bytes", platform, runtimeDistributionLimit)
		}
		contents, err := io.ReadAll(io.LimitReader(file, runtimeDistributionLimit+1))
		closeErr := file.Close()
		if err != nil {
			return nil, fmt.Errorf("read runtime distribution for %s: %w", platform, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close runtime distribution for %s: %w", platform, closeErr)
		}
		if int64(len(contents)) != info.Size() {
			return nil, fmt.Errorf("runtime distribution for %s changed while being read", platform)
		}
		if err := validateRuntimeDistributionBinary(platform, contents); err != nil {
			return nil, fmt.Errorf("validate runtime distribution for %s: %w", platform, err)
		}
		sum := sha256.Sum256(contents)
		distributions[platform] = runtimeDistribution{contents: contents, digest: fmt.Sprintf("sha256:%x", sum)}
	}
	return distributions, nil
}

func validateRuntimeDistributionBinary(platform compiler.Platform, contents []byte) error {
	switch platform {
	case compiler.PlatformLinuxAMD64:
		binary, err := elf.NewFile(bytes.NewReader(contents))
		if err != nil {
			return fmt.Errorf("open ELF executable: %w", err)
		}
		defer func() { _ = binary.Close() }()
		if binary.Class != elf.ELFCLASS64 || binary.Machine != elf.EM_X86_64 || binary.Type != elf.ET_EXEC {
			return fmt.Errorf("want a thin 64-bit linux/amd64 executable")
		}
	case compiler.PlatformDarwinARM64:
		binary, err := macho.NewFile(bytes.NewReader(contents))
		if err != nil {
			return fmt.Errorf("open Mach-O executable: %w", err)
		}
		defer func() { _ = binary.Close() }()
		if binary.Cpu != macho.CpuArm64 || binary.Type != macho.TypeExec {
			return fmt.Errorf("want a thin darwin/arm64 executable")
		}
	default:
		return fmt.Errorf("unsupported platform")
	}
	return nil
}

func executableDigest() (string, error) {
	_, _, digest, err := executable()
	return digest, err
}

func executable() (path string, contents []byte, digest string, err error) {
	path, err = os.Executable()
	if err != nil {
		return "", nil, "", fmt.Errorf("locate compiler executable: %w", err)
	}
	readPath := path
	if runtime.GOOS == "linux" {
		// /proc/self/exe remains bound to the running inode if the launch path is
		// replaced. The plugin importer relies on these exact bytes for Linux jobs.
		readPath = "/proc/self/exe"
	}
	file, err := os.Open(readPath)
	if err != nil {
		return "", nil, "", fmt.Errorf("open running compiler executable: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return "", nil, "", fmt.Errorf("inspect running compiler executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Size() <= 0 || info.Size() > runtimeDistributionLimit {
		_ = file.Close()
		return "", nil, "", fmt.Errorf("running compiler executable must be an executable regular file between 1 and %d bytes", runtimeDistributionLimit)
	}
	contents, err = io.ReadAll(io.LimitReader(file, runtimeDistributionLimit+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return "", nil, "", fmt.Errorf("read running compiler executable: %w", errors.Join(err, closeErr))
	}
	if int64(len(contents)) != info.Size() {
		return "", nil, "", fmt.Errorf("running compiler executable changed while being read")
	}
	platform, err := compiler.ParsePlatform(runtime.GOOS + "/" + runtime.GOARCH)
	if err != nil {
		return "", nil, "", fmt.Errorf("validate running compiler executable: %w", err)
	}
	if err := validateRuntimeDistributionBinary(platform, contents); err != nil {
		return "", nil, "", fmt.Errorf("validate running compiler executable: %w", err)
	}
	sum := sha256.Sum256(contents)
	return path, contents, fmt.Sprintf("sha256:%x", sum), nil
}
