// phase4-upload is the deliberately narrow, trusted Phase 4 proof importer.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"syscall"

	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

type config struct {
	eventPath, runtimePath, runtimeVersion, runtimeQueue, node24Path, commit, workflow string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, transport.Agent{Runner: transport.CommandRunner{Stderr: os.Stderr}}, os.Getenv); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "phase4-upload: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, agent transport.Agent, getenv func(string) string) error {
	cfg, err := parseArgs(args)
	if err != nil {
		return err
	}
	importer := getenv("BUILDKITE_STEP_KEY")
	if getenv("BUILDKITE") == "" || importer == "" {
		return errors.New("BUILDKITE and BUILDKITE_STEP_KEY are required")
	}
	workflow, err := os.ReadFile(cfg.workflow)
	if err != nil {
		return fmt.Errorf("read workflow: %w", err)
	}
	event, err := os.ReadFile(cfg.eventPath)
	if err != nil {
		return fmt.Errorf("read event: %w", err)
	}
	event, err = bindCommit(event, cfg.commit)
	if err != nil {
		return err
	}
	runtimeBytes, err := os.ReadFile(cfg.runtimePath)
	if err != nil {
		return fmt.Errorf("read runtime: %w", err)
	}
	nodeBytes, err := os.ReadFile(cfg.node24Path)
	if err != nil {
		return fmt.Errorf("read Node 24: %w", err)
	}
	nodeArchive, err := deterministicGzip(nodeBytes)
	if err != nil {
		return fmt.Errorf("compress Node 24: %w", err)
	}
	runtimeDigest, nodeDigest := transport.Digest(runtimeBytes), transport.Digest(nodeArchive)
	actionCache, err := os.MkdirTemp("", "buildkite-gha-phase4-actions-")
	if err != nil {
		return fmt.Errorf("create action cache: %w", err)
	}
	defer func() { _ = os.RemoveAll(actionCache) }()
	resolver, err := source.NewResolver(nil)
	if err != nil {
		return fmt.Errorf("configure action resolver: %w", err)
	}
	store, err := source.NewStore(actionCache, nil)
	if err != nil {
		return fmt.Errorf("configure action store: %w", err)
	}
	bundle, err := compiler.CompileBundleContext(ctx, cfg.workflow, workflow, event, cfg.runtimeVersion, runtimeDigest, importer, compiler.Options{
		EventTrust:         compiler.EventTrusted,
		Runners:            compiler.RunnerPolicy{Labels: map[string]string{"ubuntu-latest": cfg.runtimeQueue, "ubuntu-24.04": cfg.runtimeQueue, "ubuntu-22.04": cfg.runtimeQueue}},
		ActionSource:       compiler.PublicActionSource{Resolver: resolver, Store: store},
		NodeRuntimeDigests: map[int]string{24: nodeDigest},
	})
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	runtimeArtifactPath, err := buildkite.DistributionPath(runtimeDigest)
	if err != nil {
		return err
	}
	nodePath, err := buildkite.NodeRuntimePath(24, nodeDigest)
	if err != nil {
		return err
	}
	artifacts := []transport.Artifact{{Path: runtimeArtifactPath, Digest: runtimeDigest, Contents: runtimeBytes}, {Path: nodePath, Digest: nodeDigest, Contents: nodeArchive}}
	for _, p := range bundle.Plans {
		artifacts = append(artifacts, transport.Artifact{Path: p.Path, Digest: p.Digest, Contents: p.Contents})
	}
	root, err := os.MkdirTemp("", "buildkite-gha-phase4-upload-")
	if err != nil {
		return fmt.Errorf("create artifact root: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	if err := transport.UploadArtifacts(ctx, agent, root, artifacts, bundle.Pipeline); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Uploaded %d jobs with importer %s.\n", len(bundle.Plans), importer)
	return err
}

func parseArgs(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("phase4-upload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.eventPath, "event-path", "", "")
	fs.StringVar(&c.runtimePath, "runtime", "", "")
	fs.StringVar(&c.runtimeVersion, "runtime-version", "", "")
	fs.StringVar(&c.runtimeQueue, "runtime-queue", "", "")
	fs.StringVar(&c.node24Path, "node24", "", "")
	fs.StringVar(&c.commit, "commit", "", "")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.eventPath == "" || c.runtimePath == "" || c.runtimeVersion == "" || c.runtimeQueue == "" || c.node24Path == "" || c.commit == "" {
		return c, errors.New("all flags are required")
	}
	if c.runtimeQueue != "hosted" {
		return c, errors.New("--runtime-queue must be hosted")
	}
	if !commitRE.MatchString(c.commit) {
		return c, errors.New("--commit must be 40 lowercase hexadecimal characters")
	}
	if fs.NArg() != 1 {
		return c, errors.New("exactly one workflow is required")
	}
	c.workflow = fs.Arg(0)
	return c, nil
}

func bindCommit(source []byte, commit string) ([]byte, error) {
	var event struct {
		Provider   string              `json:"provider"`
		Event      string              `json:"event"`
		Repository compiler.Repository `json:"repository"`
		Ref        string              `json:"ref"`
		SHA        string              `json:"sha"`
		Actor      string              `json:"actor"`
		Payload    map[string]any      `json:"payload"`
	}
	d := json.NewDecoder(bytes.NewReader(source))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(&event); err != nil {
		return nil, fmt.Errorf("parse event snapshot: %w", err)
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, errors.New("parse event snapshot: multiple JSON values")
		}
		return nil, fmt.Errorf("parse event snapshot: %w", err)
	}
	event.SHA = commit
	bound, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("bind event commit: %w", err)
	}
	return bound, nil
}

func deterministicGzip(source []byte) ([]byte, error) {
	var out bytes.Buffer
	w, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	w.Header.OS = 255
	if _, err := w.Write(source); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
