//go:build !windows

package runtime

import "path/filepath"

func canonicalHostExecutable(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
