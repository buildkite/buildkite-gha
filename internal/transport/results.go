package transport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ResultSource is the immutable compiler-owned identity of one generated
// producer belonging to a logical GHA need.
type ResultSource struct {
	StepKey    string
	PlanDigest string
}

// NeedResult is the verified runtime projection exposed to needs contexts.
type NeedResult struct {
	Result    string
	Outputs   map[string]string
	Producers []Producer
	Artifacts []NeedArtifact
}

type NeedArtifact struct {
	Artifact ResultArtifact
	Producer Producer
}

// Publication records the authoritative artifact and non-fatal visibility
// failures. Consumers never use metadata or annotations as runtime authority.
type Publication struct {
	Path                   string
	ManifestDigest         string
	MetadataMirrorError    error
	SummaryAnnotationError error
	WarningAnnotationError error
	ErrorAnnotationError   error
}

// SearchArtifactProducer resolves exactly one artifact owner under the
// compiler-selected step key. The returned job UUID is then used for download.
func (a Agent) SearchArtifactProducer(ctx context.Context, path, producerStep string) (string, error) {
	if !keyPattern.MatchString(producerStep) {
		return "", fmt.Errorf("invalid producer step key %q", producerStep)
	}
	output, err := a.run(ctx, []string{"artifact", "search", path, "--step", producerStep, "--format", "%j"}, nil)
	if err != nil {
		return "", err
	}
	lines := strings.Fields(string(output))
	if len(lines) != 1 || !uuidPattern.MatchString(lines[0]) {
		return "", fmt.Errorf("artifact %q under step %q has %d valid producer jobs, want exactly one", path, producerStep, len(lines))
	}
	return lines[0], nil
}

// PublishResult uploads the authoritative canonical manifest before attempting
// visibility-only metadata mirrors. Artifact failure is fatal; mirror failure
// is returned separately so it cannot downgrade a published logical result.
func PublishResult(ctx context.Context, agent Agent, root, workflow, instance string, manifest ResultManifest) (Publication, error) {
	encoded, err := MarshalResultManifest(manifest)
	if err != nil {
		return Publication{}, err
	}
	path := ResultPath(manifest.Producer.StepKey, manifest.PlanDigest)
	materialized, err := materializeResult(root, path, encoded)
	if err != nil {
		return Publication{}, err
	}
	if err := verifyMaterialized(materialized, encoded, Digest(encoded)); err != nil {
		return Publication{}, fmt.Errorf("verify result before upload: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Publication{}, fmt.Errorf("resolve result artifact root: %w", err)
	}
	if err := agent.UploadArtifactFrom(ctx, resolvedRoot, path); err != nil {
		return Publication{}, fmt.Errorf("upload result manifest: %w", err)
	}
	publication := Publication{Path: path, ManifestDigest: Digest(encoded)}
	metadata, err := ResultMetadata(workflow, instance, manifest, encoded)
	if err != nil {
		publication.MetadataMirrorError = err
		return publication, nil
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := agent.SetMetadata(ctx, key, metadata[key]); err != nil {
			publication.MetadataMirrorError = errors.Join(publication.MetadataMirrorError, fmt.Errorf("set metadata mirror %q: %w", key, err))
		}
	}
	return publication, nil
}

func materializeResult(root, relative string, contents []byte) (string, error) {
	if root == "" {
		return "", fmt.Errorf("result artifact root is required")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve result artifact root: %w", err)
	}
	rootFS, err := os.OpenRoot(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("open result artifact root: %w", err)
	}
	defer func() { _ = rootFS.Close() }()
	path, err := writeMaterialized(rootFS, absoluteRoot, relative, contents)
	if err != nil {
		return "", fmt.Errorf("materialize result manifest: %w", err)
	}
	return path, nil
}

// DownloadResult resolves the producer job UUID from the expected step, uses
// that UUID for the download, and verifies all embedded producer bindings.
func DownloadResult(ctx context.Context, agent Agent, root, buildID string, source ResultSource) (ResultManifest, error) {
	if !uuidPattern.MatchString(buildID) || !keyPattern.MatchString(source.StepKey) || !digestPattern.MatchString(source.PlanDigest) {
		return ResultManifest{}, fmt.Errorf("invalid expected result identity")
	}
	path := ResultPath(source.StepKey, source.PlanDigest)
	jobID, err := agent.SearchArtifactProducer(ctx, path, source.StepKey)
	if err != nil {
		return ResultManifest{}, fmt.Errorf("resolve result producer %q: %w", source.StepKey, err)
	}
	if root == "" {
		return ResultManifest{}, fmt.Errorf("result download root is required")
	}
	destination, err := os.MkdirTemp(root, ".buildkite-gha-results-")
	if err != nil {
		return ResultManifest{}, fmt.Errorf("create result download directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(destination) }()
	if err := agent.DownloadArtifact(ctx, path, destination, jobID); err != nil {
		return ResultManifest{}, fmt.Errorf("download result from producer %q: %w", source.StepKey, err)
	}
	manifestPath := filepath.Join(destination, filepath.FromSlash(path))
	info, err := os.Stat(manifestPath)
	if err != nil {
		return ResultManifest{}, fmt.Errorf("stat downloaded result: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > MaxResultManifestBytes {
		return ResultManifest{}, fmt.Errorf("downloaded result is not a regular bounded manifest")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return ResultManifest{}, fmt.Errorf("read downloaded result: %w", err)
	}
	expected := Producer{BuildID: buildID, JobID: jobID, StepKey: source.StepKey}
	manifest, err := VerifyResultManifest(data, source.PlanDigest, expected)
	if err != nil {
		return ResultManifest{}, fmt.Errorf("verify result from producer %q: %w", source.StepKey, err)
	}
	return manifest, nil
}

// LoadNeeds downloads every exact generated producer for each logical need and
// returns only verified results and outputs. Conflicting matrix outputs fail
// closed because Buildkite artifacts do not expose a trusted completion order.
func LoadNeeds(ctx context.Context, agent Agent, root, buildID string, sources map[string][]ResultSource) (map[string]NeedResult, error) {
	if len(sources) > MaxResultProducers {
		return nil, fmt.Errorf("result transport has %d logical needs, maximum is %d", len(sources), MaxResultProducers)
	}
	names := make([]string, 0, len(sources))
	logicalNames := make(map[string]struct{}, len(sources))
	for name := range sources {
		lower := strings.ToLower(name)
		if _, exists := logicalNames[lower]; exists {
			return nil, fmt.Errorf("result transport repeats logical need %q", name)
		}
		logicalNames[lower] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	needs := make(map[string]NeedResult, len(sources))
	for _, name := range names {
		producers := append([]ResultSource(nil), sources[name]...)
		if !keyPattern.MatchString(name) || len(producers) == 0 || len(producers) > MaxResultProducers {
			return nil, fmt.Errorf("logical need %q has no valid producers", name)
		}
		sort.Slice(producers, func(i, j int) bool { return producers[i].StepKey < producers[j].StepKey })
		need := NeedResult{Result: "skipped", Outputs: map[string]string{}}
		outputNames := make(map[string]string)
		for i, producer := range producers {
			if i > 0 && producers[i-1].StepKey == producer.StepKey {
				return nil, fmt.Errorf("logical need %q repeats producer %q", name, producer.StepKey)
			}
			manifest, err := DownloadResult(ctx, agent, root, buildID, producer)
			if err != nil {
				return nil, fmt.Errorf("load logical need %q: %w", name, err)
			}
			need.Producers = append(need.Producers, manifest.Producer)
			for _, artifact := range manifest.Artifacts {
				need.Artifacts = append(need.Artifacts, NeedArtifact{Artifact: artifact, Producer: manifest.Producer})
			}
			need.Result = aggregateResult(need.Result, manifest.Result)
			for _, output := range manifest.Outputs {
				lower := strings.ToLower(output.Name)
				if canonical, ok := outputNames[lower]; ok && need.Outputs[canonical] != output.Value {
					return nil, fmt.Errorf("logical need %q has conflicting matrix output %q without authoritative completion order", name, output.Name)
				}
				if _, exists := outputNames[lower]; exists {
					continue
				}
				if len(need.Outputs) == MaxResultOutputs {
					return nil, fmt.Errorf("logical need %q has more than %d aggregate outputs", name, MaxResultOutputs)
				}
				outputNames[lower] = output.Name
				need.Outputs[output.Name] = output.Value
			}
		}
		needs[name] = need
	}
	return needs, nil
}

func aggregateResult(current, next string) string {
	priority := map[string]int{"skipped": 0, "success": 1, "cancelled": 2, "failure": 3}
	if priority[next] > priority[current] {
		return next
	}
	return current
}
