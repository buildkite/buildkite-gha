package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWindowsEnvironment(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows environment semantics")
	}
	env := mergeStepEnvironment(map[string]string{"RUNNER_OS": "Windows", "PATH": "original"}, map[string]string{"runner_os": "poisoned", "Path": "replacement"})
	if env["RUNNER_OS"] != "Windows" || env["PATH"] != "replacement" || len(env) != 2 {
		t.Fatalf("environment = %#v", env)
	}
	process := processEnv(env)
	if !strings.Contains(strings.ToUpper(strings.Join(process, "\n")), "SYSTEMROOT=") {
		t.Fatal("missing Windows system environment")
	}
	// Exercise legacy common-appdata resolution in a child with the actual
	// restricted environment, rather than merely asserting an allowlist entry.
	commonAppData := os.Getenv("ALLUSERSPROFILE")
	if commonAppData == "" {
		t.Fatal("Windows host has no ALLUSERSPROFILE")
	}
	command := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `[Environment]::GetFolderPath('CommonApplicationData')`)
	command.Env = processEnv(nil)
	output, err := command.CombinedOutput()
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(output)), commonAppData) {
		t.Fatalf("common appdata = %q, want %q; error=%v", output, commonAppData, err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tool.EXE"), []byte("executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATHEXT", ".COM;.EXE")
	if path, err := resolveExecutableInPath("tool", bin); err != nil || !strings.EqualFold(path, filepath.Join(bin, "tool.exe")) {
		t.Fatalf("lookup = %q, %v", path, err)
	}
}

func TestWindowsProcessHelper(t *testing.T) {
	mode := os.Getenv("GHA_WINDOWS_PROCESS_HELPER")
	if mode == "" {
		return
	}
	if mode == "child" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	child := exec.Command(executable, "-test.run=^TestWindowsProcessHelper$")
	child.Env = append(os.Environ(), "GHA_WINDOWS_PROCESS_HELPER=child")
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(os.Getenv("GHA_WINDOWS_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		os.Exit(4)
	}
	if mode == "parent-wait" {
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestWindowsProcessTree(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows job objects")
	}
	for _, mode := range []string{"parent-exit", "parent-wait", "batch-parent-exit", "batch-parent-wait"} {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"-test.run=^TestWindowsProcessHelper$"}
			if strings.HasPrefix(mode, "batch-") {
				wrapper := filepath.Join(t.TempDir(), "process.cmd")
				if err := os.WriteFile(wrapper, []byte("@echo off\r\n\""+executable+"\" -test.run=^TestWindowsProcessHelper$\r\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				executable, args = wrapper, nil
				mode = strings.TrimPrefix(mode, "batch-")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			processor := newCommandOutputProcessor(io.Discard, io.Discard)
			go func() {
				done <- (Runner{}).runStreaming(ctx, processor, "", map[string]string{"GHA_WINDOWS_PROCESS_HELPER": mode, "GHA_WINDOWS_CHILD_PID": pidFile}, executable, args...)
			}()
			var pid []byte
			deadline := time.After(5 * time.Second)
			for len(pid) == 0 {
				pid, _ = os.ReadFile(pidFile)
				select {
				case <-deadline:
					t.Fatal("child did not start")
				default:
					time.Sleep(10 * time.Millisecond)
				}
			}
			if mode == "parent-wait" {
				cancel()
			}
			select {
			case err := <-done:
				if mode == "parent-exit" && err != nil {
					t.Fatal(err)
				}
				if mode == "parent-wait" && err == nil {
					t.Fatal("cancellation succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("descendant retained output pipes")
			}
			probe := exec.Command("pwsh", "-NoProfile", "-Command", fmt.Sprintf("if (Get-Process -Id %s -ErrorAction SilentlyContinue) { exit 1 }", pid))
			if output, err := probe.CombinedOutput(); err != nil {
				t.Fatalf("child survived: %v: %s", err, output)
			}
		})
	}
}
