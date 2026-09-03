package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/agentapi"
)

const (
	cacheCredentialResponseLimit = 64 << 10
	cacheActionToolPath          = "/usr/local/bin:/usr/bin:/bin"
	defaultCacheResultsURL       = "https://ghacs.buildkite.com/"
	cacheURLCompatibility        = "https://buildkite-gha-cache-gate.invalid/"

	// githubServerURLOverride is the synthetic, non-resolvable server URL
	// substituted in when shouldOverrideGitHubServerURL reports true, so
	// actions/cache's isGhes() check doesn't force the unsupported cache-v1
	// path. Only its ".localhost" suffix is load-bearing.
	githubServerURLOverride = "https://buildkite-gha.localhost"
)

var cacheRuntimeTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

// CacheCredentials are the short-lived values exposed only to one action
// lifecycle invocation.
type CacheCredentials struct {
	ResultsURL string
	Token      string
}

// CacheCredentialProvider mints one action-scoped cache credential at a time.
type CacheCredentialProvider interface {
	Credentials(context.Context) (CacheCredentials, error)
}

// AgentCacheConfig identifies the exact current Buildkite job and cache-v2
// service. Endpoint and JobToken are runtime connection and authentication
// material; this provider does not add them to action subprocess environments.
type AgentCacheConfig struct {
	Endpoint      string
	JobID         string
	JobToken      string
	ResultsURL    string
	ClientVersion string
	Client        *http.Client
}

// AgentCacheCredentials mints GHAC tokens through Buildkite's job-bound Agent
// API endpoint.
type AgentCacheCredentials struct {
	mintURL    string
	resultsURL string
	agent      *agentapi.Client
}

func NewAgentCacheCredentials(config AgentCacheConfig) (*AgentCacheCredentials, error) {
	agent, err := agentapi.New(agentapi.Config{
		Endpoint: config.Endpoint, JobID: config.JobID, JobToken: config.JobToken,
		ClientVersion: config.ClientVersion, HTTPClient: config.Client,
	}, "cache")
	if err != nil {
		return nil, err
	}
	if config.ResultsURL == "" {
		config.ResultsURL = defaultCacheResultsURL
	}
	resultsURL, err := normalizeCacheResultsURL(config.ResultsURL)
	if err != nil {
		return nil, err
	}
	return &AgentCacheCredentials{
		mintURL: agent.URL("ghac_tokens"), resultsURL: resultsURL, agent: agent,
	}, nil
}

func (c *AgentCacheCredentials) Credentials(ctx context.Context) (CacheCredentials, error) {
	if c == nil {
		return CacheCredentials{}, fmt.Errorf("cache credentials are not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.mintURL, http.NoBody)
	if err != nil {
		return CacheCredentials{}, fmt.Errorf("create cache credential request: %w", err)
	}
	response, err := c.agent.Do(request)
	if err != nil {
		return CacheCredentials{}, fmt.Errorf("request cache credential: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, cacheCredentialResponseLimit))
		return CacheCredentials{}, cacheCredentialStatusError(response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, cacheCredentialResponseLimit+1))
	if err != nil {
		return CacheCredentials{}, fmt.Errorf("read cache credential response: %w", err)
	}
	if len(payload) > cacheCredentialResponseLimit {
		return CacheCredentials{}, fmt.Errorf("cache credential response exceeds the %d-byte limit", cacheCredentialResponseLimit)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var body struct {
		Token string `json:"token"`
	}
	if err := decoder.Decode(&body); err != nil {
		return CacheCredentials{}, fmt.Errorf("decode cache credential response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CacheCredentials{}, fmt.Errorf("cache credential response has trailing data")
	}
	if len(body.Token) > 16<<10 || !cacheRuntimeTokenPattern.MatchString(body.Token) {
		return CacheCredentials{}, fmt.Errorf("cache credential response contains an invalid token")
	}
	return CacheCredentials{ResultsURL: c.resultsURL, Token: body.Token}, nil
}

func normalizeCacheResultsURL(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || !validCredentialServiceURL(u) || u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("safe cache Results service URL using HTTPS or loopback HTTP is required")
	}
	u.Path = "/"
	u.RawPath = ""
	return u.String(), nil
}

func validCredentialServiceURL(u *url.URL) bool {
	if u == nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

func cacheCredentialStatusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("cache credential request was denied")
	case http.StatusNotFound:
		return fmt.Errorf("cache credential service is not enabled for this organization")
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("cache credential request rejected the current build provenance")
	case http.StatusTooManyRequests:
		return fmt.Errorf("cache credential service is rate limited")
	default:
		return fmt.Errorf("cache credential service returned HTTP %d", status)
	}
}

func isCacheServiceEnvironment(name string) bool {
	switch name {
	case "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN", "ACTIONS_CACHE_SERVICE_V2", "ACTIONS_CACHE_URL", "ACTIONS_RUNTIME_URL":
		return true
	default:
		return false
	}
}

func removeCacheServiceEnvironment(env map[string]string) map[string]string {
	clean := cloneStrings(env)
	for name := range clean {
		if isCacheServiceEnvironment(name) {
			delete(clean, name)
		}
	}
	return clean
}

// shouldOverrideGitHubServerURL reports whether serverURL needs to be
// replaced with githubServerURLOverride before starting an audited action
// with a bundled cache client. Its isGhes() forces the unsupported cache-v1
// path for every host outside its own allowlist (github.com, *.ghe.com,
// *.localhost). Malformed/empty values are left untouched.
func shouldOverrideGitHubServerURL(serverURL string) bool {
	u, err := url.Parse(serverURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToUpper(strings.TrimRight(u.Hostname(), " "))
	isGitHub := host == "GITHUB.COM"
	isGheCloud := strings.HasSuffix(host, ".GHE.COM")
	isLocal := strings.HasSuffix(host, ".LOCALHOST")
	return !isGitHub && !isGheCloud && !isLocal
}

func isolateCacheActionEnvironment(env map[string]string) map[string]string {
	isolated := removeCacheServiceEnvironment(env)
	for _, name := range []string{
		"NODE_OPTIONS", "NODE_PATH", "NODE_EXTRA_CA_CERTS", "NODE_TLS_REJECT_UNAUTHORIZED", "SSLKEYLOGFILE", "LD_AUDIT", "LD_PRELOAD", "LD_LIBRARY_PATH",
		"OPENSSL_CONF", "OPENSSL_CONF_INCLUDE", "OPENSSL_ENGINES", "OPENSSL_MODULES",
		"TAR_OPTIONS",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy",
		"BUILDKITE_AGENT_ACCESS_TOKEN", "BUILDKITE_JOB_ID",
	} {
		delete(isolated, name)
	}
	isolated["PATH"] = cacheActionToolPath
	return isolated
}

func applyGitHubServerURLOverride(env map[string]string) {
	if shouldOverrideGitHubServerURL(env["GITHUB_SERVER_URL"]) {
		env["GITHUB_SERVER_URL"] = githubServerURLOverride
	}
}

func (r Runner) cacheActionEnvironment(ctx context.Context, processor *commandOutputProcessor) (map[string]string, error) {
	if r.Cache == nil {
		return nil, fmt.Errorf("cache credential provider is not configured")
	}
	credentials, err := r.Cache.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	resultsURL, err := normalizeCacheResultsURL(credentials.ResultsURL)
	if err != nil {
		return nil, err
	}
	if len(credentials.Token) > 16<<10 || !cacheRuntimeTokenPattern.MatchString(credentials.Token) {
		return nil, fmt.Errorf("cache credential provider returned an invalid token")
	}
	if r.Redactor == nil {
		return nil, fmt.Errorf("cache credential provider requires a redactor")
	}
	processor.addMask(credentials.Token)
	if err := r.Redactor.AddRedaction(ctx, credentials.Token); err != nil {
		return nil, processor.scrubError(err)
	}
	return map[string]string{
		"ACTIONS_CACHE_SERVICE_V2": "true",
		"ACTIONS_RESULTS_URL":      resultsURL,
		"ACTIONS_RUNTIME_TOKEN":    credentials.Token,
	}, nil
}
