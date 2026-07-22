package harness

import (
	"context"
	"strings"
	"testing"
)

func TestShellOracleMaterializesAndComparesExactFixture(t *testing.T) {
	source := smokeFixturePath()
	repository, err := MaterializeShellFixture(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	commit := repository.Commit
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	output := strings.NewReader(strings.Join([]string{
		`provider prefix SMOKE_OBSERVATION={"variant":"two","result":"smoke-shell"}`,
		`SMOKE_OBSERVATION={"result":"smoke-shell","variant":"one"}`,
	}, "\n"))
	got, err := CompareShellOracle(context.Background(), source, commit, "test", output)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"observations":[{"identity":"consumer[variant=one]","document":{"result":"smoke-shell","variant":"one"}},{"identity":"consumer[variant=two]","document":{"result":"smoke-shell","variant":"two"}}],"lifecycle":[]}`
	if string(got) != want {
		t.Fatalf("CompareShellOracle() = %s, want %s", got, want)
	}
}

func TestShellOracleRejectsCommitAndCaptureDrift(t *testing.T) {
	source := smokeFixturePath()
	valid := strings.NewReader(strings.Join([]string{
		`SMOKE_OBSERVATION={"result":"smoke-shell","variant":"one"}`,
		`SMOKE_OBSERVATION={"result":"smoke-shell","variant":"two"}`,
	}, "\n"))
	if _, err := CompareShellOracle(context.Background(), source, strings.Repeat("0", 40), "test", valid); err == nil || !strings.Contains(err.Error(), "fixture commit differs") {
		t.Fatalf("commit drift error = %v", err)
	}

	repository, err := MaterializeShellFixture(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	commit := repository.Commit
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	drifted := strings.NewReader(strings.Join([]string{
		`SMOKE_OBSERVATION={"result":"wrong","variant":"one"}`,
		`SMOKE_OBSERVATION={"result":"smoke-shell","variant":"two"}`,
	}, "\n"))
	if _, err := CompareShellOracle(context.Background(), source, commit, "test", drifted); err == nil || !strings.Contains(err.Error(), `observation "consumer[variant=one]" differs`) {
		t.Fatalf("capture drift error = %v", err)
	}
}

func smokeFixturePath() string {
	return "../../testdata/smoke"
}
