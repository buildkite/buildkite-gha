package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/buildkite/buildkite-gha/internal/agentapi"
)

const (
	// variableResolutionResponseLimit bounds a successful response. The
	// backend caps repository and organization names and values at 256 KiB
	// combined, and JSON escaping expands a byte to at most six, so 2 MiB
	// holds any response it can send.
	variableResolutionResponseLimit = 2 << 20

	// repositoryVariableCountLimit and organizationVariableCountLimit are
	// GitHub's maximum numbers of variables per scope.
	repositoryVariableCountLimit   = 500
	organizationVariableCountLimit = 1000
	// variablesByteLimit is the backend's bound on the UTF-8 bytes of
	// repository and organization variable names and values combined.
	variablesByteLimit = 256 << 10
)

// ErrVariablesUnavailable reports that the Agent API offers no repository and
// organization variable resolution: the backend lacks the endpoint or the
// organization opted out. Callers keep the vars context empty for names no
// scope defines, as they did before the endpoint existed.
var ErrVariablesUnavailable = errors.New("the Agent API does not offer GitHub variable resolution")

// VariablesSnapshot is the Buildkite backend's snapshot of a repository's
// GitHub Actions variables outside any environment, by scope. Values are
// plaintext configuration: callers must keep them out of logs and
// diagnostics. Names are unique case-insensitively within each scope.
type VariablesSnapshot struct {
	Repository   map[string]string
	Organization map[string]string
}

// AgentVariableResolverConfig carries the current Buildkite job's Agent
// connection and authentication material.
type AgentVariableResolverConfig struct {
	Endpoint      string
	JobID         string
	JobToken      string
	ClientVersion string
	Client        *http.Client
}

// AgentVariableResolver resolves repository and organization variables
// through the job-scoped Agent API endpoint github-actions/variables. The
// backend performs the GitHub reads with its own credentials, restricted to
// the pipeline's configured GitHub.com repository.
type AgentVariableResolver struct {
	resolveURL string
	agent      *agentapi.Client
}

// NewAgentVariableResolver validates the Agent connection configuration.
func NewAgentVariableResolver(config AgentVariableResolverConfig) (*AgentVariableResolver, error) {
	client := config.Client
	if client == nil {
		// The backend walks paginated GitHub listings for both scopes, so
		// allow longer than the default Agent API request timeout.
		client = &http.Client{Timeout: 30 * time.Second}
	}
	agent, err := agentapi.New(agentapi.Config{
		Endpoint: config.Endpoint, JobID: config.JobID, JobToken: config.JobToken,
		ClientVersion: config.ClientVersion, HTTPClient: client,
	}, "variable resolution")
	if err != nil {
		return nil, err
	}
	return &AgentVariableResolver{resolveURL: agent.URL("github-actions/variables"), agent: agent}, nil
}

// ResolveVariables resolves the repository and organization variables of the
// given owner/repository in one request. It returns ErrVariablesUnavailable
// when the Agent API answers 404; every other failure carries the backend's
// status, and its message or Retry-After delay when present. Error messages
// name at most a variable, never a value.
func (c *AgentVariableResolver) ResolveVariables(ctx context.Context, repository string) (VariablesSnapshot, error) {
	if c == nil {
		return VariablesSnapshot{}, fmt.Errorf("variable resolver is not configured")
	}
	if !validCheckoutRepository(repository) {
		return VariablesSnapshot{}, fmt.Errorf("variable resolution requires a valid event repository")
	}
	body, err := json.Marshal(struct {
		RepositoryURL string `json:"repo_url"`
	}{RepositoryURL: "https://github.com/" + repository})
	if err != nil {
		return VariablesSnapshot{}, fmt.Errorf("encode variable resolution request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolveURL, bytes.NewReader(body))
	if err != nil {
		return VariablesSnapshot{}, fmt.Errorf("create variable resolution request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.agent.Do(request)
	if err != nil {
		return VariablesSnapshot{}, fmt.Errorf("request variable resolution: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		errorBody, _ := io.ReadAll(io.LimitReader(response.Body, environmentResolutionErrorLimit))
		return VariablesSnapshot{}, variableResolutionStatusError(response.StatusCode, response.Header.Get("Retry-After"), errorBody)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, variableResolutionResponseLimit+1))
	if err != nil {
		return VariablesSnapshot{}, fmt.Errorf("read variable resolution response: %w", err)
	}
	if len(payload) > variableResolutionResponseLimit {
		return VariablesSnapshot{}, fmt.Errorf("variable resolution response exceeds the %d-byte limit", variableResolutionResponseLimit)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	// Both scopes decode through pointers: the backend always sends both, so
	// an absent scope is a contract violation, not an empty scope.
	var decoded struct {
		Repository   *[]resolvedVariable `json:"repository_variables"`
		Organization *[]resolvedVariable `json:"organization_variables"`
	}
	if err := decoder.Decode(&decoded); err != nil {
		return VariablesSnapshot{}, fmt.Errorf("decode variable resolution response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return VariablesSnapshot{}, fmt.Errorf("variable resolution response has trailing data")
	}
	if decoded.Repository == nil || decoded.Organization == nil {
		return VariablesSnapshot{}, fmt.Errorf("variable resolution response omits repository_variables or organization_variables")
	}
	total := 0
	repositoryVariables, err := resolvedVariablesMap("repository", *decoded.Repository, repositoryVariableCountLimit, &total)
	if err != nil {
		return VariablesSnapshot{}, fmt.Errorf("invalid variable resolution response: %w", err)
	}
	organizationVariables, err := resolvedVariablesMap("organization", *decoded.Organization, organizationVariableCountLimit, &total)
	if err != nil {
		return VariablesSnapshot{}, fmt.Errorf("invalid variable resolution response: %w", err)
	}
	if total > variablesByteLimit {
		return VariablesSnapshot{}, fmt.Errorf("repository and organization variables exceed %d bytes; remove or shrink repository or organization variables", variablesByteLimit)
	}
	return VariablesSnapshot{Repository: repositoryVariables, Organization: organizationVariables}, nil
}

type resolvedVariable struct {
	Name  *string `json:"name"`
	Value *string `json:"value"`
}

// resolvedVariablesMap validates one scope's variables against GitHub's
// bounds, adds their bytes to total, and returns them by name. Errors name
// the scope and, at most, a variable name; the caller wraps them with the
// response context.
func resolvedVariablesMap(scope string, variables []resolvedVariable, countLimit int, total *int) (map[string]string, error) {
	if len(variables) > countLimit {
		return nil, fmt.Errorf("%d %s variables exceed GitHub's limit of %d", len(variables), scope, countLimit)
	}
	values := make(map[string]string, len(variables))
	identities := make(map[string]string, len(variables))
	for _, variable := range variables {
		if variable.Name == nil || variable.Value == nil {
			return nil, fmt.Errorf("a variable in the %s scope has no name or value", scope)
		}
		if !environmentSecretNamePattern.MatchString(*variable.Name) {
			return nil, fmt.Errorf("invalid %s variable name", scope)
		}
		identity := strings.ToUpper(*variable.Name)
		if previous, exists := identities[identity]; exists {
			return nil, fmt.Errorf("%s variable %q repeats %q; GitHub variable names are case-insensitive", scope, *variable.Name, previous)
		}
		identities[identity] = *variable.Name
		if len(*variable.Value) > environmentVariableValueLimit {
			return nil, fmt.Errorf("%s variable %q exceeds GitHub's %d-byte value limit", scope, *variable.Name, environmentVariableValueLimit)
		}
		*total += len(*variable.Name) + len(*variable.Value)
		values[*variable.Name] = *variable.Value
	}
	return values, nil
}

func variableResolutionStatusError(status int, retryAfter string, body []byte) error {
	switch status {
	case http.StatusBadRequest:
		if message := errorBodyMessage(body); message != "" {
			return fmt.Errorf("the variable resolution request was rejected: %s", message)
		}
		return fmt.Errorf("the variable resolution request was rejected; confirm that Buildkite's GitHub App can read the repository's variables")
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("the variable resolution request was denied")
	case http.StatusNotFound:
		return ErrVariablesUnavailable
	case http.StatusTooManyRequests:
		retryAfter = strings.TrimSpace(retryAfter)
		if retryAfterSecondsPattern.MatchString(retryAfter) {
			return fmt.Errorf("variable resolution requests are rate limited; retry after %s seconds", retryAfter)
		}
		return fmt.Errorf("variable resolution requests are rate limited")
	case http.StatusServiceUnavailable:
		retryAfter = strings.TrimSpace(retryAfter)
		if retryAfterSecondsPattern.MatchString(retryAfter) {
			return fmt.Errorf("the variable resolution service is temporarily unavailable; retry after %s seconds", retryAfter)
		}
		return fmt.Errorf("the variable resolution service is temporarily unavailable")
	default:
		return fmt.Errorf("the variable resolution service returned HTTP %d", status)
	}
}
