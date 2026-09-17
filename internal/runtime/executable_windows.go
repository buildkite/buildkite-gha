package runtime

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func canonicalHostExecutable(path string) (string, error) {
	// Resolve the opened target through Windows itself. EvalSymlinks can fail
	// for executables inside mounted volumes even when Windows can open them.
	// Do not fall back to the unresolved path: workflow steps could retarget it.
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	buffer := make([]uint16, 260)
	for {
		n, err := windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if n >= uint32(len(buffer)) {
			buffer = make([]uint16, n+1)
			continue
		}
		resolved := windows.UTF16ToString(buffer[:n])
		// Git's shell-based credential helper needs ordinary DOS/UNC spelling,
		// rather than the extended namespace prefix returned by this API.
		if strings.HasPrefix(resolved, `\\?\UNC\`) {
			return `\\` + strings.TrimPrefix(resolved, `\\?\UNC\`), nil
		}
		return strings.TrimPrefix(resolved, `\\?\`), nil
	}
}
