package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompatibilityGapsKeepsBuildkiteOutputOutOfJSONReports(t *testing.T) {
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "mise"), `#!/bin/sh
set -eu
if [ "$1" = exec ] && [ "$2" = -- ] && [ "$3" = go ] && [ "$4" = build ]; then
  while [ "$1" != -o ]; do shift; done
  cat >"$2" <<'EOF'
#!/bin/sh
case "$*" in
  *github-environments.yml*)
    echo '{"result":"indeterminate","diagnostics":[{"message":"environment resolution unavailable in profile validation"}]}'
    exit 1
    ;;
esac
echo '{"result":"admitted","diagnostics":[{"message":"legacy release warning"}]}'
echo 'buildkite-agent: annotation published' >&2
EOF
  chmod +x "$2"
  exit 0
fi
if [ "$1" = exec ] && [ "$2" = -- ] && [ "$3" = go ] && [ "$4" = test ]; then
  case "$*" in
    *./internal/cli*) exit "$ENVIRONMENT_TEST_STATUS" ;;
  esac
  exit 0
fi
exit 2
`)
	writeExecutable(t, filepath.Join(bin, "buildkite-agent"), "#!/bin/sh\ncat >\"$ANNOTATION_PATH\"\n")

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"0", "1"} {
		t.Run("environment-test-status-"+status, func(t *testing.T) {
			annotationPath := filepath.Join(t.TempDir(), "annotation.md")
			cmd := exec.Command(filepath.Join(root, "scripts", "compatibility-gaps"))
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"BUILDKITE=true",
				"BUILDKITE_JOB_ID=test-job",
				"ENVIRONMENT_TEST_STATUS="+status,
				"ANNOTATION_PATH="+annotationPath,
			)
			output, err := cmd.CombinedOutput()
			if (err == nil) != (status == "0") {
				t.Fatalf("compatibility-gaps: %v\n%s", err, output)
			}
			text := string(output)
			tally := "Compatibility gaps: 16 passing; 0 blocked."
			if status == "1" {
				tally = "Compatibility gaps: 15 passing; 1 blocked."
			}
			if !strings.Contains(text, "upload-artifact-versions: passing") || !strings.Contains(text, tally) {
				t.Fatalf("compatibility-gaps output:\n%s", text)
			}
			annotation, err := os.ReadFile(annotationPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(annotation), "simulated Agent API") || strings.Contains(string(annotation), "environment resolution unavailable") {
				t.Fatalf("compatibility-gaps annotation:\n%s", annotation)
			}
		})
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
