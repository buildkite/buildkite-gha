package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

const containerEventDirectory = "/github/workflow"
const containerEventPath = containerEventDirectory + "/event.json"

// newEventFile owns a separate directory, outside the writable runner temp and
// checkout. Containers mount this directory read-only, including for image USERs
// that differ from the host runner. The caller removes it after job teardown.
func newEventFile(event plan.Event, docker bool) (path string, err error) {
	if event.Payload == nil {
		return "", fmt.Errorf("event file requires a hydrated payload")
	}
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return "", fmt.Errorf("encode event file: %w", err)
	}
	dir, err := os.MkdirTemp("", "buildkite-gha-event-file-")
	if err != nil {
		return "", fmt.Errorf("create event file directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("canonicalize event file directory: %w", err)
	}
	path = filepath.Join(canonicalDir, "event.json")
	if err = os.WriteFile(path, payload, 0o400); err != nil {
		return "", fmt.Errorf("write event file: %w", err)
	}
	if docker {
		if err = os.Chmod(path, 0o444); err != nil {
			return "", fmt.Errorf("make event file readable: %w", err)
		}
		if err = os.Chmod(dir, 0o711); err != nil {
			return "", fmt.Errorf("make event file directory traversable: %w", err)
		}
	}
	return path, nil
}
