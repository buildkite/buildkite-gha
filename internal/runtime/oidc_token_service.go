package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/agentapi"
)

const oidcTokenResponseLimit = 64 << 10

var oidcTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

// OIDCTokenProvider mints a Buildkite OIDC token for an action-requested audience.
type OIDCTokenProvider interface {
	OIDCToken(context.Context, string) (string, error)
}

// AgentOIDCTokenConfig identifies the current Buildkite job's Agent API.
type AgentOIDCTokenConfig struct {
	Endpoint       string
	JobID          string
	JobToken       string
	Claims         []string
	AWSSessionTags []string
	SubjectClaim   string
	ClientVersion  string
	Client         *http.Client
}

// AgentOIDCTokens mints job-bound Buildkite OIDC tokens through the Agent API.
type AgentOIDCTokens struct {
	mintURL string
	claims  []string
	awsTags []string
	subject string
	agent   *agentapi.Client
}

func NewAgentOIDCTokens(config AgentOIDCTokenConfig) (*AgentOIDCTokens, error) {
	agent, err := agentapi.New(agentapi.Config{
		Endpoint: config.Endpoint, JobID: config.JobID, JobToken: config.JobToken,
		ClientVersion: config.ClientVersion, HTTPClient: config.Client,
	}, "OIDC token")
	if err != nil {
		return nil, err
	}
	return &AgentOIDCTokens{
		mintURL: agent.URL("oidc/tokens"),
		claims:  append([]string(nil), config.Claims...),
		awsTags: append([]string(nil), config.AWSSessionTags...),
		subject: config.SubjectClaim,
		agent:   agent,
	}, nil
}

func (c *AgentOIDCTokens) OIDCToken(ctx context.Context, audience string) (token string, err error) {
	defer func() { err = markJobSetupFailure(FailureClassOIDCToken, err) }()
	if c == nil {
		return "", fmt.Errorf("OIDC token provider is not configured")
	}
	if len(audience) > 4096 || !utf8.ValidString(audience) {
		return "", fmt.Errorf("OIDC token audience is invalid")
	}
	body, err := json.Marshal(struct {
		Audience       string   `json:"audience"`
		Claims         []string `json:"claims,omitempty"`
		AWSSessionTags []string `json:"aws_session_tags,omitempty"`
		SubjectClaim   string   `json:"subject_claim,omitempty"`
	}{Audience: audience, Claims: c.claims, AWSSessionTags: c.awsTags, SubjectClaim: c.subject})
	if err != nil {
		return "", fmt.Errorf("encode OIDC token request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.mintURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create OIDC token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.agent.Do(request)
	if err != nil {
		return "", fmt.Errorf("request OIDC token: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, oidcTokenResponseLimit))
		return "", oidcTokenStatusError(response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, oidcTokenResponseLimit+1))
	if err != nil {
		return "", fmt.Errorf("read OIDC token response: %w", err)
	}
	if len(payload) > oidcTokenResponseLimit {
		return "", fmt.Errorf("OIDC token response exceeds the %d-byte limit", oidcTokenResponseLimit)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var decoded struct {
		Token string `json:"token"`
	}
	if err := decoder.Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode OIDC token response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("OIDC token response has trailing data")
	}
	if len(decoded.Token) > 16<<10 || !oidcTokenPattern.MatchString(decoded.Token) {
		return "", fmt.Errorf("OIDC token response contains an invalid token")
	}
	return decoded.Token, nil
}

func oidcTokenStatusError(status int) error {
	message := ""
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		message = "OIDC token request was rejected"
	case http.StatusUnauthorized, http.StatusForbidden:
		message = "OIDC token request was denied"
	case http.StatusNotFound:
		message = "OIDC token service is not enabled for this organization"
	case http.StatusTooManyRequests:
		message = "OIDC token service is rate limited"
	default:
		message = fmt.Sprintf("OIDC token service returned HTTP %d", status)
	}
	return newJobSetupHTTPFailure(FailureClassOIDCToken, status, message)
}

func isIDTokenEnvironment(name string) bool {
	return name == "ACTIONS_ID_TOKEN_REQUEST_URL" || name == "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
}

func removeIDTokenEnvironment(env map[string]string) map[string]string {
	clean := cloneStrings(env)
	delete(clean, "ACTIONS_ID_TOKEN_REQUEST_URL")
	delete(clean, "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	return clean
}

type idTokenService struct {
	server     *http.Server
	listener   net.Listener
	provider   OIDCTokenProvider
	redactor   Redactor
	processor  *commandProcessor
	mu         sync.RWMutex
	authHashes map[[sha256.Size]byte]*idTokenInvocation
}

type idTokenInvocation struct {
	mu      sync.Mutex
	failure error
}

func (i *idTokenInvocation) recordFailure(err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.failure == nil {
		i.failure = err
	}
}

func (i *idTokenInvocation) failureError() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.failure
}

func startIDTokenService(ctx context.Context, provider OIDCTokenProvider, redactor Redactor, processor *commandProcessor) (*idTokenService, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start actions ID-token service: %w", err)
	}
	service := &idTokenService{listener: listener, provider: provider, redactor: redactor, processor: processor, authHashes: map[[sha256.Size]byte]*idTokenInvocation{}}
	service.server = &http.Server{
		Handler:           service,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() { _ = service.server.Serve(listener) }()
	return service, nil
}

func (s *idTokenService) Close(parent context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

func (s *idTokenService) actionEnvironment(ctx context.Context, baseEnv map[string]string) (map[string]string, func() error, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, nil, fmt.Errorf("create actions ID-token request credential: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256([]byte(token))
	s.processor.addMask(token)
	if err := s.redactor.AddRedaction(ctx, token); err != nil {
		return nil, nil, s.processor.scrubError(err)
	}
	s.mu.Lock()
	invocation := &idTokenInvocation{}
	s.authHashes[hash] = invocation
	s.mu.Unlock()
	revoke := func() error {
		s.mu.Lock()
		delete(s.authHashes, hash)
		s.mu.Unlock()
		return invocation.failureError()
	}
	env := map[string]string{
		"ACTIONS_ID_TOKEN_REQUEST_URL":   "http://" + s.listener.Addr().String() + "/idtoken?api-version=2",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": token,
	}
	host, _, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		_ = revoke()
		return nil, nil, fmt.Errorf("parse actions ID-token listener: %w", err)
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		env[name] = host
		if baseEnv[name] != "" {
			env[name] = baseEnv[name] + "," + host
		}
	}
	return env, revoke, nil
}

func (s *idTokenService) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(w, "actions ID-token endpoint requires GET", http.StatusMethodNotAllowed)
		return
	}
	presented := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	presentedHash := sha256.Sum256([]byte(presented))
	s.mu.RLock()
	matched := 0
	var invocation *idTokenInvocation
	for authHash, candidate := range s.authHashes {
		match := subtle.ConstantTimeCompare(presentedHash[:], authHash[:])
		matched |= match
		if match == 1 {
			invocation = candidate
		}
	}
	s.mu.RUnlock()
	if presented == "" || matched != 1 {
		http.Error(w, "actions ID-token request is unauthorized", http.StatusUnauthorized)
		return
	}
	token, err := s.provider.OIDCToken(request.Context(), request.URL.Query().Get("audience"))
	if err != nil {
		err = markJobSetupFailure(FailureClassOIDCToken, err)
		status := http.StatusBadGateway
		if upstreamStatus, ok := AgentAPIHTTPStatus(err); ok {
			switch upstreamStatus {
			case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
				status = upstreamStatus
			}
		}
		invocation.recordFailure(err)
		http.Error(w, "could not mint actions ID token", status)
		return
	}
	s.processor.addMask(token)
	if err := s.redactor.AddRedaction(request.Context(), token); err != nil {
		http.Error(w, "could not protect actions ID token", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Value string `json:"value"`
	}{Value: token})
}
