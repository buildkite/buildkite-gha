//go:build !linux && !darwin && !freebsd

package source

import "errors"

func testUmask(int) (int, error) {
	return 0, errors.ErrUnsupported
}
