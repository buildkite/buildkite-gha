package integration

import (
	"errors"
	"fmt"
	"strings"
)

type unsupportedVersionError struct {
	action    string
	commit    string
	boundary  string
	supported []string
}

func (e *unsupportedVersionError) Error() string {
	return fmt.Sprintf("%s %s does not admit resolved commit %q; supported commits are %s", e.action, e.boundary, e.commit, strings.Join(e.supported, ", "))
}

// UnsupportedVersionDiagnostic separates corrective version guidance from
// immutable admission details for a rejected action integration.
func UnsupportedVersionDiagnostic(reference string, err error) (message, detail string, ok bool) {
	var versionErr *unsupportedVersionError
	if !errors.As(err, &versionErr) {
		return "", "", false
	}
	requested := ""
	if _, ref, found := strings.Cut(reference, "@"); found {
		requested = strings.TrimSpace(ref)
	}
	anchor := map[string]string{
		"actions/checkout":          "checkout-action",
		"actions/upload-artifact":   "upload-artifact-action",
		"actions/download-artifact": "download-artifact-action",
		"actions/cache":             "cache-action",
	}[versionErr.action]
	docs := "https://github.com/buildkite/buildkite-gha/blob/main/docs/compatibility.md#" + anchor
	if requested == "" {
		message = fmt.Sprintf("This %s version is unsupported. Use a supported version from %s.", versionErr.action, docs)
	} else {
		message = fmt.Sprintf("%s %s is unsupported. Use a supported version from %s.", versionErr.action, requested, docs)
	}
	return message, versionErr.Error(), true
}

func versionError(action, boundary, commit string, supported []string) error {
	return &unsupportedVersionError{action: action, boundary: boundary, commit: commit, supported: supported}
}
