package expression

import (
	"errors"
	"fmt"
	"strings"
)

// ErrRunValueUnavailable reports a compile-time reference to a value GitHub
// assigns to each run, such as github.run_attempt.
var ErrRunValueUnavailable = errors.New("run_id, run_number, and run_attempt exist only while a job runs, not when the workflow is imported")

// importGitHubValues lists the github properties a compile-time expression may
// read. It mirrors the compile-time validator allowlist.
const importGitHubValues = "actor, base_ref, event.<property>, event_name, head_ref, ref, ref_name, ref_type, repository, repository_owner, sha, workflow"

// authoredError marks an error whose text quotes only workflow-authored
// expression text, such as context, function, and reference names, and never
// an evaluated value.
type authoredError struct{ err error }

func (e authoredError) Error() string { return e.err.Error() }
func (e authoredError) Unwrap() error { return e.err }

func authoredErrorf(format string, args ...any) error {
	return authoredError{fmt.Errorf(format, args...)}
}

// AuthoredErrorText returns err's text when it quotes only workflow-authored
// expression text. It reports false when any wrapper added text, because that
// text may quote evaluated values.
func AuthoredErrorText(err error) (string, bool) {
	var authored authoredError
	if !errors.As(err, &authored) || authored.Error() != err.Error() {
		return "", false
	}
	return err.Error(), true
}

// unavailableGitHubValueError explains a github reference that has no value
// when a workflow is imported.
func unavailableGitHubValueError(reference, property string) error {
	switch strings.ToLower(property) {
	case "run_id", "run_number", "run_attempt":
		return authoredErrorf("compile-time expression references unavailable value %q: %w; github values available at import: %s", reference, ErrRunValueUnavailable, importGitHubValues)
	}
	return authoredErrorf("compile-time expression references unavailable value %q; github values available at import: %s", reference, importGitHubValues)
}
