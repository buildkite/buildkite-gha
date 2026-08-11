package main

import (
	"os"

	"github.com/buildkite/buildkite-gha/internal/launcher"
)

func main() { os.Exit(launcher.Run(os.Args[1:])) }
