//go:build linux

package runtime

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func waitForProcessStopped(pid int) {
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return
		}
		closeParen := strings.LastIndexByte(string(stat), ')')
		if closeParen >= 0 {
			fields := strings.Fields(string(stat[closeParen+1:]))
			if len(fields) != 0 && (fields[0] == "T" || fields[0] == "t") {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
}

func signalProcessGroupChildren(group int, signal syscall.Signal) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == group {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		closeParen := strings.LastIndexByte(string(stat), ')')
		if closeParen < 0 {
			continue
		}
		fields := strings.Fields(string(stat[closeParen+1:]))
		if len(fields) < 3 {
			continue
		}
		processGroup, err := strconv.Atoi(fields[2])
		if err == nil && processGroup == group {
			signalProcessGroupMember(pid, group, signal)
		}
	}
}
