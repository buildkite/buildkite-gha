package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var windowsProcessJobs = struct {
	sync.Mutex
	jobs map[int]windows.Handle
}{jobs: make(map[int]windows.Handle)}

func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED}
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
