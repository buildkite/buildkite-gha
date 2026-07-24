//go:build linux

package runtime

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

type pinnedDirectory struct {
	file *os.File
	mode os.FileMode
}

func pinDirectory(path string) (*pinnedDirectory, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("pin directory %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat pinned directory %q: %w", path, err)
	}
	return &pinnedDirectory{file: file, mode: info.Mode().Perm()}, nil
}

func (p *pinnedDirectory) source() string {
	// Docker must resolve the retained inode, not the mutable workspace name.
	// This requires a local Linux daemon that can see this process in /proc;
	// failure to resolve it is intentionally fatal rather than falling back.
	return "/proc/" + strconv.Itoa(os.Getpid()) + "/fd/" + strconv.FormatUint(uint64(p.file.Fd()), 10)
}

func (p *pinnedDirectory) widen() error   { return p.file.Chmod(0o777) }
func (p *pinnedDirectory) restore() error { return p.file.Chmod(p.mode) }
func (p *pinnedDirectory) close() error   { return p.file.Close() }
