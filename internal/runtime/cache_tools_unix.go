//go:build !windows

package runtime

func isolateCacheToolEnvironment(env map[string]string) (func(), error) {
	env["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	return func() {}, nil
}
