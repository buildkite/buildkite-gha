package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// SecretResolver resolves only names declared by the verified job plan.
type SecretResolver interface {
	ResolveSecret(context.Context, string) (string, error)
}

// Redactor registers a secret with the log sink before any step can run.
type Redactor interface {
	AddRedaction(context.Context, string) error
}

// EnvironmentSecrets resolves plan-declared secrets from the job environment.
type EnvironmentSecrets struct{}

func (EnvironmentSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	value, ok := os.LookupEnv("BUILDKITE_GHA_SECRET_" + name)
	if !ok {
		return "", fmt.Errorf("secret %q is unavailable", name)
	}
	return value, nil
}

// AgentRedactor registers values with the Buildkite Agent redactor.
type AgentRedactor struct {
	Executable string
}

func (r AgentRedactor) AddRedaction(ctx context.Context, value string) error {
	executable := r.Executable
	if executable == "" {
		executable = "buildkite-agent"
	}
	command := exec.CommandContext(ctx, executable, "redactor", "add", value)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("register secret with Buildkite Agent redactor: %w: %s", err, output)
	}
	return nil
}
