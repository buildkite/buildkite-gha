package transport

import (
	"strings"
	"testing"
)

func TestClassifyRetry(t *testing.T) {
	key := testKey(t)
	desired := UploadIntent{
		BuildID: testBuildID, ImporterKey: "gha-importer", PipelineDigest: Digest([]byte("pipeline")),
		Jobs: []UploadJob{{Key: "gha-producer", PlanDigest: Digest([]byte("producer"))}, {Key: "gha-consumer", PlanDigest: Digest([]byte("consumer"))}},
	}
	expected, err := SignMarker(key, desired, "expected")
	if err != nil {
		t.Fatal(err)
	}
	completed, err := SignMarker(key, desired, "completed")
	if err != nil {
		t.Fatal(err)
	}
	exact := []ObservedJob{
		{Key: "gha-consumer", PlanDigest: Digest([]byte("consumer")), SignatureVerified: true, SignedFields: []string{"command", "env"}},
		{Key: "gha-producer", PlanDigest: Digest([]byte("producer")), SignatureVerified: true, SignedFields: []string{"env"}},
	}
	tests := []struct {
		name      string
		expected  string
		completed string
		observed  []ObservedJob
		want      RetryClass
		wantErr   bool
	}{
		{name: "fresh", want: RetryFresh},
		{name: "complete", expected: expected.Encoded(), completed: completed.Encoded(), want: RetryAlreadyCompleted},
		{name: "query required", expected: expected.Encoded(), observed: nil, want: RetryNeedsLiveQuery, wantErr: true},
		{name: "no jobs remains ambiguous", expected: expected.Encoded(), observed: []ObservedJob{}, want: RetryNeedsLiveQuery, wantErr: true},
		{name: "exact signed jobs", expected: expected.Encoded(), observed: exact, want: RetryVerifiedCompleted},
		{name: "partial jobs", expected: expected.Encoded(), observed: exact[:1], want: RetryPartial, wantErr: true},
		{name: "unsigned job", expected: expected.Encoded(), observed: []ObservedJob{{Key: "gha-consumer", PlanDigest: Digest([]byte("consumer"))}, exact[1]}, want: RetryPartial, wantErr: true},
		{name: "completed without expected", completed: completed.Encoded(), want: RetryTampered, wantErr: true},
		{name: "tampered marker", expected: expected.Encoded() + "x", want: RetryTampered, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ClassifyRetry(key, desired, test.expected, test.completed, test.observed)
			if got != test.want || (err != nil) != test.wantErr {
				t.Fatalf("ClassifyRetry() = %q, %v; want %q, error=%v", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestClassifyRetryRejectsSignedMarkerForDifferentPipeline(t *testing.T) {
	key := testKey(t)
	desired := UploadIntent{BuildID: testBuildID, ImporterKey: "importer", PipelineDigest: Digest([]byte("one")), Jobs: []UploadJob{{Key: "one", PlanDigest: Digest([]byte("one"))}, {Key: "two", PlanDigest: Digest([]byte("two"))}}}
	other := desired
	other.PipelineDigest = Digest([]byte("two"))
	marker, err := SignMarker(key, other, "expected")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ClassifyRetry(key, desired, marker.Encoded(), "", nil)
	if got != RetryConflict || err == nil || !strings.Contains(err.Error(), "different upload") {
		t.Fatalf("ClassifyRetry() = %q, %v", got, err)
	}
}
