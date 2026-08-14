package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const maxSecretBytes = 64 << 10

// SecretResolver resolves only names declared by the verified job plan.
type SecretResolver interface {
	ResolveSecret(context.Context, string) (string, error)
}

// Redactor registers a secret with the log sink before any step can run.
type Redactor interface {
	AddRedaction(context.Context, string) error
}

// AgentSecrets resolves plan-declared secrets with the destination job's
// authenticated Buildkite Agent session.
type AgentSecrets struct {
	Executable string
	Endpoint   string
	JobID      string
	JobToken   string
	NoHTTP2    string
}

func resolveAgentSecretsBeforeWorkflow(secrets SecretResolver) (SecretResolver, error) {
	resolve := func(secrets AgentSecrets) (AgentSecrets, error) {
		executable, err := resolveHostExecutableBeforeWorkflow(secrets.Executable, "buildkite-agent", "Buildkite Agent secret resolver")
		if err != nil {
			return AgentSecrets{}, err
		}
		secrets.Executable = executable
		return secrets, nil
	}
	switch secrets := secrets.(type) {
	case AgentSecrets:
		return resolve(secrets)
	case *AgentSecrets:
		if secrets == nil {
			return nil, fmt.Errorf("buildkite Agent secret resolver is nil")
		}
		resolved, err := resolve(*secrets)
		if err != nil {
			return nil, err
		}
		return &resolved, nil
	default:
		return secrets, nil
	}
}

func (r AgentSecrets) ResolveSecret(ctx context.Context, name string) (string, error) {
	executable := r.Executable
	if executable == "" {
		executable = "buildkite-agent"
	}
	command := exec.CommandContext(ctx, executable, "secret", "get", name)
	command.Env = []string{
		"BUILDKITE_JOB_ID=" + r.JobID,
		"BUILDKITE_AGENT_ACCESS_TOKEN=" + r.JobToken,
		"BUILDKITE_AGENT_JOB_API_SOCKET=" + os.Getenv("BUILDKITE_AGENT_JOB_API_SOCKET"),
		"BUILDKITE_AGENT_JOB_API_TOKEN=" + os.Getenv("BUILDKITE_AGENT_JOB_API_TOKEN"),
	}
	if r.Endpoint != "" {
		command.Env = append(command.Env, "BUILDKITE_AGENT_ENDPOINT="+r.Endpoint)
	}
	if r.NoHTTP2 != "" {
		command.Env = append(command.Env, "BUILDKITE_NO_HTTP2="+r.NoHTTP2)
	}
	var output boundedSecretBuffer
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", errors.New("buildkite Agent secret request failed")
	}
	if output.exceeded {
		return "", fmt.Errorf("buildkite Agent secret response exceeds %d bytes", maxSecretBytes)
	}
	return strings.TrimSuffix(output.String(), "\n"), nil
}

type boundedSecretBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *boundedSecretBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := maxSecretBytes + 1 - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	b.exceeded = b.Len() > maxSecretBytes
	return original, nil
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
