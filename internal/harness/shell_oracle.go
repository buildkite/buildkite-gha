package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const shellExpectedCapturePath = "expected/shell-capture.json"

var shellFixtureIdentity = CommitIdentity{
	Name:  "buildkite-gha shell oracle",
	Email: "shell-oracle@buildkite.invalid",
	When:  time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
}

// MaterializeShellFixture creates the deterministic repository used by the
// shell provider oracles.
func MaterializeShellFixture(ctx context.Context, source string) (*Repository, error) {
	return Materialize(ctx, source, shellFixtureIdentity)
}

// CompareShellOracle materializes the fixture, requires the exact expected
// fixture commit, captures provider output, and compares its normalized form
// with the checked-in portable expectation.
func CompareShellOracle(ctx context.Context, source, expectedCommit, provider string, output io.Reader) (result []byte, err error) {
	if expectedCommit == "" {
		return nil, errors.New("expected fixture commit is required")
	}
	if provider == "" {
		return nil, errors.New("provider is required")
	}

	repository, err := MaterializeShellFixture(ctx, source)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, repository.Close())
	}()
	if repository.Commit != expectedCommit {
		return nil, fmt.Errorf("fixture commit differs: expected %s, got %s", expectedCommit, repository.Commit)
	}

	capture, err := CaptureShellOutput(provider, output)
	if err != nil {
		return nil, err
	}
	result, err = Normalize(capture)
	if err != nil {
		return nil, fmt.Errorf("normalize shell capture: %w", err)
	}

	root, err := os.OpenRoot(repository.Path)
	if err != nil {
		return nil, fmt.Errorf("open materialized fixture: %w", err)
	}
	expected, readErr := root.ReadFile(shellExpectedCapturePath)
	if err := errors.Join(readErr, root.Close()); err != nil {
		return nil, fmt.Errorf("read expected shell capture: %w", err)
	}
	expected, err = canonicalJSON(expected)
	if err != nil {
		return nil, fmt.Errorf("decode expected shell capture: %w", err)
	}
	actual, err := canonicalJSON(result)
	if err != nil {
		return nil, fmt.Errorf("canonicalize normalized shell capture: %w", err)
	}
	if !bytes.Equal(expected, actual) {
		return nil, fmt.Errorf("normalized shell capture differs: expected %s, got %s", expected, result)
	}
	return result, nil
}
