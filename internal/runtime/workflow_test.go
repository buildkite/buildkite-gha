package runtime

import (
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestVerifyWorkflowDoesNotReadRemoteCalleeFromCallerWorkspace(t *testing.T) {
	job := plan.Job{Workflow: plan.Workflow{
		Path: "owner/repository/.github/workflows/ci.yml@v1", Digest: "sha256:" + strings.Repeat("a", 64),
		Remote: &plan.RemoteWorkflowSource{
			Repository: "owner/repository", RequestedRef: "v1", Commit: strings.Repeat("b", 40), SourceDigest: "sha256:" + strings.Repeat("c", 64),
		},
	}}
	if err := verifyWorkflow(job, t.TempDir()); err != nil {
		t.Fatalf("verifyWorkflow() remote binding error = %v", err)
	}

	job.Workflow.Remote = nil
	if err := verifyWorkflow(job, t.TempDir()); err == nil || !strings.Contains(err.Error(), "verify workflow binding") {
		t.Fatalf("verifyWorkflow() local missing file error = %v", err)
	}
}
