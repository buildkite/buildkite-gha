package shell

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestParseTemplate(t *testing.T) {
	tests := []struct {
		name    string
		shell   string
		want    []string
		wantErr string
	}{
		{name: "arguments and quotes", shell: `julia --color=yes "two words" {0} --project='quoted value'`, want: []string{"julia", "--color=yes", "two words", "{0}", "--project=quoted value"}},
		{name: "literal values without expansion", shell: `julia "" '$HOME' semi;colon escaped\ value {0}`, want: []string{"julia", "", "$HOME", "semi;colon", "escaped value", "{0}"}},
		{name: "embedded and repeated placeholders", shell: `Rscript --file={0} {0}`, want: []string{"Rscript", "--file={0}", "{0}"}},
		{name: "empty command", shell: `"" {0}`, wantErr: "must contain a command"},
		{name: "missing placeholder", shell: `Rscript --vanilla`, wantErr: "must contain {0}"},
		{name: "malformed quote", shell: `julia "unterminated {0}`, wantErr: "parse shell template"},
		{name: "PowerShell remains unsupported", shell: `/usr/bin/pwsh -File {0}`, wantErr: "PowerShell and Windows shells cannot run"},
		{name: "Windows shell remains unsupported", shell: `cmd.exe /C {0}`, wantErr: "PowerShell and Windows shells cannot run"},
		{name: "MSYS2 remains unsupported", shell: `msys2 {0}`, wantErr: "PowerShell and Windows shells cannot run"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseTemplate(test.shell)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("ParseTemplate(%q) error = %v, want %q", test.shell, err, test.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, test.want) {
				t.Fatalf("ParseTemplate(%q) = %#v, %v, want %#v", test.shell, got, err, test.want)
			}
		})
	}
}

func TestValidateCompatibilityClassifiesUnsupportedCommands(t *testing.T) {
	for _, test := range []struct {
		value   string
		command string
	}{
		{value: "pwsh", command: "pwsh"},
		{value: "PowerShell.exe", command: "powershell.exe"},
		{value: "cmd /C {0}", command: "cmd"},
		{value: "msys2.cmd {0}", command: "msys2.cmd"},
		{value: "/opt/microsoft/powershell/7/pwsh -File {0}", command: "pwsh"},
		{value: `'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe' -File {0}`, command: "powershell.exe"},
	} {
		t.Run(test.value, func(t *testing.T) {
			if err := ValidateCompatibility(test.value); err == nil || !strings.Contains(err.Error(), "shell "+strconv.Quote(test.command)+" is unsupported") {
				t.Fatalf("ValidateCompatibility(%q) error = %v", test.value, err)
			}
		})
	}

	for _, value := range []string{"bash", "sh", "python", "bash -l {0}", "Rscript {0}", "julia --color=yes {0}", `julia "unterminated {0}`} {
		t.Run(value, func(t *testing.T) {
			if err := ValidateCompatibility(value); err != nil {
				t.Fatalf("ValidateCompatibility(%q) error = %v", value, err)
			}
		})
	}
}

func TestCommandOmitsShellArguments(t *testing.T) {
	command, ok := Command(`/usr/bin/Rscript --vanilla "event value"`)
	if !ok || command != "rscript" {
		t.Fatalf("Command() = %q, %t, want rscript, true", command, ok)
	}
}
