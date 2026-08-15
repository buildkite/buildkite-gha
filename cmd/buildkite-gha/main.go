package main

import (
	"os"
	"runtime/debug"

	"github.com/buildkite/buildkite-gha/internal/cli"
)

var version = "dev"
var revision string

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, resolvedVersion()))
}

func resolvedVersion() string {
	if version != "dev" {
		return version
	}
	if revision == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					revision = setting.Value
					break
				}
			}
		}
	}
	return developmentVersion(version, revision)
}

func developmentVersion(base, commit string) string {
	if base != "dev" || len(commit) < 12 {
		return base
	}
	for _, character := range commit {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return base
		}
	}
	return base + "+" + commit[:12]
}
