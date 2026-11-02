package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// repairHostname restores Git's OS email fallback on native Namespace hosts.
// It never changes the kernel hostname or supplies a Git identity. Container
// jobs have their own hosts file and are deliberately outside this correction.
func (r *jobRun) repairHostname(ctx context.Context, processor *commandOutputProcessor) {
	if !r.RepairHostname || runtime.GOOS != "linux" || r.job.Container != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := repairCanonicalHostname(ctx); err != nil {
		processor.writeLiteral(processor.stderr, fmt.Sprintf("buildkite-gha: warning: cannot repair canonical hostname: %v; Git email inference may remain unavailable", err))
	}
}

func repairCanonicalHostname(ctx context.Context) error {
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	if qualifiedHostname(host) {
		return nil
	}
	// Use libc's resolver, as Git does, rather than Go's DNS resolver: release
	// binaries disable cgo, and Go's resolver does not implement every NSS source.
	fqdn := func() bool {
		name, err := exec.CommandContext(ctx, "/bin/hostname", "-f").Output()
		return err == nil && qualifiedHostname(strings.TrimSpace(string(name)))
	}
	if fqdn() {
		return nil
	}
	data, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return err
	}
	updated, err := qualifyHostMapping(data, host)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, updated) {
		// Hosts is commonly a bind mount, so rename is not available. Only insert
		// canonical names: the new content is longer and needs no truncation.
		file, err := os.OpenFile("/etc/hosts", os.O_WRONLY, 0)
		switch {
		case errors.Is(err, os.ErrPermission):
			// The Linux bootstrap grants runner passwordless sudo. Never prompt,
			// and never resolve these privileged tools through workflow PATH.
			cmd := exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "--", "/usr/bin/dd", "of=/etc/hosts", "conv=notrunc", "status=none")
			cmd.Stdin = bytes.NewReader(updated)
			if output, sudoErr := cmd.CombinedOutput(); sudoErr != nil {
				return fmt.Errorf("write /etc/hosts: %w (%s)", sudoErr, strings.TrimSpace(string(output)))
			}
		case err != nil:
			return err
		default:
			_, writeErr := file.Write(updated)
			if err := errors.Join(writeErr, file.Close()); err != nil {
				return err
			}
		}
	}
	if !fqdn() {
		return fmt.Errorf("system resolver still has no qualified self name")
	}
	return nil
}

func qualifiedHostname(name string) bool {
	return strings.Contains(strings.TrimSuffix(name, "."), ".")
}

func qualifyHostMapping(data []byte, host string) ([]byte, error) {
	// Namespace instance IDs are single DNS labels. Do not insert arbitrary
	// kernel hostname bytes into the system hosts file.
	if host == "" || len(host) > 63 || strings.Trim(host, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" || strings.HasPrefix(host, "-") || strings.HasSuffix(host, "-") {
		return nil, fmt.Errorf("hostname is not a DNS label")
	}
	lines := strings.SplitAfter(string(data), "\n")
	found := false
	for i, line := range lines {
		entry, _, _ := strings.Cut(line, "#")
		fields := strings.Fields(entry)
		if len(fields) < 2 || fields[1] == "localhost" {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		for _, alias := range fields[1:] {
			if !strings.EqualFold(alias, host) {
				continue
			}
			// Preserve custom qualified mappings even when NSS cannot use them.
			if qualifiedHostname(fields[1]) {
				return data, nil
			}
			start := strings.Index(entry, fields[0]) + len(fields[0])
			start += strings.Index(entry[start:], fields[1])
			lines[i] = line[:start] + host + ".buildkite.internal " + line[start:]
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("/etc/hosts has no usable self mapping")
	}
	return []byte(strings.Join(lines, "")), nil
}
