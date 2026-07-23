// Package harness provides the repository and evidence primitives used by
// differential smoke runners.
package harness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// CommitIdentity is the complete author and committer identity used for the
// fixture repository's initial commit.
type CommitIdentity struct {
	Name  string
	Email string
	When  time.Time
}

// Repository is a temporary Git repository materialized from a fixture.
// Close verifies that the source fixture was not mutated and removes the
// temporary repository, including when verification fails.
type Repository struct {
	Path   string
	Commit string

	parent       string
	source       string
	sourceDigest string
	closeOnce    sync.Once
	closeErr     error
}

// Materialize copies source into a new temporary repository and creates one
// real Git commit with identity. Source must contain only directories and
// regular files; symlinks and an existing .git entry are rejected.
func Materialize(ctx context.Context, source string, identity CommitIdentity) (*Repository, error) {
	if err := validateIdentity(identity); err != nil {
		return nil, err
	}

	source, err := filepath.Abs(source)
	if err != nil {
		return nil, fmt.Errorf("resolve fixture path: %w", err)
	}
	before, err := treeDigest(source)
	if err != nil {
		return nil, fmt.Errorf("inspect fixture: %w", err)
	}

	parent, err := os.MkdirTemp("", "buildkite-gha-smoke-")
	if err != nil {
		return nil, fmt.Errorf("create temporary fixture repository: %w", err)
	}
	cleanup := func(cause error) (*Repository, error) {
		if removeErr := os.RemoveAll(parent); removeErr != nil {
			return nil, errors.Join(cause, fmt.Errorf("remove temporary fixture repository: %w", removeErr))
		}
		return nil, cause
	}

	repoPath := filepath.Join(parent, "repo")
	if err := copyTree(source, repoPath); err != nil {
		return cleanup(fmt.Errorf("copy fixture: %w", err))
	}
	copied, err := treeDigest(repoPath)
	if err != nil {
		return cleanup(fmt.Errorf("inspect copied fixture: %w", err))
	}
	if copied != before {
		return cleanup(errors.New("materialized fixture does not match its source snapshot"))
	}
	after, err := treeDigest(source)
	if err != nil {
		return cleanup(fmt.Errorf("reinspect fixture: %w", err))
	}
	if before != after {
		return cleanup(errors.New("fixture mutated while it was being materialized"))
	}

	templatePath := filepath.Join(parent, "git-template")
	if err := os.Mkdir(templatePath, 0o700); err != nil {
		return cleanup(fmt.Errorf("create empty Git template: %w", err))
	}
	if err := runGit(ctx, repoPath, nil, "init", "--initial-branch=main", "--template="+templatePath); err != nil {
		return cleanup(err)
	}
	if err := runGit(ctx, repoPath, nil, "add", "--all"); err != nil {
		return cleanup(err)
	}
	date := identity.When.Format(time.RFC3339Nano)
	gitEnv := []string{
		"GIT_AUTHOR_NAME=" + identity.Name,
		"GIT_AUTHOR_EMAIL=" + identity.Email,
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + identity.Name,
		"GIT_COMMITTER_EMAIL=" + identity.Email,
		"GIT_COMMITTER_DATE=" + date,
	}
	if err := runGit(ctx, repoPath, gitEnv, "-c", "commit.gpgsign=false", "commit", "--no-gpg-sign", "-m", "test: materialize smoke fixture"); err != nil {
		return cleanup(err)
	}
	commit, err := gitOutput(ctx, repoPath, "rev-parse", "HEAD")
	if err != nil {
		return cleanup(err)
	}

	return &Repository{
		Path:         repoPath,
		Commit:       commit,
		parent:       parent,
		source:       source,
		sourceDigest: before,
	}, nil
}

// Close verifies the source fixture and removes the temporary repository.
func (r *Repository) Close() error {
	r.closeOnce.Do(func() {
		got, err := treeDigest(r.source)
		if err != nil {
			r.closeErr = fmt.Errorf("verify source fixture: %w", err)
		} else if got != r.sourceDigest {
			r.closeErr = errors.New("source fixture mutated while the temporary repository was in use")
		}
		if err := os.RemoveAll(r.parent); err != nil {
			r.closeErr = errors.Join(r.closeErr, fmt.Errorf("remove temporary fixture repository: %w", err))
		}
	})
	return r.closeErr
}

// Target describes a checked-in smoke workflow and whether its runtime is part
// of the current harness lane.
type Target struct {
	Name                   string
	WorkflowPath           string
	RuntimeReady           bool
	ExpectedObservationIDs []string
}

var targets = map[string]Target{
	"shell": {
		Name:                   "shell",
		WorkflowPath:           ".github/workflows/shell.yml",
		RuntimeReady:           true,
		ExpectedObservationIDs: []string{"consumer[variant=one]", "consumer[variant=two]"},
	},
	"ci": {
		Name:                   "ci",
		WorkflowPath:           ".github/workflows/ci.yml",
		ExpectedObservationIDs: []string{"consumer"},
	},
	"artifact": {
		Name:                   "artifact",
		WorkflowPath:           ".github/workflows/artifact.yml",
		ExpectedObservationIDs: []string{"consumer[variant=one]", "consumer[variant=two]"},
	},
}

// LookupTarget returns an isolated copy of the named smoke target.
func LookupTarget(name string) (Target, error) {
	target, ok := targets[name]
	if !ok {
		return Target{}, fmt.Errorf("unknown smoke target %q", name)
	}
	target.ExpectedObservationIDs = append([]string(nil), target.ExpectedObservationIDs...)
	return target, nil
}

// Record is one portable observation document or lifecycle event. Identity is
// stable across providers; Document must contain one JSON value.
type Record struct {
	Identity string          `json:"identity"`
	Document json.RawMessage `json:"document"`
}

// Capture contains evidence from one runner. ProviderFields and
// NativeTransport are retained for runner diagnostics but are deliberately
// excluded from Normalize and Compare.
type Capture struct {
	Provider        string
	Observations    []Record
	Lifecycle       []Record
	ProviderFields  map[string]json.RawMessage
	NativeTransport []Record
}

type normalizedCapture struct {
	Observations []Record `json:"observations"`
	Lifecycle    []Record `json:"lifecycle"`
}

// CaptureShellOutput extracts and validates the portable observation records
// emitted by shell.yml. Provider log prefixes before SMOKE_OBSERVATION= are
// tolerated; the JSON value must end the line.
func CaptureShellOutput(provider string, output io.Reader) (Capture, error) {
	const marker = "SMOKE_OBSERVATION="
	capture := Capture{Provider: provider}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		index := strings.Index(line, marker)
		if index < 0 {
			continue
		}
		document := json.RawMessage(strings.TrimSpace(line[index+len(marker):]))
		var value struct {
			Variant string `json:"variant"`
		}
		if err := decodeJSON(document, &value); err != nil {
			return Capture{}, fmt.Errorf("decode shell observation: %w", err)
		}
		identity := fmt.Sprintf("consumer[variant=%s]", value.Variant)
		capture.Observations = append(capture.Observations, Record{Identity: identity, Document: document})
	}
	if err := scanner.Err(); err != nil {
		return Capture{}, fmt.Errorf("read shell output: %w", err)
	}

	expected := Capture{Observations: []Record{
		{Identity: "consumer[variant=one]", Document: json.RawMessage(`{"result":"smoke-shell","variant":"one"}`)},
		{Identity: "consumer[variant=two]", Document: json.RawMessage(`{"result":"smoke-shell","variant":"two"}`)},
	}}
	if err := Compare(expected, capture); err != nil {
		return Capture{}, fmt.Errorf("capture shell target: %w", err)
	}
	return capture, nil
}

// CaptureConcurrentOutput extracts the single portable observation emitted by
// concurrent.yml after all Phase 3 assertions have passed.
func CaptureConcurrentOutput(provider string, output io.Reader) (Capture, error) {
	const marker = "PHASE3_OBSERVATION="
	capture := Capture{Provider: provider}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		index := strings.Index(line, marker)
		if index < 0 {
			continue
		}
		document := json.RawMessage(strings.TrimSpace(line[index+len(marker):]))
		var value map[string]any
		if err := decodeJSON(document, &value); err != nil {
			return Capture{}, fmt.Errorf("decode concurrent observation: %w", err)
		}
		capture.Observations = append(capture.Observations, Record{Identity: "concurrent", Document: document})
	}
	if err := scanner.Err(); err != nil {
		return Capture{}, fmt.Errorf("read concurrent output: %w", err)
	}
	expected := Capture{Observations: []Record{{Identity: "concurrent", Document: json.RawMessage(`{"cancel":"graceful","failure":"failure-at-wait","implicit":"implicit-wait-all","parallel":"parallel","queue_max":10,"targeted":"targeted-and-full"}`)}}}
	if err := Compare(expected, capture); err != nil {
		return Capture{}, fmt.Errorf("capture concurrent target: %w", err)
	}
	return capture, nil
}

// ReadObservation reads a JSON observation below root. Absolute paths, parent
// traversal, and symlinks that resolve outside root are rejected.
func ReadObservation(root, identity, path string) (Record, error) {
	if !filepath.IsLocal(path) {
		return Record{}, fmt.Errorf("read observation %q: path %q escapes root", identity, path)
	}
	rootDirectory, err := os.OpenRoot(root)
	if err != nil {
		return Record{}, fmt.Errorf("read observation %q: open root: %w", identity, err)
	}
	document, readErr := rootDirectory.ReadFile(path)
	if err := errors.Join(readErr, rootDirectory.Close()); err != nil {
		return Record{}, fmt.Errorf("read observation %q within root: %w", identity, err)
	}
	if _, err := canonicalJSON(document); err != nil {
		return Record{}, fmt.Errorf("read observation %q: %w", identity, err)
	}
	return Record{Identity: identity, Document: document}, nil
}

// Normalize returns canonical, provider-neutral JSON. Observations are ordered
// by identity, lifecycle order is preserved, and documents are canonicalized
// recursively.
func Normalize(capture Capture) ([]byte, error) {
	observations, err := normalizeRecords("observation", capture.Observations, true)
	if err != nil {
		return nil, err
	}
	lifecycle, err := normalizeRecords("lifecycle event", capture.Lifecycle, false)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalizedCapture{Observations: observations, Lifecycle: lifecycle})
}

// Compare requires the same observation and lifecycle identities and canonical
// JSON values. Provider-specific fields and native transport artifacts are not
// part of the comparison.
func Compare(expected, actual Capture) error {
	if err := compareRecords("observation", expected.Observations, actual.Observations, false); err != nil {
		return err
	}
	if err := compareRecords("lifecycle event", expected.Lifecycle, actual.Lifecycle, true); err != nil {
		return err
	}
	return nil
}

func compareRecords(kind string, expected, actual []Record, ordered bool) error {
	want, err := recordMap(kind, expected)
	if err != nil {
		return fmt.Errorf("expected capture: %w", err)
	}
	got, err := recordMap(kind, actual)
	if err != nil {
		return fmt.Errorf("actual capture: %w", err)
	}
	wantIdentities := sortedKeys(want)
	gotIdentities := sortedKeys(got)
	for _, identity := range wantIdentities {
		if _, ok := got[identity]; !ok {
			return fmt.Errorf("missing expected %s %q", kind, identity)
		}
	}
	for _, identity := range gotIdentities {
		if _, ok := want[identity]; !ok {
			return fmt.Errorf("unknown %s identity %q", kind, identity)
		}
	}
	for _, identity := range wantIdentities {
		if !bytes.Equal(want[identity], got[identity]) {
			return fmt.Errorf("%s %q differs: expected %s, got %s", kind, identity, want[identity], got[identity])
		}
	}
	if ordered {
		for index := range expected {
			if expected[index].Identity != actual[index].Identity {
				return fmt.Errorf("%s order differs at position %d: expected %q, got %q", kind, index, expected[index].Identity, actual[index].Identity)
			}
		}
	}
	return nil
}

func recordMap(kind string, records []Record) (map[string][]byte, error) {
	result := make(map[string][]byte, len(records))
	for _, record := range records {
		if strings.TrimSpace(record.Identity) == "" {
			return nil, fmt.Errorf("%s identity is empty", kind)
		}
		if _, exists := result[record.Identity]; exists {
			return nil, fmt.Errorf("duplicate %s identity %q", kind, record.Identity)
		}
		document, err := canonicalJSON(record.Document)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", kind, record.Identity, err)
		}
		result[record.Identity] = document
	}
	return result, nil
}

func normalizeRecords(kind string, records []Record, sortByIdentity bool) ([]Record, error) {
	values, err := recordMap(kind, records)
	if err != nil {
		return nil, err
	}
	identities := make([]string, 0, len(values))
	if sortByIdentity {
		identities = sortedKeys(values)
	} else {
		for _, record := range records {
			identities = append(identities, record.Identity)
		}
	}
	normalized := make([]Record, 0, len(values))
	for _, identity := range identities {
		normalized = append(normalized, Record{Identity: identity, Document: values[identity]})
	}
	return normalized, nil
}

func sortedKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func canonicalJSON(document []byte) ([]byte, error) {
	var value any
	if err := decodeJSON(document, &value); err != nil {
		return nil, fmt.Errorf("invalid JSON document: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON document: %w", err)
	}
	return canonical, nil
}

func decodeJSON(document []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateIdentity(identity CommitIdentity) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "name", value: identity.Name},
		{name: "email", value: identity.Email},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("commit identity %s is required", field.name)
		}
		if strings.ContainsAny(field.value, "\x00\r\n") {
			return fmt.Errorf("commit identity %s contains an invalid character", field.name)
		}
	}
	if identity.When.IsZero() {
		return errors.New("commit identity time is required")
	}
	return nil
}

func treeDigest(root string) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("fixture root is not a directory")
	}

	hash := sha256.New()
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			_, _ = fmt.Fprintf(hash, "d . %o\x00", info.Mode().Perm())
			return nil
		}
		if entry.Name() == ".git" {
			return errors.New("fixture contains forbidden .git entry")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			_, _ = fmt.Fprintf(hash, "d %s %o\x00", filepath.ToSlash(relative), info.Mode().Perm())
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fixture entry %q is not a regular file", relative)
		}
		_, _ = fmt.Fprintf(hash, "f %s %o %d\x00", filepath.ToSlash(relative), info.Mode().Perm(), info.Size())
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := destination
		if relative != "." {
			target = filepath.Join(destination, relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fixture entry %q is not a regular file", relative)
		}
		sourceFile, err := os.Open(path)
		if err != nil {
			return err
		}
		targetFile, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return errors.Join(err, sourceFile.Close())
		}
		_, copyErr := io.Copy(targetFile, sourceFile)
		if err := errors.Join(copyErr, targetFile.Close(), sourceFile.Close()); err != nil {
			return err
		}
		return os.Chmod(target, info.Mode().Perm())
	})
}

func runGit(ctx context.Context, directory string, extraEnv []string, args ...string) error {
	_, err := gitCommand(ctx, directory, extraEnv, args...)
	return err
}

func gitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	output, err := gitCommand(ctx, directory, nil, args...)
	return strings.TrimSpace(string(output)), err
}

func gitCommand(ctx context.Context, directory string, extraEnv []string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	command.Env = gitEnvironment(extraEnv)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func gitEnvironment(extra []string) []string {
	dangerous := map[string]bool{
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_COMMON_DIR":                   true,
		"GIT_DIR":                          true,
		"GIT_INDEX_FILE":                   true,
		"GIT_OBJECT_DIRECTORY":             true,
		"GIT_WORK_TREE":                    true,
	}
	environment := make([]string, 0, len(os.Environ())+len(extra)+3)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if dangerous[name] || strings.HasPrefix(name, "GIT_CONFIG_") {
			continue
		}
		environment = append(environment, value)
	}
	environment = append(environment,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	)
	return append(environment, extra...)
}
