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
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	Client         *http.Client
}

// AgentOIDCTokens mints job-bound Buildkite OIDC tokens through the Agent API.
type AgentOIDCTokens struct {
	mintURL  string
	jobToken string
	claims   []string
	awsTags  []string
	subject  string
	client   *http.Client
}

func NewAgentOIDCTokens(config AgentOIDCTokenConfig) (*AgentOIDCTokens, error) {
	mintURL, err := agentOIDCTokenURL(config.Endpoint, config.JobID)
	if err != nil {
		return nil, err
	}
	if config.JobToken == "" || strings.ContainsAny(config.JobToken, "\r\n") {
		return nil, fmt.Errorf("OIDC token Agent job token is required")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	bounded := *client
	bounded.Jar = nil
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout == 0 {
		bounded.Timeout = 15 * time.Second
	}
	return &AgentOIDCTokens{
		mintURL:  mintURL,
		jobToken: config.JobToken,
		claims:   append([]string(nil), config.Claims...),
		awsTags:  append([]string(nil), config.AWSSessionTags...),
		subject:  config.SubjectClaim,
		client:   &bounded,
	}, nil
}

func (c *AgentOIDCTokens) OIDCToken(ctx context.Context, audience string) (string, error) {
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
	request.Header.Set("Authorization", "Token "+c.jobToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
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

func agentOIDCTokenURL(endpoint, jobID string) (string, error) {
	if !validBuildkiteJobID(jobID) {
		return "", fmt.Errorf("OIDC token Agent job ID is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || !validCredentialServiceURL(u) {
		return "", fmt.Errorf("safe OIDC token Agent endpoint using HTTPS or loopback HTTP is required")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/jobs/" + jobID + "/oidc/tokens"
	u.RawPath = ""
	return u.String(), nil
}

type oidcTokenHTTPError struct {
	status  int
	message string
}

func (e *oidcTokenHTTPError) Error() string { return e.message }

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
	return &oidcTokenHTTPError{status: status, message: message}
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
	authHashes map[[sha256.Size]byte]struct{}
}

func startIDTokenService(ctx context.Context, provider OIDCTokenProvider, redactor Redactor, processor *commandProcessor) (*idTokenService, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start actions ID-token service: %w", err)
	}
	service := &idTokenService{listener: listener, provider: provider, redactor: redactor, processor: processor, authHashes: map[[sha256.Size]byte]struct{}{}}
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

func (s *idTokenService) actionEnvironment(ctx context.Context, baseEnv map[string]string) (map[string]string, func(), error) {
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
	s.authHashes[hash] = struct{}{}
	s.mu.Unlock()
	revoke := func() {
		s.mu.Lock()
		delete(s.authHashes, hash)
		s.mu.Unlock()
	}
	env := map[string]string{
		"ACTIONS_ID_TOKEN_REQUEST_URL":   "http://" + s.listener.Addr().String() + "/idtoken?api-version=2",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": token,
	}
	host, _, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		revoke()
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
	for authHash := range s.authHashes {
		matched |= subtle.ConstantTimeCompare(presentedHash[:], authHash[:])
	}
	s.mu.RUnlock()
	if presented == "" || matched != 1 {
		http.Error(w, "actions ID-token request is unauthorized", http.StatusUnauthorized)
		return
	}
	token, err := s.provider.OIDCToken(request.Context(), request.URL.Query().Get("audience"))
	if err != nil {
		status := http.StatusBadGateway
		var statusErr *oidcTokenHTTPError
		if errors.As(err, &statusErr) {
			switch statusErr.status {
			case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
				status = statusErr.status
			}
		}
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
