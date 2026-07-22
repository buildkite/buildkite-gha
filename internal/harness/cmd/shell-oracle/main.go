package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/buildkite/buildkite-gha/internal/harness"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shell-oracle <materialize|compare> [flags]")
	}
	flags := flag.NewFlagSet("shell-oracle "+args[0], flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	source := flags.String("source", "testdata/smoke", "smoke fixture directory")

	switch args[0] {
	case "materialize":
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("materialize accepts no positional arguments")
		}
		repository, err := harness.MaterializeShellFixture(ctx, *source)
		if err != nil {
			return err
		}
		fmt.Println(repository.Commit)
		return repository.Close()
	case "compare":
		commit := flags.String("commit", "", "exact materialized fixture commit")
		provider := flags.String("provider", "", "provider that produced standard input")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("compare accepts observations on standard input only")
		}
		normalized, err := harness.CompareShellOracle(ctx, *source, *commit, *provider, os.Stdin)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(append(normalized, '\n'))
		return err
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
