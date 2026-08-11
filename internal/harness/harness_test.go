package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMaterializeCreatesCommittedRepositoryAndCleansUp(t *testing.T) {
	source := filepath.Join("..", "..", "testdata", "smoke")
	hookRoot := t.TempDir()
	hookMarker := filepath.Join(hookRoot, "hook-ran")
	if err := os.WriteFile(filepath.Join(hookRoot, "pre-commit"), []byte("#!/bin/sh\ntouch \""+hookMarker+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[core]\n\thooksPath = "+hookRoot+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "wrong-repository"))
	identity := CommitIdentity{
		Name:  "Smoke Harness",
		Email: "smoke@example.invalid",
		When:  time.Date(2026, 7, 22, 1, 2, 3, 0, time.FixedZone("AEST", 10*60*60)),
	}

	repository, err := Materialize(context.Background(), source, identity)
	if err != nil {
		t.Fatal(err)
	}
	repositoryPath := repository.Path
	if len(repository.Commit) != 40 {
		t.Fatalf("commit = %q, want a full Git object ID", repository.Commit)
	}
	if _, err := os.Stat(filepath.Join(repository.Path, ".github", "workflows", "shell.yml")); err != nil {
		t.Fatalf("materialized shell workflow: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, ".git")); !os.IsNotExist(err) {
		t.Fatalf("source .git stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(hookMarker); !os.IsNotExist(err) {
		t.Fatalf("inherited Git hook ran: %v", err)
	}

	gotIdentity, err := gitOutput(context.Background(), repository.Path, "show", "-s", "--format=%an%n%ae%n%aI%n%cn%n%ce%n%cI", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity := strings.Join([]string{
		identity.Name,
		identity.Email,
		identity.When.Format(time.RFC3339),
		identity.Name,
		identity.Email,
		identity.When.Format(time.RFC3339),
	}, "\n")
	if gotIdentity != wantIdentity {
		t.Fatalf("commit identity:\n%s\nwant:\n%s", gotIdentity, wantIdentity)
	}

	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(repositoryPath); !os.IsNotExist(err) {
		t.Fatalf("repository still exists after Close: %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
}

func TestMaterializeRejectsInvalidInput(t *testing.T) {
	validIdentity := CommitIdentity{Name: "Smoke", Email: "smoke@example.invalid", When: time.Unix(1, 0)}

	t.Run("identity", func(t *testing.T) {
		_, err := Materialize(context.Background(), t.TempDir(), CommitIdentity{})
		if err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("error = %v, want identity error", err)
		}
	})

	t.Run("git entry", func(t *testing.T) {
		source := t.TempDir()
		if err := os.Mkdir(filepath.Join(source, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := Materialize(context.Background(), source, validIdentity)
		if err == nil || !strings.Contains(err.Error(), "forbidden .git") {
			t.Fatalf("error = %v, want forbidden .git error", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation needs additional privileges on Windows")
		}
		source := t.TempDir()
		if err := os.Symlink("missing", filepath.Join(source, "link")); err != nil {
			t.Fatal(err)
		}
		_, err := Materialize(context.Background(), source, validIdentity)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("error = %v, want non-regular-file error", err)
		}
	})
}

func TestRepositoryCloseDetectsSourceMutationAndStillCleansUp(t *testing.T) {
	source := t.TempDir()
	fixturePath := filepath.Join(source, "fixture.txt")
	if err := os.WriteFile(fixturePath, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	repository, err := Materialize(context.Background(), source, CommitIdentity{
		Name: "Smoke", Email: "smoke@example.invalid", When: time.Unix(1, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	repositoryPath := repository.Path
	if err := os.WriteFile(fixturePath, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err == nil || !strings.Contains(err.Error(), "source fixture mutated") {
		t.Fatalf("Close() error = %v, want fixture mutation error", err)
	}
	if _, err := os.Stat(repositoryPath); !os.IsNotExist(err) {
		t.Fatalf("repository still exists after Close: %v", err)
	}
}

func TestLookupTargetStagesWorkflows(t *testing.T) {
	shell, err := LookupTarget("shell")
	if err != nil {
		t.Fatal(err)
	}
	if !shell.RuntimeReady || shell.WorkflowPath != ".github/workflows/shell.yml" {
		t.Fatalf("shell target = %+v", shell)
	}
	for _, name := range []string{"ci", "artifact"} {
		target, err := LookupTarget(name)
		if err != nil {
			t.Fatal(err)
		}
		if target.RuntimeReady {
			t.Fatalf("%s target is runtime-ready before its runtime exists", name)
		}
	}
	shell.ExpectedObservationIDs[0] = "mutated"
	again, err := LookupTarget("shell")
	if err != nil {
		t.Fatal(err)
	}
	if again.ExpectedObservationIDs[0] == "mutated" {
		t.Fatal("LookupTarget exposed mutable package state")
	}
	if _, err := LookupTarget("missing"); err == nil || !strings.Contains(err.Error(), "unknown smoke target") {
		t.Fatalf("missing target error = %v", err)
	}
}

func TestNormalizeExcludesProviderAndNativeTransport(t *testing.T) {
	capture := Capture{
		Provider: "buildkite",
		Observations: []Record{
			{Identity: "two", Document: json.RawMessage(`{"b":2,"a":1}`)},
			{Identity: "one", Document: json.RawMessage(`{"number":1.0}`)},
		},
		Lifecycle: []Record{
			{Identity: "action/post", Document: json.RawMessage(`{"phase":"post"}`)},
		},
		ProviderFields: map[string]json.RawMessage{"build_id": json.RawMessage(`"123"`)},
		NativeTransport: []Record{
			{Identity: "diagnostic-artifact", Document: json.RawMessage(`{"digest":"abc"}`)},
		},
	}

	got, err := Normalize(capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "buildkite") || strings.Contains(string(got), "diagnostic-artifact") {
		t.Fatalf("Normalize() retained excluded fields: %s", got)
	}
	want := `{"observations":[{"identity":"one","document":{"number":1.0}},{"identity":"two","document":{"a":1,"b":2}}],"lifecycle":[{"identity":"action/post","document":{"phase":"post"}}]}`
	if string(got) != want {
		t.Fatalf("Normalize() = %s, want %s", got, want)
	}

	other := capture
	other.Provider = "github"
	other.ProviderFields = map[string]json.RawMessage{"run_id": json.RawMessage(`"456"`)}
	other.NativeTransport = nil
	if err := Compare(capture, other); err != nil {
		t.Fatalf("Compare() provider-neutral captures: %v", err)
	}
}

func TestCompareRejectsMissingUnknownDuplicateAndDifferentRecords(t *testing.T) {
	expected := Capture{
		Observations: []Record{{Identity: "result", Document: json.RawMessage(`{"ok":true}`)}},
		Lifecycle:    []Record{{Identity: "action/main", Document: json.RawMessage(`{"result":"success"}`)}},
	}
	tests := []struct {
		name   string
		actual Capture
		want   string
	}{
		{name: "missing observation", actual: Capture{Lifecycle: expected.Lifecycle}, want: `missing expected observation "result"`},
		{name: "unknown observation", actual: Capture{Observations: append(expected.Observations, Record{Identity: "other", Document: json.RawMessage(`null`)}), Lifecycle: expected.Lifecycle}, want: `unknown observation identity "other"`},
		{name: "duplicate observation", actual: Capture{Observations: append(expected.Observations, expected.Observations[0]), Lifecycle: expected.Lifecycle}, want: `duplicate observation identity "result"`},
		{name: "different observation", actual: Capture{Observations: []Record{{Identity: "result", Document: json.RawMessage(`{"ok":false}`)}}, Lifecycle: expected.Lifecycle}, want: `observation "result" differs`},
		{name: "missing lifecycle", actual: Capture{Observations: expected.Observations}, want: `missing expected lifecycle event "action/main"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := Compare(expected, test.actual); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Compare() error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestLifecycleOrderIsPortableEvidence(t *testing.T) {
	main := Record{Identity: "action/main", Document: json.RawMessage(`{"result":"success"}`)}
	post := Record{Identity: "action/post", Document: json.RawMessage(`{"result":"success"}`)}
	expected := Capture{Lifecycle: []Record{main, post}}
	actual := Capture{Lifecycle: []Record{post, main}}

	if err := Compare(expected, actual); err == nil || !strings.Contains(err.Error(), "lifecycle event order differs") {
		t.Fatalf("Compare() error = %v, want lifecycle order error", err)
	}
	got, err := Normalize(actual)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"observations":[],"lifecycle":[{"identity":"action/post","document":{"result":"success"}},{"identity":"action/main","document":{"result":"success"}}]}`
	if string(got) != want {
		t.Fatalf("Normalize() = %s, want %s", got, want)
	}
}

func TestReadObservationRejectsPathEscapes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "observation.json"), []byte("{\n  \"ok\": true\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record, err := ReadObservation(root, "result", "observation.json")
	if err != nil {
		t.Fatal(err)
	}
	if record.Identity != "result" {
		t.Fatalf("identity = %q, want result", record.Identity)
	}

	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"outside":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside.json", outside} {
		if _, err := ReadObservation(root, "escape", path); err == nil || !strings.Contains(err.Error(), "escapes root") {
			t.Fatalf("ReadObservation(%q) error = %v, want path escape", path, err)
		}
	}

	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(root, "link.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadObservation(root, "escape", "link.json"); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("symlink error = %v, want path escape", err)
		}
	}
}
