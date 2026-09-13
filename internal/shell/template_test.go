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
		{name: "curl Cygwin executable", shell: `D:\cygwin\bin\bash.exe '{0}'`, want: []string{`D:\cygwin\bin\bash.exe`, "{0}"}},
		{name: "quoted Windows executable with spaces", shell: `  "c:\Program Files\Cygwin\bin\bash.exe" '{0}' escaped\ value "escaped\"quote"`, want: []string{`c:\Program Files\Cygwin\bin\bash.exe`, "{0}", "escaped value", `escaped"quote`}},
		{name: "single quoted Windows executable", shell: `'C:\Program Files\Cygwin\bin\bash.exe' {0}`, want: []string{`C:\Program Files\Cygwin\bin\bash.exe`, "{0}"}},
		{name: "escaped Windows executable remains supported", shell: `"C:\\Program Files\\PowerShell\\7\\pwsh.exe" -File {0}`, want: []string{`C:\Program Files\PowerShell\7\pwsh.exe`, "-File", "{0}"}},
		{name: "POSIX executable escapes remain supported", shell: `/opt/shell\ tools/ba\sh "escaped\"quote" {0}`, want: []string{"/opt/shell tools/bash", `escaped"quote`, "{0}"}},
		{name: "Windows-looking argument keeps escape rules", shell: `julia D:\cygwin\bin\bash.exe {0}`, want: []string{"julia", "D:cygwinbinbash.exe", "{0}"}},
		{name: "empty command", shell: `"" {0}`, wantErr: "must contain a command"},
		{name: "missing placeholder", shell: `Rscript --vanilla`, wantErr: "must contain {0}"},
		{name: "malformed quote", shell: `julia "unterminated {0}`, wantErr: "parse shell template"},
		{name: "malformed Windows executable quote", shell: `"D:\cygwin\bin\bash.exe {0}`, wantErr: "parse shell template"},
		{name: "PowerShell template", shell: `/usr/bin/pwsh -File {0}`, want: []string{"/usr/bin/pwsh", "-File", "{0}"}},
		{name: "Windows shell remains unsupported", shell: `cmd.exe /C {0}`, wantErr: "is unsupported"},
		{name: "native cmd path remains unsupported", shell: `C:\Windows\System32\CMD.EXE /C {0}`, wantErr: "is unsupported"},
		{name: "MSYS2 template", shell: `msys2 {0}`, want: []string{"msys2", "{0}"}},
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
		{value: "cmd /C {0}", command: "cmd"},
		{value: "'C:\\Windows\\System32\\CMD.EXE' /C {0}", command: "cmd.exe"},
		{value: `C:\Windows\System32\CMD.EXE /C {0}`, command: "cmd.exe"},
		{value: `"C:\Windows\System32\CMD.EXE" /C {0}`, command: "cmd.exe"},
	} {
		t.Run(test.value, func(t *testing.T) {
			if err := ValidateCompatibility(test.value); err == nil || !strings.Contains(err.Error(), "shell "+strconv.Quote(test.command)+" is unsupported") {
				t.Fatalf("ValidateCompatibility(%q) error = %v", test.value, err)
			}
		})
	}

	for _, value := range []string{"bash", "sh", "pwsh", "PowerShell.exe", "python", "bash -l {0}", "Rscript {0}", "julia --color=yes {0}", "msys2 {0}", "msys2.cmd {0}", "MSYS2.EXE {0}", `julia "unterminated {0}`} {
		t.Run(value, func(t *testing.T) {
			if err := ValidateCompatibility(value); err != nil {
				t.Fatalf("ValidateCompatibility(%q) error = %v", value, err)
			}
		})
	}
}
