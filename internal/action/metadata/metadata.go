// Package metadata owns the supported local GitHub Action metadata model.
package metadata

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Metadata is the supported subset of a local action.yml or action.yaml file.
type Metadata struct {
	Path        string            `yaml:"-"`
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Inputs      map[string]Input  `yaml:"inputs"`
	Outputs     map[string]Output `yaml:"outputs"`
	Runs        Runs              `yaml:"runs"`
}

// Input declares one action input.
type Input struct {
	Description string  `yaml:"description"`
	Required    bool    `yaml:"required"`
	Default     *string `yaml:"default"`
}

// Output declares one action output.
type Output struct {
	Description string `yaml:"description"`
	Value       string `yaml:"value"`
}

// Runs declares how an action executes.
type Runs struct {
	Using          string            `yaml:"using"`
	Pre            string            `yaml:"pre"`
	PreIf          string            `yaml:"pre-if"`
	Main           string            `yaml:"main"`
	Post           string            `yaml:"post"`
	PostIf         string            `yaml:"post-if"`
	Image          string            `yaml:"image"`
	Entrypoint     string            `yaml:"entrypoint"`
	PreEntrypoint  string            `yaml:"pre-entrypoint"`
	PostEntrypoint string            `yaml:"post-entrypoint"`
	Args           []string          `yaml:"args"`
	Env            map[string]string `yaml:"env"`
	Steps          []CompositeStep   `yaml:"steps"`
}

// CompositeStep is the supported subset of a composite action step.
type CompositeStep struct {
	ID               string            `yaml:"id"`
	Name             string            `yaml:"name"`
	Run              string            `yaml:"run"`
	Uses             string            `yaml:"uses"`
	Shell            string            `yaml:"shell"`
	WorkingDirectory string            `yaml:"working-directory"`
	Env              map[string]string `yaml:"env"`
	If               string            `yaml:"if"`
}

// Runtime identifies one supported action execution model.
type Runtime string

const (
	// RuntimeNode24 executes a JavaScript action with managed Node 24.
	RuntimeNode24 Runtime = "node24"
	// RuntimeComposite executes a composite action.
	RuntimeComposite Runtime = "composite"
	// RuntimeDocker executes a Docker action.
	RuntimeDocker Runtime = "docker"
)

// Load reads strict action metadata from path while confining action and
// metadata symlinks to root.
func Load(root, path string) (Metadata, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Metadata{}, fmt.Errorf("resolve action root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Metadata{}, fmt.Errorf("resolve action root: %w", err)
	}

	actionPath := path
	if !filepath.IsAbs(actionPath) {
		actionPath = filepath.Join(root, filepath.FromSlash(actionPath))
	}
	actionPath, err = resolveWithinRoot(root, actionPath)
	if err != nil {
		return Metadata{}, fmt.Errorf("resolve local action %q: %w", path, err)
	}

	var source []byte
	var metadataPath string
	for _, name := range []string{"action.yml", "action.yaml"} {
		candidate, resolveErr := resolveWithinRoot(root, filepath.Join(actionPath, name))
		if resolveErr != nil {
			if errors.Is(resolveErr, os.ErrNotExist) {
				continue
			}
			return Metadata{}, fmt.Errorf("resolve local action %q metadata: %w", path, resolveErr)
		}
		source, err = os.ReadFile(candidate)
		if err == nil {
			metadataPath = candidate
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return Metadata{}, fmt.Errorf("read local action %q metadata: %w", path, err)
		}
	}
	if metadataPath == "" {
		return Metadata{}, fmt.Errorf("local action %q has no action.yml or action.yaml", path)
	}

	metadata := Metadata{Path: actionPath}
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&metadata); err != nil {
		return Metadata{}, fmt.Errorf("parse action metadata %q: %w", metadataPath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Metadata{}, fmt.Errorf("parse action metadata %q: multiple YAML documents", metadataPath)
		}
		return Metadata{}, fmt.Errorf("parse action metadata %q: %w", metadataPath, err)
	}
	if metadata.Runs.Using == "" {
		return Metadata{}, fmt.Errorf("action metadata %q has no runs.using", metadataPath)
	}
	metadata.Inputs, err = lowerNames(metadata.Inputs, "inputs")
	if err != nil {
		return Metadata{}, fmt.Errorf("parse action metadata %q: %w", metadataPath, err)
	}
	metadata.Outputs, err = lowerNames(metadata.Outputs, "outputs")
	if err != nil {
		return Metadata{}, fmt.Errorf("parse action metadata %q: %w", metadataPath, err)
	}
	return metadata, nil
}

// Runtime classifies the action execution model and rejects unsupported values
// so compilation and execution share one support boundary.
func (metadata Metadata) Runtime() (Runtime, error) {
	switch metadata.Runs.Using {
	case string(RuntimeNode24):
		return RuntimeNode24, nil
	case string(RuntimeComposite):
		return RuntimeComposite, nil
	case string(RuntimeDocker):
		return RuntimeDocker, nil
	default:
		return "", fmt.Errorf("unsupported runtime %q", metadata.Runs.Using)
	}
}

// RequiredCapabilities returns the plan capabilities needed by the runtime.
func (runtime Runtime) RequiredCapabilities() []string {
	if runtime == RuntimeDocker {
		return []string{"docker"}
	}
	return nil
}

func resolveWithinRoot(root, path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := requireWithinRoot(root, path); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if err := requireWithinRoot(root, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func requireWithinRoot(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("resolved path %q escapes root %q", path, root)
	}
	return nil
}

func lowerNames[T any](values map[string]T, kind string) (map[string]T, error) {
	out := make(map[string]T, len(values))
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lower := strings.ToLower(name)
		if _, exists := out[lower]; exists {
			return nil, fmt.Errorf("action %s contain duplicate case-insensitive name %q", kind, lower)
		}
		out[lower] = values[name]
	}
	return out, nil
}
