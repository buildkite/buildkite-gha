package integration

import (
	"strings"
	"testing"
)

func TestUnsupportedActionVersionsSeparateGuidanceFromAdmissionDetail(t *testing.T) {
	commit := strings.Repeat("z", 40)
	tests := []struct {
		name      string
		reference string
		validate  func(string) error
		wantDocs  string
		wantBound string
	}{
		{name: "checkout", reference: "actions/checkout@v3", validate: validateCheckoutCommit, wantDocs: "#checkout-action", wantBound: "native adapter"},
		{name: "upload artifact", reference: "actions/upload-artifact@v6.0.2", validate: validateUploadArtifactCommit, wantDocs: "#upload-artifact-action", wantBound: "native adapter"},
		{name: "download artifact", reference: "actions/download-artifact@v9", validate: validateDownloadArtifactCommit, wantDocs: "#download-artifact-action", wantBound: "native adapter"},
		{name: "cache", reference: "actions/cache@v6.0.0", validate: validateCacheCommit, wantDocs: "#cache-action", wantBound: "cache-v2 service"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.validate(commit)
			message, detail, ok := UnsupportedVersionDiagnostic(test.reference, err)
			requested := test.reference[strings.LastIndex(test.reference, "@")+1:]
			if !ok || !strings.Contains(message, requested+" is unsupported") || !strings.Contains(message, test.wantDocs) {
				t.Fatalf("UnsupportedVersionDiagnostic() message = %q, ok = %t", message, ok)
			}
			if strings.Contains(message, commit) || strings.Contains(message, "supported commits") || strings.Contains(message, test.wantBound) {
				t.Fatalf("message contains lower-level detail: %q", message)
			}
			if !strings.Contains(detail, commit) || !strings.Contains(detail, "supported commits") || !strings.Contains(detail, test.wantBound) {
				t.Fatalf("detail = %q", detail)
			}
		})
	}
}
