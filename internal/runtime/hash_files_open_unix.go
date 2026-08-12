//go:build linux || darwin

package runtime

import (
	"os"

	"golang.org/x/sys/unix"
)

func openHashFile(directory *os.Root, name string) (*os.File, error) {
	return directory.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
}
