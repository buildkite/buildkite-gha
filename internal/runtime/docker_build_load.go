package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// buildxDefaultLoadDrivers lists the buildx drivers that keep build results
// inside BuildKit and accept the default-load driver option. The docker
// driver always loads its result into the daemon.
var buildxDefaultLoadDrivers = map[string]bool{"docker-container": true, "kubernetes": true, "remote": true}

// enableDockerBuildLoad makes plain `docker build` load its image into the
// Docker daemon on native Namespace runners, where the Docker CLI routes
// `docker build` to a buildx builder with the remote driver and
// `docker build -t app . && docker run app` otherwise fails with "No output
// specified with remote driver". It adds default-load=true to the single node
// of every stored builder whose driver keeps results in BuildKit, in place,
// because these runners are destroyed after the job. Other stored fields, such
// as TLS driver options and pinned platforms, are left as they are.
//
// Builders with more than one node are left unchanged: with default-load set,
// buildx turns an output-less build into an OCI export and rejects OCI exports
// that span more than one node, so an output-less multi-platform build that
// ran cache-only before would fail. The runtime names each skipped builder so
// the job knows plain `docker build` still needs --load there.
//
// The step is best effort and never fails the job: the first problem prints
// one warning and leaves the remaining builders unchanged.
func (r *jobRun) enableDockerBuildLoad(processor *commandOutputProcessor, env map[string]string) {
	if !r.DockerBuildLoad || r.jobContainer != nil {
		return
	}
	storeDir := buildxStoreDir(processEnvValues(env))
	if storeDir == "" {
		return
	}
	enabled, multiNode, err := enableBuildxDefaultLoad(filepath.Join(storeDir, "instances"))
	if err != nil {
		processor.writeLiteral(processor.stderr, fmt.Sprintf("buildkite-gha: docker build may not load images into the Docker daemon: %v", err))
	}
	if len(enabled) > 0 {
		processor.writeLiteral(processor.stdout, fmt.Sprintf("buildkite-gha: enabled default-load on buildx builder %s so docker build loads images into the Docker daemon", strings.Join(enabled, ", ")))
	}
	for _, name := range multiNode {
		processor.writeLiteral(processor.stdout, fmt.Sprintf("buildkite-gha: buildx builder %s has more than one node, so plain docker build will not load images into the Docker daemon there; use --load", name))
	}
}

// errBuildxMultiNode reports a stored builder with more than one node, which
// enableBuildxDefaultLoad leaves unchanged.
var errBuildxMultiNode = errors.New("stored builder has more than one node")

// enableBuildxDefaultLoad adds default-load=true to the stored builders in
// the buildx instances directory dir. It returns the names it changed and the
// names it skipped because they have more than one node. It stops at the first
// problem. A missing directory means no stored builders.
func enableBuildxDefaultLoad(dir string) (enabled, multiNode []string, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return enabled, multiNode, err
		}
		updated, changed, err := withBuildxDefaultLoad(data)
		if errors.Is(err, errBuildxMultiNode) {
			multiNode = append(multiNode, entry.Name())
			continue
		}
		if err != nil {
			return enabled, multiNode, fmt.Errorf("%s: %w", path, err)
		}
		if !changed {
			continue
		}
		if err := os.WriteFile(path, updated, 0o600); err != nil {
			return enabled, multiNode, err
		}
		enabled = append(enabled, entry.Name())
	}
	return enabled, multiNode, nil
}

// buildxStoreDir resolves the buildx state directory the way buildx does:
// BUILDX_CONFIG, then the Docker CLI configuration directory.
func buildxStoreDir(env map[string]string) string {
	if dir := environmentValue(env, "BUILDX_CONFIG"); dir != "" {
		return dir
	}
	if dir := environmentValue(env, "DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "buildx")
	}
	if home := environmentValue(env, "HOME"); home != "" {
		return filepath.Join(home, ".docker", "buildx")
	}
	return ""
}

// withBuildxDefaultLoad returns the stored builder with default-load=true on
// its node when the node lacks the option. It preserves all other stored
// fields, leaves builders whose driver does not accept the option unchanged,
// and reports errBuildxMultiNode for builders with more than one node.
func withBuildxDefaultLoad(data []byte) ([]byte, bool, error) {
	var instance map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&instance); err != nil {
		return nil, false, fmt.Errorf("decode stored builder: %w", err)
	}
	driver, _ := instance["Driver"].(string)
	if !buildxDefaultLoadDrivers[driver] {
		return nil, false, nil
	}
	nodes, ok := instance["Nodes"].([]any)
	if !ok {
		return nil, false, errors.New("stored builder has no nodes")
	}
	if len(nodes) > 1 {
		return nil, false, errBuildxMultiNode
	}
	changed := false
	for i, entry := range nodes {
		node, ok := entry.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("stored builder node %d is not an object", i)
		}
		options, ok := node["DriverOpts"].(map[string]any)
		if node["DriverOpts"] != nil && !ok {
			return nil, false, fmt.Errorf("stored builder node %d has invalid driver options", i)
		}
		if _, set := options["default-load"]; set {
			continue
		}
		if options == nil {
			options = map[string]any{}
		}
		options["default-load"] = "true"
		node["DriverOpts"] = options
		changed = true
	}
	if !changed {
		return nil, false, nil
	}
	updated, err := json.Marshal(instance)
	if err != nil {
		return nil, false, fmt.Errorf("encode stored builder: %w", err)
	}
	return updated, true, nil
}
