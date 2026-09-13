//go:build !windows

package runtime

func isolateCacheToolEnvironment(env map[string]string) error {
	env["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	return nil
}
