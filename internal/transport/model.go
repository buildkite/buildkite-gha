// Package transport owns the Buildkite artifact and dynamic-pipeline
// contracts. It deliberately depends on a narrow command boundary instead of
// the Buildkite API so its behavior can be proved without a live build.
package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	ResultManifestSchema   = "buildkite-gha/result-manifest/v2"
	MaxResultOutputs       = 64
	MaxResultOutputBytes   = 1024
	MaxResultManifestBytes = 96 * 1024
	MaxResultProducers     = 1024

	MaxResultArtifacts         = 64
	MaxResultArtifactNameBytes = 255
	MaxResultArtifactIDBytes   = 20
	MaxResultArtifactFileCount = 10_000
)

var (
	digestPattern             = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	keyPattern                = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	logicalJobIDPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
	uuidPattern               = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	resultArtifactIDPattern   = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	resultArtifactPathPattern = regexp.MustCompile(`^buildkite-gha/v1/artifacts/[0-9a-f]{64}\.zip$`)
)

// Digest returns the content address used by transport artifacts.
func Digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing JSON data")
	}
	return nil
}

// Output is a bounded, non-secret logical job output.
type Output struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Producer identifies the Buildkite job that owns a result manifest.
type Producer struct {
	BuildID string `json:"build_id"`
	JobID   string `json:"job_id"`
	StepKey string `json:"step_key"`
}

// Validate rejects incomplete or malformed Buildkite producer identity before
// a job runs and becomes responsible for publishing a terminal result.
func (p Producer) Validate() error {
	if !uuidPattern.MatchString(p.BuildID) || !uuidPattern.MatchString(p.JobID) || !keyPattern.MatchString(p.StepKey) {
		return errors.New("invalid result producer identity")
	}
	return nil
}

// ResultArtifact binds one compatibility artifact to its immutable native
// Buildkite artifact archive.
type ResultArtifact struct {
	Name      string `json:"name"`
	ID        string `json:"id"`
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	FileCount int    `json:"file_count"`
}

func (a ResultArtifact) validate() error {
	if !utf8.ValidString(a.Name) || len(a.Name) == 0 || len(a.Name) > MaxResultArtifactNameBytes || strings.ContainsAny(a.Name, `":<>|*?`+"\r\n/\\") {
		return fmt.Errorf("invalid result artifact name %q", a.Name)
	}
	if len(a.ID) > MaxResultArtifactIDBytes || !resultArtifactIDPattern.MatchString(a.ID) {
		return fmt.Errorf("invalid result artifact ID %q", a.ID)
	}
	if !resultArtifactPathPattern.MatchString(a.Path) {
		return fmt.Errorf("invalid result artifact path %q", a.Path)
	}
	if !digestPattern.MatchString(a.Digest) {
		return fmt.Errorf("invalid result artifact digest %q", a.Digest)
	}
	if a.Size <= 0 {
		return fmt.Errorf("invalid result artifact size %d", a.Size)
	}
	if a.FileCount <= 0 || a.FileCount > MaxResultArtifactFileCount {
		return fmt.Errorf("invalid result artifact file count %d", a.FileCount)
	}
	return nil
}

// ResultManifest is authoritative; metadata only mirrors its bounded values.
type ResultManifest struct {
	Schema     string           `json:"schema"`
	PlanDigest string           `json:"plan_digest"`
	Producer   Producer         `json:"producer"`
	Result     string           `json:"result"`
	Outputs    []Output         `json:"outputs"`
	Artifacts  []ResultArtifact `json:"artifacts"`
}

// MarshalResultManifest sorts outputs and artifacts and returns stable canonical bytes.
func MarshalResultManifest(manifest ResultManifest) ([]byte, error) {
	manifest.Schema = ResultManifestSchema
	manifest.Outputs = append([]Output(nil), manifest.Outputs...)
	if manifest.Outputs == nil {
		manifest.Outputs = []Output{}
	}
	manifest.Artifacts = append([]ResultArtifact(nil), manifest.Artifacts...)
	if manifest.Artifacts == nil {
		manifest.Artifacts = []ResultArtifact{}
	}
	if !digestPattern.MatchString(manifest.PlanDigest) || manifest.Producer.Validate() != nil {
		return nil, errors.New("invalid result manifest identity")
	}
	switch manifest.Result {
	case "success", "failure", "cancelled", "skipped":
	default:
		return nil, fmt.Errorf("invalid logical result %q", manifest.Result)
	}
	if manifest.Result == "skipped" && (len(manifest.Outputs) != 0 || len(manifest.Artifacts) != 0) {
		return nil, errors.New("skipped result manifest must have empty outputs and artifacts")
	}
	if len(manifest.Outputs) > MaxResultOutputs {
		return nil, fmt.Errorf("result manifest has %d outputs, maximum is %d", len(manifest.Outputs), MaxResultOutputs)
	}
	sort.Slice(manifest.Outputs, func(i, j int) bool { return manifest.Outputs[i].Name < manifest.Outputs[j].Name })
	outputNames := make(map[string]struct{}, len(manifest.Outputs))
	for _, output := range manifest.Outputs {
		if !keyPattern.MatchString(output.Name) || len(output.Value) > MaxResultOutputBytes {
			return nil, fmt.Errorf("invalid or oversized output %q", output.Name)
		}
		name := strings.ToLower(output.Name)
		if _, exists := outputNames[name]; exists {
			return nil, fmt.Errorf("duplicate output %q", output.Name)
		}
		outputNames[name] = struct{}{}
	}
	if len(manifest.Artifacts) > MaxResultArtifacts {
		return nil, fmt.Errorf("result manifest has %d artifacts, maximum is %d", len(manifest.Artifacts), MaxResultArtifacts)
	}
	sort.Slice(manifest.Artifacts, func(i, j int) bool {
		if manifest.Artifacts[i].Name == manifest.Artifacts[j].Name {
			return manifest.Artifacts[i].ID < manifest.Artifacts[j].ID
		}
		return manifest.Artifacts[i].Name < manifest.Artifacts[j].Name
	})
	artifactNames := make(map[string]struct{}, len(manifest.Artifacts))
	artifactIDs := make(map[string]struct{}, len(manifest.Artifacts))
	artifactPaths := make(map[string]struct{}, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		if err := artifact.validate(); err != nil {
			return nil, err
		}
		name := strings.ToLower(artifact.Name)
		if _, exists := artifactNames[name]; exists {
			return nil, fmt.Errorf("duplicate result artifact name %q", artifact.Name)
		}
		if _, exists := artifactIDs[artifact.ID]; exists {
			return nil, fmt.Errorf("duplicate result artifact ID %q", artifact.ID)
		}
		if _, exists := artifactPaths[artifact.Path]; exists {
			return nil, fmt.Errorf("duplicate result artifact path %q", artifact.Path)
		}
		artifactNames[name] = struct{}{}
		artifactIDs[artifact.ID] = struct{}{}
		artifactPaths[artifact.Path] = struct{}{}
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(manifest); err != nil {
		return nil, fmt.Errorf("encode result manifest: %w", err)
	}
	encoded := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	if len(encoded) > MaxResultManifestBytes {
		return nil, fmt.Errorf("result manifest is %d bytes, maximum is %d", len(encoded), MaxResultManifestBytes)
	}
	return encoded, nil
}

// VerifyResultManifest checks both the signed-plan identity and exact artifact
// producer selected by the downloader.
func VerifyResultManifest(data []byte, expectedPlan string, expected Producer) (ResultManifest, error) {
	if len(data) > MaxResultManifestBytes {
		return ResultManifest{}, fmt.Errorf("result manifest is %d bytes, maximum is %d", len(data), MaxResultManifestBytes)
	}
	var manifest ResultManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return ResultManifest{}, fmt.Errorf("decode result manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return ResultManifest{}, fmt.Errorf("decode result manifest: %w", err)
	}
	canonical, err := MarshalResultManifest(manifest)
	if err != nil {
		return ResultManifest{}, err
	}
	if !bytes.Equal(canonical, data) {
		return ResultManifest{}, errors.New("result manifest is not canonical")
	}
	if manifest.PlanDigest != expectedPlan || manifest.Producer != expected {
		return ResultManifest{}, errors.New("result manifest producer or plan binding mismatch")
	}
	return manifest, nil
}

// ResultPath is stable and requires artifact download to be constrained by the
// producer step as a separate Buildkite API argument.
func ResultPath(stepKey, planDigest string) string {
	return fmt.Sprintf("buildkite-gha/v1/results/%s/%s.json", stepKey, strings.TrimPrefix(planDigest, "sha256:"))
}

// ResultMetadata returns the visibility-only metadata mirror.
func ResultMetadata(workflow, instance string, manifest ResultManifest, manifestBytes []byte) (map[string]string, error) {
	if !keyPattern.MatchString(workflow) || !keyPattern.MatchString(instance) {
		return nil, errors.New("invalid result metadata namespace")
	}
	resultPrefix := fmt.Sprintf("buildkite-gha/v1/results/%s/%s", workflow, instance)
	outputPrefix := fmt.Sprintf("buildkite-gha/v1/outputs/%s/%s", workflow, instance)
	values := map[string]string{
		resultPrefix:                      manifest.Result,
		resultPrefix + "/manifest-digest": Digest(manifestBytes),
	}
	for _, output := range manifest.Outputs {
		values[outputPrefix+"/"+output.Name] = output.Value
	}
	return values, nil
}
