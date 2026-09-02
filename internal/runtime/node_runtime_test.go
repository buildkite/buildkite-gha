package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagedNodeDigestsCoverSupportedPlatforms(t *testing.T) {
	for _, platform := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}} {
		for _, major := range []int{16, 20, 24} {
			got := nodeDigest(platform[0], platform[1], major)
			decoded, err := hex.DecodeString(got)
			if err != nil || len(decoded) != sha256.Size {
				t.Errorf("nodeDigest(%q, %q, %d) = %q", platform[0], platform[1], major, got)
			}
		}
	}
	if got := nodeDigest("darwin", "amd64", 24); got != "" {
		t.Fatalf("nodeDigest() unsupported platform = %q", got)
	}
}

func TestDiscoverNodeManagedAndWrongExplicitVersion(t *testing.T) {
	managed := t.TempDir()
	node := filepath.Join(managed, "node24", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, []byte("#!/bin/sh\necho v24.99.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := discoverNodeContext(t.Context(), 24, "", managed)
	if err != nil || got != node {
		t.Fatalf("discoverNodeContext(24) = %q, %v, want %q, nil", got, err, node)
	}

	wrong := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(wrong, []byte("#!/bin/sh\necho v23.1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverNodeContext(t.Context(), 24, wrong, ""); err == nil || !strings.Contains(err.Error(), `reported "v23.1.0"`) {
		t.Fatalf("discoverNodeContext(24) error = %v, want wrong-version detail", err)
	}

	node20 := filepath.Join(managed, "node", "20", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node20), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node20, []byte("#!/bin/sh\necho v20.99.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := discoverNodeContext(t.Context(), 20, "", managed); err != nil || got != node20 {
		t.Fatalf("discoverNodeContext(20) = %q, %v, want %q, nil", got, err, node20)
	}
	if _, err := discoverNodeContext(t.Context(), 24, node20, ""); err == nil || !strings.Contains(err.Error(), `reported "v20.99.0"`) {
		t.Fatalf("discoverNodeContext(24) error = %v, want exact-major rejection", err)
	}
}

func TestDiscoverNodePreservesDeadline(t *testing.T) {
	node := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(node, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	_, err := discoverNodeContext(ctx, 24, node, "")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "node 24 discovery") {
		t.Fatalf("discoverNodeContext(24) error = %v, want contextual deadline exceeded", err)
	}

	if err := os.WriteFile(node, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = discoverNodeContext(t.Context(), 24, node, "")
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("discoverNodeContext(24) error = %v, want genuine discovery failure", err)
	}
}
