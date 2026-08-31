package compiler

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompilePlansRejectStaticallyUnsupportedShellsWithAttribution(t *testing.T) {
	tests := []struct {
		name                string
		workflow            string
		wantReportedCommand string
		wantLine            int
		wantStep            int
	}{
		{
			name: "step template executable path",
			workflow: `on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - name: Unsupported shell
        shell: /opt/microsoft/powershell/7/pwsh -File {0}
        run: Write-Output test
`,
			wantReportedCommand: "pwsh",
			wantLine:            6,
			wantStep:            1,
		},
		{
			name: "job default",
			workflow: `on: push
jobs:
  test:
    runs-on: ubuntu-latest
    defaults:
      run:
        shell: cmd
    steps:
      - run: echo test
`,
			wantReportedCommand: "cmd",
			wantLine:            3,
		},
		{
			name: "resolved matrix expression",
			workflow: `on: push
jobs:
  test:
    strategy:
      matrix:
        shell: [powershell]
    runs-on: ubuntu-latest
    steps:
      - shell: ${{ matrix.shell }}
        run: Write-Output test
`,
			wantReportedCommand: "powershell",
			wantLine:            9,
			wantStep:            1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := compileUntrustedPlans(".github/workflows/shells.yml", []byte(test.workflow), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-untrusted")
			if err == nil {
				t.Fatal("compileUntrustedPlans() succeeded")
			}
			var finding *ProcessingFinding
			if !errors.As(err, &finding) {
				t.Fatalf("compileUntrustedPlans() error = %T %v, want ProcessingFinding", err, err)
			}
			if finding.Stage != StagePlans || finding.Code != CodePlanConstruction || filepath.Clean(finding.Path) != ".github/workflows/shells.yml" || finding.Line != test.wantLine || finding.Column == 0 {
				t.Fatalf("finding location = %#v", finding)
			}
			if finding.Job != "test" || finding.Instance == "" || finding.Step != test.wantStep {
				t.Fatalf("finding ownership = %#v", finding)
			}
			if finding.Blocker != "shell" || finding.BlockerDetail != test.wantReportedCommand {
				t.Fatalf("finding blocker = %q / %q", finding.Blocker, finding.BlockerDetail)
			}
			for _, want := range []string{
				`shell "` + test.wantReportedCommand + `" is unsupported`,
				"Use bash, sh, python, or a valid custom shell template whose command is available on PATH",
				"https://github.com/buildkite/buildkite-gha",
			} {
				if !strings.Contains(finding.Message, want) {
					t.Fatalf("finding message = %q, want %q", finding.Message, want)
				}
			}
		})
	}
}

func TestCompilePlansDoNotReportEventDerivedShellArguments(t *testing.T) {
	const sentinel = "EVENT-DERIVED-SHELL-SENTINEL"
	workflow := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - shell: pwsh -File {0} ${{ github.event.shell_suffix }}
        run: Write-Output test
`)
	eventSource := pushEvent(t)
	event := bytes.Replace(eventSource, []byte(`"payload": {`), []byte(`"payload": {"shell_suffix": "`+sentinel+`",`), 1)
	if bytes.Equal(event, eventSource) {
		t.Fatal("event payload was not updated")
	}
	_, err := compileUntrustedPlans(".github/workflows/shells.yml", workflow, event, "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err == nil {
		t.Fatal("compileUntrustedPlans() succeeded")
	}
	var finding *ProcessingFinding
	if !errors.As(err, &finding) {
		t.Fatalf("compileUntrustedPlans() error = %T %v, want ProcessingFinding", err, err)
	}
	if strings.Contains(finding.Message, sentinel) || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("event-derived shell argument reached diagnostic: message %q, error %v", finding.Message, err)
	}
	if !strings.Contains(finding.Message, `shell "pwsh" is unsupported`) {
		t.Fatalf("finding message = %q", finding.Message)
	}
}

func TestCompilePlansAcceptCustomShellTemplates(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - shell: bash -l {0}
        run: conda info
      - shell: Rscript {0}
        run: print("R script")
      - shell: julia --color=yes {0}
        run: println("Julia script")
`)
	plans, err := compileUntrustedPlans(".github/workflows/shells.yml", workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].Steps) != 3 {
		t.Fatalf("compiled plans = %#v", plans)
	}
	for i, want := range []string{"bash -l {0}", "Rscript {0}", "julia --color=yes {0}"} {
		if plans[0].Steps[i].Shell != want {
			t.Fatalf("step %d shell = %q, want %q", i+1, plans[0].Steps[i].Shell, want)
		}
	}
}

func TestCompilePlansRetainRuntimeShellExpressions(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  test:
    runs-on: ubuntu-latest
    env:
      SHELL: pwsh
    steps:
      - shell: ${{ env.SHELL }}
        run: Write-Output test
`)
	plans, err := compileUntrustedPlans(".github/workflows/shells.yml", workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].Steps) != 1 || plans[0].Steps[0].Shell != "${{ env.SHELL }}" {
		t.Fatalf("compiled plans = %#v", plans)
	}
}
