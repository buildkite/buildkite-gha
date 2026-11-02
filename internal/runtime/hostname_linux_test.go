package runtime

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Exercise the public job lifecycle and real Git/libc without ever writing the
// caller's hosts file or hostname. Each case gets private mount/UTS namespaces.
func TestRunJobRepairsHostname(t *testing.T) {
	if mode := os.Getenv("GHA_HOSTNAME_TEST_CASE"); mode != "" {
		runHostnameFixture(t, mode)
		return
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skip("hostname integration requires Linux unshare")
	}
	command := []string{unshare, "--mount", "--uts", "--"}
	if os.Geteuid() != 0 {
		command = append([]string{"/usr/bin/sudo", "-n", "--"}, command...)
	}
	if output, err := exec.Command(command[0], append(command[1:], "true")...).CombinedOutput(); err != nil {
		t.Skipf("hostname integration requires mount/UTS namespaces: %v: %s", err, output)
	}
	for _, mode := range []string{"root", "sudo", "qualified-raw", "qualified-canonical", "absent", "readonly", "denied", "unresolvable", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			fixture := t.TempDir()
			if err := os.Chmod(fixture, 0o777); err != nil {
				t.Fatal(err)
			}
			args := append(append([]string{}, command[1:]...), os.Args[0], "-test.run=^TestRunJobRepairsHostname$", "-test.v")
			cmd := exec.Command(command[0], args...)
			cmd.Env = append(os.Environ(), "GHA_HOSTNAME_TEST_CASE="+mode, "GHA_HOSTNAME_TEST_DIR="+fixture,
				"GHA_HOSTNAME_TEST_MNT="+readNamespace(t, "mnt"), "GHA_HOSTNAME_TEST_UTS="+readNamespace(t, "uts"))
			// sudo normally filters environment; pass only fixture metadata back.
			if os.Geteuid() != 0 {
				cmd.Args = append(cmd.Args[:2], append([]string{"--preserve-env=GHA_HOSTNAME_TEST_CASE,GHA_HOSTNAME_TEST_DIR,GHA_HOSTNAME_TEST_MNT,GHA_HOSTNAME_TEST_UTS,PATH"}, cmd.Args[2:]...)...)
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("isolated fixture: %v\n%s", err, output)
			}
		})
	}
}

func readNamespace(t *testing.T, name string) string {
	t.Helper()
	value, err := os.Readlink("/proc/self/ns/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func runHostnameFixture(t *testing.T, mode string) {
	t.Helper()
	for _, name := range []string{"mnt", "uts"} {
		parent := os.Getenv("GHA_HOSTNAME_TEST_" + strings.ToUpper(name))
		if parent == "" || parent == readNamespace(t, name) {
			t.Fatal("refusing hostname fixture outside isolated namespaces")
		}
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	unchanged := map[string][]byte{}
	for _, path := range []string{"/etc/hostname", "/etc/machine-id"} {
		unchanged[path], _ = os.ReadFile(path)
	}
	bind := func(path, value string) {
		t.Helper()
		source := filepath.Join(os.Getenv("GHA_HOSTNAME_TEST_DIR"), filepath.Base(path))
		if err := os.WriteFile(source, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mount(source, path, "", syscall.MS_BIND, ""); err != nil {
			t.Fatal(err)
		}
	}
	host := "probe-instance"
	hosts := "127.0.0.1 localhost\n127.0.1.1\tprobe-instance retained-alias # keep\n::1 ip6-localhost\n"
	wantEmail := "runner@probe-instance.buildkite.internal"
	wantStatus := "0"
	wantWarning := false
	switch mode {
	case "qualified-raw":
		host = "probe-instance.custom.internal"
		wantEmail = "runner@" + host
	case "qualified-canonical":
		hosts = strings.Replace(hosts, "\tprobe-instance ", "\tprobe-instance.custom.internal probe-instance ", 1)
		wantEmail = "runner@probe-instance.custom.internal"
	case "absent":
		hosts = "127.0.0.1 localhost\n"
		wantStatus, wantWarning = "128", true
	case "readonly", "denied", "unresolvable":
		wantStatus, wantWarning = "128", true
	case "disabled":
		wantStatus = "128"
	}
	if err := syscall.Sethostname([]byte(host)); err != nil {
		t.Fatal(err)
	}
	bind("/etc/hosts", hosts)
	bind("/etc/passwd", "root:x:0:0:,,,:/root:/bin/bash\nrunner:x:1001:1001:,,,:/home/runner:/bin/bash\n")
	bind("/etc/group", "root:x:0:\nrunner:x:1001:\n")
	bind("/etc/shadow", "root:*:20000:0:99999:7:::\nrunner:*:20000:0:99999:7:::\n")
	if _, err := os.Stat("/etc/mailname"); err == nil {
		bind("/etc/mailname", "")
	}
	nss := "passwd: files\ngroup: files\nhosts: files\n"
	if mode == "unresolvable" {
		nss = "passwd: files\ngroup: files\nhosts: no_such_test_module\n"
	}
	bind("/etc/nsswitch.conf", nss)
	if mode == "readonly" {
		if err := syscall.Mount("", "/etc/hosts", "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "sudo" || mode == "denied" {
		sudoers := "root ALL=(ALL) NOPASSWD: ALL\n"
		if mode == "sudo" {
			sudoers += "runner ALL=(ALL) NOPASSWD: ALL\n"
		}
		bind("/etc/sudoers", sudoers)
		if err := os.Chmod("/etc/sudoers", 0o440); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Setgroups([]int{}); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Setgid(1001); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Setuid(1001); err != nil {
			t.Fatal(err)
		}
	} else {
		// Root tests exercise writing without sudo and use the root OS identity.
		wantEmail = strings.Replace(wantEmail, "runner@", "root@", 1)
	}
	workspace := t.TempDir()
	t.Setenv("HOME", workspace)
	workflow := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflow, "name: hostname fixture\n")
	job := runtimePlan(t, workspace, workflow, []runtimeTestStep{{ID: "commit", Kind: "run", Command: `
export GIT_CONFIG_NOSYSTEM=1
git init -q
status=0
git -c user.name='Probe Name' -c commit.gpgsign=false commit --allow-empty -qm probe || status=$?
echo "status=$status" >> "$GITHUB_OUTPUT"
if [ "$status" = 0 ]; then git show -s --format='email=%ae' >> "$GITHUB_OUTPUT"; fi
git -c user.name='Explicit Name' -c user.email=explicit@example.invalid -c commit.gpgsign=false commit --allow-empty -qm explicit
git show -s --format='explicit=%ae,%ce' >> "$GITHUB_OUTPUT"
GIT_AUTHOR_NAME='Author' GIT_AUTHOR_EMAIL=author@example.invalid GIT_COMMITTER_NAME='Committer' GIT_COMMITTER_EMAIL=committer@example.invalid git -c user.email=config@example.invalid -c commit.gpgsign=false commit --allow-empty -qm env
git show -s --format='roles=%an <%ae>,%cn <%ce>' >> "$GITHUB_OUTPUT"
status=0
git -c user.name='Probe Name' -c user.useConfigOnly=true var GIT_AUTHOR_IDENT >/dev/null 2>&1 || status=$?
test "$status" = 128
status=0
git var GIT_AUTHOR_IDENT >/dev/null 2>&1 || status=$?
test "$status" = 128
`}})
	job.Outputs = map[string]string{"status": "${{ steps.commit.outputs.status }}", "email": "${{ steps.commit.outputs.email }}", "explicit": "${{ steps.commit.outputs.explicit }}", "roles": "${{ steps.commit.outputs.roles }}"}
	var stdout, stderr bytes.Buffer
	result, err := (Runner{RepairHostname: mode != "disabled", Stdout: &stdout, Stderr: &stderr}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Outputs["status"] != wantStatus || result.Outputs["explicit"] != "explicit@example.invalid,explicit@example.invalid" {
		t.Fatalf("result=%+v error=%v\n%s\n%s", result, err, &stdout, &stderr)
	}
	if wantStatus == "0" && result.Outputs["email"] != wantEmail {
		t.Fatalf("inferred email=%q, want %q", result.Outputs["email"], wantEmail)
	}
	if result.Outputs["roles"] != "Author <author@example.invalid>,Committer <committer@example.invalid>" {
		t.Fatalf("explicit role identities changed: %q", result.Outputs["roles"])
	}
	if strings.Contains(stderr.String(), "warning: cannot repair canonical hostname") != wantWarning {
		t.Fatalf("warning mismatch: %s", &stderr)
	}
	gotHost, err := os.Hostname()
	if err != nil || gotHost != host {
		t.Fatalf("raw hostname changed: %q, %v", gotHost, err)
	}
	gotHosts, err := os.ReadFile("/etc/hosts")
	if err != nil {
		t.Fatal(err)
	}
	wantHosts := hosts
	if mode == "root" || mode == "sudo" || mode == "unresolvable" {
		wantHosts = strings.Replace(hosts, "\tprobe-instance ", "\tprobe-instance.buildkite.internal probe-instance ", 1)
	}
	if string(gotHosts) != wantHosts {
		t.Fatalf("hosts changed unexpectedly:\n%s\nwant:\n%s", gotHosts, wantHosts)
	}
	for path, before := range unchanged {
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatalf("%s changed", path)
		}
	}
	fmt.Printf("%s: workflow success; name-only commit=%s; explicit identities preserved\n", mode, wantStatus)
}
