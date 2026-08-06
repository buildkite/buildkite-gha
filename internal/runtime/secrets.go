package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

func resolveAgentRedactorBeforeWorkflow(redactor Redactor) (Redactor, error) {
	resolve := func(redactor AgentRedactor) (AgentRedactor, error) {
		executable, err := resolveHostExecutableBeforeWorkflow(redactor.Executable, "buildkite-agent", "Buildkite Agent redactor")
		if err != nil {
			return AgentRedactor{}, err
		}
		redactor.Executable = executable
		return redactor, nil
	}
	switch redactor := redactor.(type) {
	case AgentRedactor:
		return resolve(redactor)
	case *AgentRedactor:
		if redactor == nil {
			return nil, fmt.Errorf("buildkite Agent redactor is nil")
		}
		resolved, err := resolve(*redactor)
		if err != nil {
			return nil, err
		}
		return &resolved, nil
	default:
		return redactor, nil
	}
}

func (r AgentRedactor) AddRedaction(ctx context.Context, value string) error {
	executable := r.Executable
	if executable == "" {
		executable = "buildkite-agent"
	}
	// redactor add uses only the Job API, but its embedded Agent API config
	// still requires an access-token value during CLI validation.
	command := exec.CommandContext(ctx, executable, "redactor", "add", "--agent-access-token=unused")
	command.Env = []string{
		"BUILDKITE_AGENT_JOB_API_SOCKET=" + os.Getenv("BUILDKITE_AGENT_JOB_API_SOCKET"),
		"BUILDKITE_AGENT_JOB_API_TOKEN=" + os.Getenv("BUILDKITE_AGENT_JOB_API_TOKEN"),
	}
	command.Stdin = strings.NewReader(value)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("register secret with Buildkite Agent redactor: %w: %s", err, output)
	}
	return nil
}
