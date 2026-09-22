package runtime

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func isolateCacheToolEnvironment(env map[string]string) error {
	// The audited bundles derive tar paths from PROGRAMFILES and SYSTEMDRIVE.
	// Read installation paths from Windows, never workflow or agent environment
	// variables. Do not admit workspace tools or package-manager shims via PATH.
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return fmt.Errorf("locate cache tools Program Files: %w", err)
	}
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return fmt.Errorf("locate cache tools System32: %w", err)
	}
	systemRoot := filepath.Dir(systemDirectory)
	env["PROGRAMFILES"] = programFiles
	env["SYSTEMDRIVE"] = filepath.VolumeName(systemDirectory)
	env["SYSTEMROOT"] = systemRoot
	env["WINDIR"] = systemRoot
	env["COMSPEC"] = filepath.Join(systemDirectory, "cmd.exe")
	env["PATHEXT"] = ".EXE"
	env["NODEFAULTCURRENTDIRECTORYINEXEPATH"] = "1"
	env["PATH"] = strings.Join([]string{
		filepath.Join(programFiles, "Git", "usr", "bin"),
		filepath.Join(programFiles, "zstd"),
		systemDirectory,
	}, ";")
	return nil
}
