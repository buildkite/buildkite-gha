package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	ghacache "github.com/buildkite/buildkite-gha/internal/cache"
)

type jobCacheService struct {
	handler          *ghacache.Handler
	server           *http.Server
	done             chan error
	actionEnv        map[string]string
	containerEnv     map[string]string
	containerRouting bool
}

func (s *jobCacheService) Env(container bool) map[string]string {
	if s == nil {
		return nil
	}
	if container {
		return s.containerEnv
	}
	return s.actionEnv
}

func (s *jobCacheService) DockerArgs() []string {
	if s == nil || !s.containerRouting {
		return nil
	}
	return []string{"--add-host", cacheContainerHost}
}

func (r *Runner) startCacheService(ctx context.Context, processor *commandProcessor, tempDir string, containerRouting bool) (*jobCacheService, error) {
	if r.Cache == nil || r.Cache.Backend == nil {
		return nil, errors.New("cache backend is not configured")
	}
	if r.Redactor == nil {
		return nil, errors.New("cache service requires a redactor")
	}
	token, err := randomCacheValue(32)
	if err != nil {
		return nil, fmt.Errorf("generate cache token: %w", err)
	}
	if err := r.Redactor.AddRedaction(ctx, token); err != nil {
		return nil, fmt.Errorf("register cache token redaction: %w", err)
	}
	processor.addMask(token)
	session, err := randomCacheValue(16)
	if err != nil {
		return nil, fmt.Errorf("generate cache session: %w", err)
	}
	listenAddress := "127.0.0.1:0"
	if containerRouting {
		listenAddress = "0.0.0.0:0"
	}
	listener, err := net.Listen("tcp4", listenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen for cache service: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	baseURL := fmt.Sprintf("http://127.0.0.1:%d/", port)
	containerBaseURL := ""
	if containerRouting {
		containerBaseURL = fmt.Sprintf("http://%s:%d/", cacheContainerHostname, port)
	}
	registerRedaction := func(redactionCtx context.Context, value string) error {
		if err := r.Redactor.AddRedaction(redactionCtx, value); err != nil {
			return fmt.Errorf("register cache URL redaction: %w", err)
		}
		processor.addMask(value)
		return nil
	}
	handler, err := ghacache.NewHandler(r.Cache.Backend, ghacache.Config{
		Token: token, Session: session, BaseURL: baseURL, ContainerBaseURL: containerBaseURL, TempDir: tempDir,
		ReadOnly:   r.Cache.ReadOnly,
		MaxArchive: r.Cache.MaxArchive, MaxCandidates: r.Cache.MaxCandidates,
		MaxKey: r.Cache.MaxKey, MaxVersion: r.Cache.MaxVersion,
		RegisterRedaction: registerRedaction,
	})
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("configure cache service: %w", err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	service := &jobCacheService{
		handler: handler, server: server, done: done,
		actionEnv: map[string]string{
			"ACTIONS_CACHE_URL":     baseURL,
			"ACTIONS_RUNTIME_TOKEN": token,
		},
		containerRouting: containerRouting,
	}
	if containerRouting {
		service.containerEnv = map[string]string{
			"ACTIONS_CACHE_URL":     containerBaseURL,
			"ACTIONS_RUNTIME_TOKEN": token,
		}
	}
	return service, nil
}

func (s *jobCacheService) close(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := s.server.Shutdown(ctx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, s.server.Close())
	}
	serveErr := <-s.done
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(shutdownErr, serveErr, s.handler.Close())
}

func randomCacheValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (r Runner) applyActionCacheEnvironment(env map[string]string, container bool) {
	if r.cacheService == nil {
		return
	}
	delete(env, "ACTIONS_CACHE_SERVICE_V2")
	delete(env, "ACTIONS_RESULTS_URL")
	mergeInto(env, r.cacheService.Env(container))
}

func (r Runner) postActionTimeout() time.Duration {
	if r.PostActionTimeout > 0 {
		return r.PostActionTimeout
	}
	return defaultPostActionTimeout
}

// postPhaseContext returns a context for post-actions with an independent
// timeout (default 10 minutes). If parent is cancelled, post-actions still run
// until cancelGrace elapses (default cleanupTimeout / 10s), then the returned
// context is cancelled. This deliberately separates the post-action budget from
// the short resource-cleanup deadline so cache saves in post can finish.
func postPhaseContext(parent context.Context, timeout, cancelGrace time.Duration) (context.Context, context.CancelFunc) {
	postCtx, cancelPosts := context.WithTimeout(context.Background(), timeout)
	postDone := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-parent.Done():
			timer := time.NewTimer(cancelGrace)
			defer timer.Stop()
			select {
			case <-timer.C:
				cancelPosts()
			case <-postDone:
			}
		case <-postDone:
		}
	}()
	return postCtx, func() {
		once.Do(func() { close(postDone) })
		cancelPosts()
	}
}
