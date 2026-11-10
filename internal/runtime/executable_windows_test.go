package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWindowsExecutablePreflightPinsJunctionTarget(t *testing.T) {
	root := t.TempDir()
	original, replacement := filepath.Join(root, "original"), filepath.Join(root, "replacement")
	for _, directory := range []string{original, replacement} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "agent.exe"), []byte(directory), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "agent junction")
	junction := func(target string) {
		t.Helper()
		if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("create junction: %v\n%s", err, output)
		}
	}
	junction(original)
	pinned, err := resolveHostExecutableBeforeWorkflow(filepath.Join(link, "agent.exe"), "", "test agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	junction(replacement)
	got, err := os.ReadFile(pinned)
	if err != nil || string(got) != original {
		t.Fatalf("pinned executable changed with junction: contents=%q error=%v", got, err)
	}
	if _, err := resolveHostExecutableBeforeWorkflow(filepath.Join(root, "missing.exe"), "", "test agent"); err == nil {
		t.Fatal("missing executable accepted")
	}
}
