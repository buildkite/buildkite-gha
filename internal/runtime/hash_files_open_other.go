//go:build !linux && !darwin

package runtime

import "os"

func openHashFile(directory *os.Root, name string) (*os.File, error) {
	return directory.Open(name)
}
