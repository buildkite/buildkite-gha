package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestManagedNodeDigestsCoverSupportedPlatforms(t *testing.T) {
	for _, platform := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}} {
		for _, major := range []int{16, 20, 24} {
			got := nodeDigest(platform[0], platform[1], major)
			decoded, err := hex.DecodeString(got)
			if err != nil || len(decoded) != sha256.Size {
				t.Errorf("nodeDigest(%q, %q, %d) = %q", platform[0], platform[1], major, got)
			}
		}
	}
	if got := nodeDigest("darwin", "amd64", 24); got != "" {
		t.Fatalf("nodeDigest() unsupported platform = %q", got)
	}
}

func TestDiscoverNodeManagedAndWrongExplicitVersion(t *testing.T) {
	managed := t.TempDir()
	node := filepath.Join(managed, "node24", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, []byte("#!/bin/sh\necho v24.99.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := discoverNodeContext(t.Context(), 24, "", managed)
	if err != nil || got != node {
		t.Fatalf("discoverNodeContext(24) = %q, %v, want %q, nil", got, err, node)
	}

	wrong := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(wrong, []byte("#!/bin/sh\necho v23.1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverNodeContext(t.Context(), 24, wrong, ""); err == nil || !strings.Contains(err.Error(), `reported "v23.1.0"`) {
		t.Fatalf("discoverNodeContext(24) error = %v, want wrong-version detail", err)
	}

	node20 := filepath.Join(managed, "node", "20", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node20), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node20, []byte("#!/bin/sh\necho v20.99.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := discoverNodeContext(t.Context(), 20, "", managed); err != nil || got != node20 {
		t.Fatalf("discoverNodeContext(20) = %q, %v, want %q, nil", got, err, node20)
	}
	if _, err := discoverNodeContext(t.Context(), 24, node20, ""); err == nil || !strings.Contains(err.Error(), `reported "v20.99.0"`) {
		t.Fatalf("discoverNodeContext(24) error = %v, want exact-major rejection", err)
	}
}

func TestDiscoverNodePreservesDeadline(t *testing.T) {
	node := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(node, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	_, err := discoverNodeContext(ctx, 24, node, "")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "node 24 discovery") {
		t.Fatalf("discoverNodeContext(24) error = %v, want contextual deadline exceeded", err)
	}

	if err := os.WriteFile(node, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = discoverNodeContext(t.Context(), 24, node, "")
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("discoverNodeContext(24) error = %v, want genuine discovery failure", err)
	}
}

func TestMiseNodeSelectionIsExactAndConfigFree(t *testing.T) {
	for _, test := range []struct {
		major   int
		version string
	}{
		{major: 16, version: Node16Version},
		{major: 20, version: Node20Version},
	} {
		t.Run(fmt.Sprintf("Node %d", test.major), func(t *testing.T) {
			root := canonicalTempDir(t)
			log := filepath.Join(root, "args")
			dataDir := filepath.Join(root, "data")
			installation := filepath.Join(dataDir, "installs", "node", test.version)
			node := filepath.Join(installation, "bin", "node")
			nodeBytes := fmt.Appendf(nil, "#!/bin/sh\nprintf 'v%s\\n'\n", test.version)
			if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(node, nodeBytes, 0o755); err != nil {
				t.Fatal(err)
			}
			mise := filepath.Join(root, "mise")
			writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", log, installation))
			if err := os.Chmod(mise, 0o700); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(nodeBytes)
			got, err := newJobRun(Runner{Mise: mise, MiseDataDir: dataDir, nodeDigests: map[int]string{test.major: hex.EncodeToString(digest[:])}}).discoverNode(t.Context(), test.major, "")
			if err != nil || got != node {
				t.Fatalf("discoverNode() = %q, %v", got, err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("--no-config install core:node@%s\n--no-config where core:node@%s\n", test.version, test.version)
			if string(data) != want {
				t.Fatalf("mise arguments = %q, want %q", data, want)
			}
		})
	}
}

func TestMiseNodeInstallationAllowsSymlinkedDataDirAncestor(t *testing.T) {
	base := canonicalTempDir(t)
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	logicalParent := filepath.Join(base, "logical")
	if err := os.Symlink(realParent, logicalParent); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	dataDir := filepath.Join(logicalParent, "data")
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	writeNodeExecutable(t, node, 24)
	mise := filepath.Join(base, "mise")
	writeFixtureFile(t, base, "mise", fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q\n", installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	gotInstallation, gotNode, err := (Runner{MiseDataDir: dataDir}).miseNodeInstallation(t.Context(), 24, mise)
	if err != nil {
		t.Fatal(err)
	}
	wantInstallation := filepath.Join(realParent, "data", "installs", "node", Node24Version)
	if gotInstallation != wantInstallation || gotNode != filepath.Join(wantInstallation, "bin", "node") {
		t.Fatalf("miseNodeInstallation() = %q, %q; want canonical paths under %q", gotInstallation, gotNode, wantInstallation)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeNodeExecutable(t, filepath.Join(outside, "node"), 24)
	if err := os.RemoveAll(filepath.Join(wantInstallation, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(wantInstallation, "bin")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Runner{MiseDataDir: dataDir}).miseNodeInstallation(t.Context(), 24, mise); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("miseNodeInstallation() accepted symlinked bin directory: %v", err)
	}
}

func TestMiseNodePathIgnoresProgressOnStderr(t *testing.T) {
	root := canonicalTempDir(t)
	nodeRoot := filepath.Join(root, "node")
	if err := os.MkdirAll(filepath.Join(nodeRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(nodeRoot, "bin", "node")
	writeNodeExecutable(t, node, 24)
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\nprintf 'mise progress\\n' >&2\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", nodeRoot))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	wrongBytes, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	wrongDigest := sha256.Sum256(wrongBytes)
	if _, err := newJobRun(Runner{Mise: mise, nodeDigests: map[int]string{24: hex.EncodeToString(wrongDigest[:])}}).resolveMiseNodePath(t.Context(), 24); err == nil || !strings.Contains(err.Error(), `reported "v24.99.0", want "v24.18.0"`) {
		t.Fatalf("resolveMiseNodePath() error = %v, want exact executable version rejection", err)
	}
	correctBytes := []byte("#!/bin/sh\nprintf 'v24.18.0\\n'\n")
	if err := os.WriteFile(node, correctBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	correctDigest := sha256.Sum256(correctBytes)
	got, err := newJobRun(Runner{Mise: mise, nodeDigests: map[int]string{24: hex.EncodeToString(correctDigest[:])}}).resolveMiseNodePath(t.Context(), 24)
	if err != nil || got != filepath.Join(nodeRoot, "bin", "node") {
		t.Fatalf("resolveMiseNodePath() = %q, %v", got, err)
	}
}

func TestManagedMiseCacheReplacesNodeWithWrongDigest(t *testing.T) {
	root := canonicalTempDir(t)
	dataDir := filepath.Join(root, "data")
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	poisoned := []byte("#!/bin/sh\nprintf 'v24.18.0\\n'\n# poisoned\n")
	if err := os.WriteFile(node, poisoned, 0o755); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("#!/bin/sh\nprintf 'v24.18.0\\n'\n# replacement\n")
	digest := sha256.Sum256(replacement)
	mise := filepath.Join(root, "mise")
	script := fmt.Sprintf(`#!/bin/sh
node=%q
installation=%q
case "$2" in
  install)
    if [ ! -x "$node" ]; then
      mkdir -p "$(dirname "$node")"
      cat > "$node" <<'NODE'
%sNODE
      chmod 0755 "$node"
    fi
    ;;
  where) printf '%%s\n' "$installation" ;;
  *) exit 9 ;;
esac
`, node, installation, replacement)
	if err := os.WriteFile(mise, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	verification := &managedNodeVerification{paths: make(map[int]string)}
	runner := newJobRun(Runner{
		Mise:        mise,
		MiseDataDir: dataDir,
		nodeDigests: map[int]string{24: hex.EncodeToString(digest[:])},
	})
	runner.nodeVerification = verification
	if got, err := runner.discoverNode(t.Context(), 24, ""); err != nil || got != node {
		t.Fatalf("discoverNode() = %q, %v", got, err)
	}
	got, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) || verification.paths[24] != node {
		t.Fatalf("replacement node = %q, verified = %#v", got, verification.paths)
	}
}

func TestManagedMiseCacheRefusesSymlinkedRemoval(t *testing.T) {
	dataDir := canonicalTempDir(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dataDir, "installs")); err != nil {
		t.Fatal(err)
	}
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	if err := removeManagedNodeInstallation(dataDir, installation); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("removeManagedNodeInstallation() error = %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside directory was affected: %v", err)
	}
}

func TestJavaScriptPhaseUsesVerifiedMiseNodeWithoutWorkflowRedirection(t *testing.T) {
	root := canonicalTempDir(t)
	log := filepath.Join(root, "node-args")
	dataDir := filepath.Join(root, "mise-data")
	installation := filepath.Join(dataDir, "installs", "node", Node24Version)
	node := filepath.Join(installation, "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	nodeBytes := fmt.Appendf(nil, "#!/bin/sh\nif [ \"${1:-}\" = --version ]; then printf 'v24.18.0\\n'; else printf '%%s|MISE_DATA_DIR=%%s\\n' \"$*\" \"${MISE_DATA_DIR-unset}\" >> %q; fi\n", log)
	if err := os.WriteFile(node, nodeBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	writeFixtureFile(t, root, "mise", fmt.Sprintf("#!/bin/sh\ncase \"$2\" in install) :;; where) printf '%%s\\n' %q;; *) exit 9;; esac\n", installation))
	if err := os.Chmod(mise, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, root, "main.js", "")
	node20 := filepath.Join(root, "node20")
	writeNodeExecutable(t, node20, 20)
	digest := sha256.Sum256(nodeBytes)
	runner := newJobRun(Runner{Mise: mise, MiseDataDir: dataDir, Node20: node20, nodeDigests: map[int]string{24: hex.EncodeToString(digest[:])}})
	resolvedNode, err := runner.discoverNode(t.Context(), 24, "")
	if err != nil {
		t.Fatal(err)
	}
	result := newResult()
	action := javaScriptAction{Name: "mise", Path: root, Main: "main.js", Env: map[string]string{"MISE_DATA_DIR": "/workflow-controlled"}, nodeMajor: 24}
	if err := runner.runJavaScriptPhase(t.Context(), newCommandOutputProcessor(io.Discard, io.Discard), root, resolvedNode, action, action.Main, nil, nil, &result); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "main.js") + "|MISE_DATA_DIR=/workflow-controlled\n"
	if string(data) != want {
		t.Fatalf("Node invocations = %q, want %q", data, want)
	}
}

func TestMiseMissingIsClear(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := newJobRun(Runner{}).discoverNode(t.Context(), 24, "")
	if err == nil || !strings.Contains(err.Error(), "mise is required") {
		t.Fatalf("discoverNode() error = %v", err)
	}
}

func TestNode20DeclarationUsesNode24ForJavaScriptLifecycle(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/path/action.yml", `name: JavaScript path writer
runs:
  using: node20
  pre: pre.js
  main: main.js
  post: post.js
`)
	writeFixtureFile(t, workspace, ".github/actions/path/pre.js", "")
	writeFixtureFile(t, workspace, ".github/actions/path/main.js", "")
	writeFixtureFile(t, workspace, ".github/actions/path/post.js", "")
	fakeNode := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then
  echo v24.0.0
  exit 0
fi
case "${1##*/}" in
  pre.js) printf '%s\n' "$PATH_ENTRY" >> "$GITHUB_PATH" ;;
  main.js) case ":$PATH:" in *":$PATH_ENTRY:"*) ;; *) exit 9 ;; esac ;;
  post.js) printf 'NODE24_POST=true\n' >> "$GITHUB_ENV" ;;
esac
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	pathEntry := filepath.Join(workspace, "from-pre")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "javascript", Kind: "uses", Uses: "./.github/actions/path"}})
	job.Env = map[string]string{"PATH_ENTRY": pathEntry}

	result, err := (Runner{Node24: fakeNode}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v", result)
	}
	if result.Env["NODE24_POST"] != "true" {
		t.Fatalf("RunJob() environment = %#v, want Node 24 post lifecycle effect", result.Env)
	}
	if result.WarningAnnotations != "" {
		t.Fatalf("RunJob() warnings = %q, want no Node 20 deprecation warning", result.WarningAnnotations)
	}
}

func TestNode16DeclarationUsesExactLifecycleAndWarns(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/node16/action.yml", `name: Node 16 lifecycle
runs:
  using: node16
  pre: pre.js
  main: main.js
  post: post.js
`)
	for _, entry := range []string{"pre.js", "main.js", "post.js"} {
		writeFixtureFile(t, workspace, ".github/actions/node16/"+entry, "")
	}
	fakeNode := filepath.Join(workspace, "node16")
	writeFixtureFile(t, workspace, "node16", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then
  echo v16.20.2
  exit 0
fi
printf 'NODE16_%s=true\n' "$(basename "$1" .js | tr '[:lower:]' '[:upper:]')" >> "$GITHUB_ENV"
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "javascript", Kind: "uses", Uses: "./.github/actions/node16"}})
	var logs bytes.Buffer
	result, err := (Runner{Node16: fakeNode, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	for _, phase := range []string{"PRE", "MAIN", "POST"} {
		if result.Env["NODE16_"+phase] != "true" {
			t.Fatalf("RunJob() environment = %#v, want Node 16 %s lifecycle effect", result.Env, phase)
		}
	}
	warning := fmt.Sprintf(node16DeprecationMessage, "./.github/actions/node16")
	if strings.Count(logs.String(), warning) != 1 || strings.Count(result.WarningAnnotations, warning) != 1 {
		t.Fatalf("RunJob() logs = %q, warnings = %q, want one Node 16 lifecycle warning %q", logs.String(), result.WarningAnnotations, warning)
	}
}

func TestNode16WarningAggregatesInvokedActions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	for _, name := range []string{"alpha", "beta", "skipped"} {
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/action.yml", "name: "+name+"\nruns:\n  using: node16\n  main: main.js\n")
		writeFixtureFile(t, workspace, ".github/actions/"+name+"/main.js", "")
	}
	writeFixtureFile(t, workspace, ".github/actions/modern/action.yml", "name: modern\nruns:\n  using: node20\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/modern/main.js", "")
	node16 := filepath.Join(workspace, "node16")
	node24 := filepath.Join(workspace, "node24")
	writeNodeExecutable(t, node16, 16)
	writeNodeExecutable(t, node24, 24)
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
		{ID: "beta", Kind: "uses", Uses: "./.github/actions/beta"},
		{ID: "alpha", Kind: "uses", Uses: "./.github/actions/alpha"},
		{ID: "alpha-again", Kind: "uses", Uses: "./.github/actions/alpha"},
		{ID: "skipped", Kind: "uses", Uses: "./.github/actions/skipped", Condition: "false"},
		{ID: "modern", Kind: "uses", Uses: "./.github/actions/modern"},
	})
	var logs bytes.Buffer
	result, err := (Runner{Node16: node16, Node24: node24, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	warning := fmt.Sprintf(node16DeprecationMessage, "./.github/actions/alpha, ./.github/actions/beta")
	if strings.Count(logs.String(), warning) != 1 || strings.Count(result.WarningAnnotations, warning) != 1 {
		t.Fatalf("RunJob() logs = %q, warnings = %q, want one aggregate warning %q", logs.String(), result.WarningAnnotations, warning)
	}
	for _, absent := range []string{"./.github/actions/skipped", "./.github/actions/modern"} {
		if strings.Contains(result.WarningAnnotations, absent) {
			t.Fatalf("RunJob() warnings = %q, unexpectedly named %q", result.WarningAnnotations, absent)
		}
	}
}

func TestNode16WarningSurvivesSuppressionAndMasksReferences(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/sensitive/action.yml", "name: sensitive\nruns:\n  using: node16\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/sensitive/main.js", "")
	fakeNode := filepath.Join(workspace, "node16")
	writeFixtureFile(t, workspace, "node16", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v16.20.2; exit 0; fi
printf '%s\n' '::add-mask::sensitive'
head -c 1048577 /dev/zero | tr '\000' x
printf '\n'
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "node16", Kind: "uses", Uses: "./.github/actions/sensitive"}})
	var logs bytes.Buffer
	result, err := (Runner{Node16: fakeNode, Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), "line exceeds 1048576-byte limit") || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v, want oversized-line failure", result, err)
	}
	warningLog := logs.String()
	if warningIndex := strings.LastIndex(warningLog, "warning: Node.js 16 actions are deprecated"); warningIndex >= 0 {
		warningLog = warningLog[warningIndex:]
	}
	for _, output := range []string{warningLog, result.WarningAnnotations} {
		if !strings.Contains(output, "Node.js 16 actions are deprecated") || !strings.Contains(output, "./.github/actions/***") || strings.Contains(output, "sensitive") {
			t.Fatalf("RunJob() warning = %q, want masked Node 16 warning after stream suppression", output)
		}
	}
}

func TestNode16WarningHasPriorityOverUntrustedAnnotationLimit(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/node16/action.yml", "name: node16\nruns:\n  using: node16\n  main: main.js\n")
	writeFixtureFile(t, workspace, ".github/actions/node16/main.js", "")
	fakeNode := filepath.Join(workspace, "node16")
	writeFixtureFile(t, workspace, "node16", `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then echo v16.20.2; exit 0; fi
i=0
while [ "$i" -lt 17 ]; do
  printf '%s' '::warning::'
  head -c 65536 /dev/zero | tr '\000' x
  printf '\n'
  i=$((i + 1))
done
`)
	if err := os.Chmod(fakeNode, 0o700); err != nil {
		t.Fatal(err)
	}
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "node16", Kind: "uses", Uses: "./.github/actions/node16"}})
	var logs bytes.Buffer
	result, err := (Runner{Node16: fakeNode, Stdout: io.Discard, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	warning := fmt.Sprintf(node16DeprecationMessage, "./.github/actions/node16")
	if strings.Count(logs.String(), warning) != 1 || strings.Count(result.WarningAnnotations, warning) != 1 {
		t.Fatalf("RunJob() logs = %q, warnings contain Node 16 message %d times, want one priority warning", logs.String(), strings.Count(result.WarningAnnotations, warning))
	}
	if len(result.WarningAnnotations) > maxJobAnnotationBytes || !utf8.ValidString(result.WarningAnnotations) || !strings.HasSuffix(result.WarningAnnotations, workflowCommandTruncationNotice) {
		t.Fatalf("RunJob() warning annotation bytes = %d, valid UTF-8 = %v, suffix present = %v", len(result.WarningAnnotations), utf8.ValidString(result.WarningAnnotations), strings.HasSuffix(result.WarningAnnotations, workflowCommandTruncationNotice))
	}
}
