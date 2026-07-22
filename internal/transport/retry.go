package transport

import (
	"fmt"
	"slices"
)

// RetryClass is deliberately fail-closed for every ambiguous state.
type RetryClass string

const (
	RetryFresh             RetryClass = "fresh-upload"
	RetryAlreadyCompleted  RetryClass = "already-completed"
	RetryVerifiedCompleted RetryClass = "verified-completed"
	RetryNeedsLiveQuery    RetryClass = "needs-live-query"
	RetryConflict          RetryClass = "conflict"
	RetryTampered          RetryClass = "tampered"
	RetryPartial           RetryClass = "partial-upload"
)

// ObservedJob is the exact verified state required to prove an interrupted
// upload. The current REST response exposes a signature but no verification
// verdict, so SignatureVerified must not be inferred from signature presence.
type ObservedJob struct {
	Key               string
	PlanDigest        string
	SignatureVerified bool
	SignedFields      []string
}

// ClassifyRetry verifies signed markers before using their contents. When the
// expected marker exists without completion, missing or empty observations
// remain ambiguous because query consistency and upload atomicity are live
// oracles.
func ClassifyRetry(key ES256Key, desired UploadIntent, expected, completed string, observed []ObservedJob) (RetryClass, error) {
	desired, err := normalizeIntent(desired)
	if err != nil {
		return RetryConflict, err
	}
	if expected == "" && completed == "" {
		return RetryFresh, nil
	}
	if expected == "" && completed != "" {
		return RetryTampered, fmt.Errorf("completed marker exists without expected marker")
	}
	prepared, err := verifyMarker(key, expected, "expected")
	if err != nil {
		return RetryTampered, fmt.Errorf("verify expected marker: %w", err)
	}
	if !equalIntent(prepared, desired) {
		return RetryConflict, fmt.Errorf("expected marker describes a different upload")
	}
	if completed != "" {
		finished, err := verifyMarker(key, completed, "completed")
		if err != nil {
			return RetryTampered, fmt.Errorf("verify completed marker: %w", err)
		}
		if !equalIntent(finished, desired) {
			return RetryConflict, fmt.Errorf("completed marker describes a different upload")
		}
		return RetryAlreadyCompleted, nil
	}
	if observed == nil {
		return RetryNeedsLiveQuery, fmt.Errorf("prepared upload requires exact live job verification")
	}
	if len(observed) == 0 {
		return RetryNeedsLiveQuery, fmt.Errorf("an empty job query does not prove that retrying is safe")
	}
	if exactObserved(desired.Jobs, observed) {
		return RetryVerifiedCompleted, nil
	}
	return RetryPartial, fmt.Errorf("observed jobs do not exactly match the signed upload intent")
}

func equalIntent(left, right UploadIntent) bool {
	return left.BuildID == right.BuildID && left.ImporterKey == right.ImporterKey && left.PipelineDigest == right.PipelineDigest && slices.Equal(left.Jobs, right.Jobs)
}

func exactObserved(expected []UploadJob, observed []ObservedJob) bool {
	if len(expected) != len(observed) {
		return false
	}
	byKey := make(map[string]ObservedJob, len(observed))
	for _, job := range observed {
		if _, duplicate := byKey[job.Key]; duplicate {
			return false
		}
		byKey[job.Key] = job
	}
	for _, job := range expected {
		actual, ok := byKey[job.Key]
		if !ok || actual.PlanDigest != job.PlanDigest || !actual.SignatureVerified || !slices.Contains(actual.SignedFields, "env") {
			return false
		}
	}
	return true
}
