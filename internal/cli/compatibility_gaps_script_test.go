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
echo '{"result":"admitted","diagnostics":[{"message":"legacy release warning"}]}'
echo 'buildkite-agent: annotation published' >&2
EOF
  chmod +x "$2"
  exit 0
fi
if [ "$1" = exec ] && [ "$2" = -- ] && [ "$3" = go ] && [ "$4" = test ]; then
  exit 0
fi
exit 2
`)
	writeExecutable(t, filepath.Join(bin, "buildkite-agent"), "#!/bin/sh\ncat >/dev/null\n")

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "compatibility-gaps"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BUILDKITE=true",
		"BUILDKITE_JOB_ID=test-job",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compatibility-gaps: %v\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, "upload-artifact-versions: passing") || !strings.Contains(text, "Compatibility gaps: 16 passing; 0 blocked.") {
		t.Fatalf("compatibility-gaps output:\n%s", text)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
