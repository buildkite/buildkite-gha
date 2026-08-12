//go:build !linux && !darwin

package runtime

import (
	"errors"
	"os"
)

func testFileLock(*os.File, bool) error {
	return nil
}

func testExec(string, []string, []string) error {
	return errors.ErrUnsupported
}

func testProcessExists(int) bool {
	return false
}

func testMkfifo(string, uint32) error {
	return errors.ErrUnsupported
}
