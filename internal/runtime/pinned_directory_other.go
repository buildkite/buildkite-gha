//go:build !linux

package runtime

import "fmt"

type pinnedDirectory struct{}

func pinDirectory(string) (*pinnedDirectory, error) {
	return nil, fmt.Errorf("pinned Docker directories are unsupported on this platform")
}

func (*pinnedDirectory) source() string { return "" }
func (*pinnedDirectory) widen() error   { return nil }
func (*pinnedDirectory) restore() error { return nil }
func (*pinnedDirectory) close() error   { return nil }
