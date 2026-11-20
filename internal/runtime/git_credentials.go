package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

// GitCredentialHelperCommand is an internal Git credential-protocol entry point.
const GitCredentialHelperCommand = "git-credentials-helper"

const gitCredentialSocket = "git.sock"

func gitCredentialExecutable() string {
	if runtime.GOOS == "windows" {
		return "buildkite-gha-credentials.exe"
	}
	return "buildkite-gha-credentials"
}

// The socket is shared through the existing runner-temp container mount. The
// connection credential grants only this job's repository-token lookup, never
// the Agent API. GitHub tokens remain inside Git's credential protocol.
type gitCredentialService struct {
	server     *http.Server
	key        string
	host       string
	repository string
	token      *plan.GitHubToken
	runner     Runner
	processor  *commandOutputProcessor
	mu         sync.Mutex
	value      string
	issued     time.Time
	checkouts  []string
}

func startGitCredentialService(ctx context.Context, temp string, runner Runner, job plan.Job, processor *commandOutputProcessor) (*gitCredentialService, error) {
	_, host, _ := checkoutRepositoryURL(job.Event.Provider, job.Event.Repository, job.Event.ServerURL)
	s := &gitCredentialService{key: rand.Text(), host: host, repository: job.Event.Repository, token: job.GitHubToken, runner: runner, processor: processor}
	processor.addMask(s.key)
	executable := runner.RuntimeExecutable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	input, err := os.Open(executable)
	if err != nil {
		return nil, err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(filepath.Join(temp, gitCredentialExecutable()), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(output, input)
	if err := errors.Join(copyErr, output.Close()); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", filepath.Join(temp, gitCredentialSocket))
	if err != nil {
		return nil, fmt.Errorf("start Git credential service: %w", err)
	}
	// Job containers may execute as a different UID. Requests also require the
	// per-job connection credential stored in this checkout's Git config.
	if err := os.Chmod(filepath.Join(temp, gitCredentialSocket), 0o666); err != nil {
		_ = listener.Close()
		return nil, err
	}
	s.server = &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = s.server.Serve(listener) }()
	return s, nil
}

func (s *gitCredentialService) helper() string {
	return `!"$RUNNER_TEMP/` + gitCredentialExecutable() + `" ` + GitCredentialHelperCommand + " " + s.key
}

func (s *gitCredentialService) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost || request.URL.Path != "/credential" || subtle.ConstantTimeCompare([]byte(request.Header.Get("Authorization")), []byte("Bearer "+s.key)) != 1 {
		http.Error(w, "Git credential request denied", http.StatusForbidden)
		return
	}
	var fields map[string]string
	if err := json.NewDecoder(http.MaxBytesReader(w, request.Body, 64<<10)).Decode(&fields); err != nil {
		http.Error(w, "Invalid Git credential request", http.StatusBadRequest)
		return
	}
	if fields["protocol"] != "https" || !strings.EqualFold(fields["host"], s.host) || !strings.EqualFold(strings.TrimSuffix(fields["path"], ".git"), s.repository) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.token == nil || s.token.Permissions["contents"] != "read" && s.token.Permissions["contents"] != "write" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value == "" || time.Since(s.issued) >= 45*time.Minute {
		token, err := s.runner.resolveWorkflowToken(request.Context(), s.processor, s.repository, s.token.Workflow, s.token.Permissions)
		if err != nil {
			// Return the actionable server diagnostic, never provider response bodies.
			http.Error(w, s.processor.scrubError(err).Error(), http.StatusForbidden)
			return
		}
		s.value, s.issued = token, time.Now()
	}
	_, _ = fmt.Fprintf(w, "username=x-access-token\npassword=%s\n\n", s.value)
}

func (s *gitCredentialService) Close(ctx context.Context) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCleanupTimeout)
	defer cancel()
	var errs []error
	for _, directory := range s.checkouts {
		if _, err := os.Stat(filepath.Join(directory, ".git")); os.IsNotExist(err) {
			continue
		}
		for key, value := range map[string]string{
			"credential.https://" + s.host + ".helper":      s.helper(),
			"credential.https://" + s.host + ".useHttpPath": "true",
		} {
			cmd := exec.CommandContext(cleanup, s.runner.Git, "config", "--local", "--fixed-value", "--unset-all", key, value)
			cmd.Dir, cmd.Env = directory, processEnv(nil)
			if err := cmd.Run(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 5 {
					errs = append(errs, fmt.Errorf("remove checkout credential helper: %w", err))
				}
			}
		}
	}
	return errors.Join(append(errs, s.server.Close())...)
}

// RunGitCredentialHelper implements Git's get/store/erase protocol. It has no
// Agent credentials and cannot select the workflow permissions or repository.
func RunGitCredentialHelper(args []string, input io.Reader, output, stderr io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	if args[1] != "get" {
		return 0
	}
	fields := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(input, 64<<10))
	for scanner.Scan() {
		if scanner.Text() == "" {
			break
		}
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			fields[key] = value
		}
	}
	if scanner.Err() != nil {
		return 1
	}
	body, _ := json.Marshal(fields)
	ctx, cancel := context.WithTimeout(context.Background(), workflowTokenBudget+5*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(os.Getenv("RUNNER_TEMP"), gitCredentialSocket))
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/credential", strings.NewReader(string(body)))
	if err != nil {
		return 1
	}
	request.Header.Set("Authorization", "Bearer "+args[0])
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "Buildkite checkout credential helper is no longer available")
		return 1
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNoContent {
		return 0
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(stderr, io.LimitReader(response.Body, 64<<10))
		return 1
	}
	_, err = io.Copy(output, io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return 1
	}
	return 0
}

var checkoutAuthenticationKey = regexp.MustCompile(`^(credential(\..*)?\.(helper|username|usehttppath)|http(\..*)?\.(extraheader|sslcainfo|sslcapath|sslverify))$`)

// Snapshot administrator-configured authentication before workflow processes
// can edit HOME. Do not import URL rewrites, hooks, or arbitrary Git settings.
func snapshotCheckoutAuthentication(ctx context.Context, git, directory string) (string, error) {
	cmd := exec.CommandContext(ctx, git, "config", "--null", "--get-regexp", checkoutAuthenticationKey.String())
	env := map[string]string{}
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME"} {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}
	cmd.Dir, cmd.Env = directory, processEnv(env)
	data, err := cmd.Output()
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return "", fmt.Errorf("read configured Git authentication: %w", err)
	}
	var config strings.Builder
	quote := func(value string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "\b", `\b`).Replace(value) + `"`
	}
	for entry := range strings.SplitSeq(string(data), "\x00") {
		key, value, ok := strings.Cut(entry, "\n")
		if !ok {
			continue
		}
		// The inherited Agent helper needs broad job credentials. Replace it
		// with our narrow broker when provider credentials are enabled.
		if strings.HasSuffix(key, ".helper") && strings.Contains(value, "buildkite-agent") && strings.Contains(value, "git-credentials-helper") {
			continue
		}
		first, last := strings.IndexByte(key, '.'), strings.LastIndexByte(key, '.')
		section := key[:first]
		if first != last {
			section += " " + quote(key[first+1:last])
		}
		fmt.Fprintf(&config, "[%s]\n%s = %s\n", section, key[last+1:], quote(value))
	}
	path := filepath.Join(directory, "checkout-auth.config")
	return path, os.WriteFile(path, []byte(config.String()), 0o600)
}
