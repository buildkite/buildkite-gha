package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var windowsProcessJobs = struct {
	sync.Mutex
	jobs map[int]windows.Handle
}{jobs: make(map[int]windows.Handle)}

// Batch wrappers need cmd.exe, not CreateProcess's native-executable quoting.
// Carry values through one percent-expansion pass, inside quotes: literal %,
// !, &, and ^ must not become command syntax or undergo recursive expansion.
// Wrappers that re-evaluate arguments (CALL or delayed expansion) are not safe
// forwarding interfaces. setup-msys2's wrapper forwards %* directly to bash.
func prepareProcessCommand(command *exec.Cmd) error {
	if !strings.EqualFold(filepath.Ext(command.Path), ".cmd") {
		return nil
	}
	values := append([]string{command.Path}, command.Args[1:]...)
	for _, value := range values {
		if strings.ContainsAny(value, "\"\r\n\x00") || strings.HasSuffix(value, `\`) {
			return fmt.Errorf("Windows batch shell arguments cannot contain double quotes, line breaks, NUL, or a trailing backslash")
		}
	}
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	const prefix = "BUILDKITE_GHA_BATCH_ARG_"
	env := make([]string, 0, len(command.Env)+len(values))
	for _, entry := range command.Env {
		if !strings.HasPrefix(strings.ToUpper(entry), prefix) {
			env = append(env, entry)
		}
	}
	arguments := make([]string, len(values))
	for i, value := range values {
		// cmd treats an empty environment value as undefined and leaves its
		// percent reference literal. Empty argv needs no data interpolation.
		if value == "" {
			arguments[i] = `""`
			continue
		}
		name := fmt.Sprintf("%s%d", prefix, i)
		env = append(env, name+"="+value)
		arguments[i] = `"%` + name + `%"`
	}
	command.Path = filepath.Join(systemDirectory, "cmd.exe")
	command.Args = nil
	command.Env = env
	command.SysProcAttr = &windows.SysProcAttr{
		CmdLine: `cmd.exe /d /s /v:off /c "` + strings.Join(arguments, " ") + `"`,
	}
	return nil
}

func configureProcessGroup(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &windows.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED
}

func processStarted(command *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	err = windows.AssignProcessToJobObject(job, process)
	_ = windows.CloseHandle(process)
	if err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	windowsProcessJobs.Lock()
	windowsProcessJobs.jobs[command.Process.Pid] = job
	windowsProcessJobs.Unlock()
	// Assignment must precede execution so even immediately spawned children
	// inherit the job. Go closes the primary thread handle after CreateProcess.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err := windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(command.Process.Pid) {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		return err
	}
	return fmt.Errorf("find suspended process thread")
}

func processFinished(pid int) {
	windowsProcessJobs.Lock()
	defer windowsProcessJobs.Unlock()
	if job, ok := windowsProcessJobs.jobs[pid]; ok {
		delete(windowsProcessJobs.jobs, pid)
		_ = windows.CloseHandle(job)
	}
}

func terminateProcessGroup(ctx context.Context, pid int, _, _ time.Duration, finished <-chan struct{}) {
	select {
	case <-ctx.Done():
		terminateProcessGroupNow(pid, 0, 0)
	case <-finished:
	}
}

func terminateProcessGroupNow(pid int, _, _ time.Duration) {
	windowsProcessJobs.Lock()
	defer windowsProcessJobs.Unlock()
	if job, ok := windowsProcessJobs.jobs[pid]; ok {
		_ = windows.TerminateJobObject(job, 1)
	}
}
