// Package metadata owns the supported local GitHub Action metadata model.
package metadata

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Metadata is the supported subset of a local action.yml or action.yaml file.
type Metadata struct {
	Path string `yaml:"-"`
	// SourceRoot is the verified tree whose digest binds this action. For a
	// workspace action it is Path; for a materialized GitHub action it is the
	// repository root.
	SourceRoot  string `yaml:"-"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// DeprecationMessage is inert metadata emitted by deprecated actions.
	DeprecationMessage string            `yaml:"deprecationMessage"`
	Author             string            `yaml:"author"`
	Inputs             map[string]Input  `yaml:"inputs"`
	Outputs            map[string]Output `yaml:"outputs"`
	Runs               Runs              `yaml:"runs"`
	Branding           Branding          `yaml:"branding"`
}

// Input declares one action input.
type Input struct {
	Description              string `yaml:"description"`
	DeprecationMessage       string `yaml:"deprecation-message"`
	LegacyDeprecationMessage string `yaml:"deprecationMessage"`
	// Type is accepted as inert metadata because some GitHub-hosted actions
	// declare it even though action inputs are exposed as strings.
	Type     string  `yaml:"type"`
	Required bool    `yaml:"required"`
	Default  *string `yaml:"default"`
}

// Output declares one action output.
type Output struct {
	Description string `yaml:"description"`
	Value       string `yaml:"value"`
}

// Branding declares inert Marketplace presentation metadata.
type Branding struct {
	Icon  string `yaml:"icon"`
	Color string `yaml:"color"`
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
	With             map[string]string `yaml:"with"`
}

// Runtime identifies one supported action execution model.
type Runtime string

const (
	// MaxNestedActionDepth bounds local action expansion in both compilation and execution.
	MaxNestedActionDepth = 10
	// RuntimeNode16 executes a JavaScript action with managed Node 16.
	RuntimeNode16 Runtime = "node16"
	// RuntimeNode20 identifies the accepted node20 action declaration. Matching
	// GitHub-hosted runners, Runtime maps it to the managed Node 24 runtime.
	RuntimeNode20 Runtime = "node20"
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
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil {
		return Metadata{}, fmt.Errorf("parse action metadata %q: %w", metadataPath, err)
	}
	if err := rejectCompositeControls(metadataPath, &document); err != nil {
		return Metadata{}, err
	}
	if err := validateCompositeSteps(metadataPath, &document); err != nil {
		return Metadata{}, err
	}
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
	for name, input := range metadata.Inputs {
		if input.DeprecationMessage != "" && input.LegacyDeprecationMessage != "" {
			return Metadata{}, fmt.Errorf("parse action metadata %q: input %q declares both deprecation-message and deprecationMessage", metadataPath, name)
		}
		if input.DeprecationMessage == "" {
			input.DeprecationMessage = input.LegacyDeprecationMessage
		}
		input.LegacyDeprecationMessage = ""
		metadata.Inputs[name] = input
	}
	metadata.Outputs, err = lowerNames(metadata.Outputs, "outputs")
	if err != nil {
		return Metadata{}, fmt.Errorf("parse action metadata %q: %w", metadataPath, err)
	}
	return metadata, nil
}

func rejectCompositeControls(path string, document *yaml.Node) error {
	node := document
	if node.Kind == yaml.DocumentNode && len(node.Content) != 0 {
		node = node.Content[0]
	}
	runs := mappingValue(node, "runs")
	using := mappingValue(runs, "using")
	if using == nil || using.Value != string(RuntimeComposite) {
		return nil
	}
	steps := mappingValue(runs, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return nil
	}
	for i, step := range steps.Content {
		id := ""
		if idNode := mappingValue(step, "id"); idNode != nil {
			id = idNode.Value
		}
		for _, control := range []string{"background", "wait", "wait-all", "cancel", "parallel"} {
			if controlNode := mappingKeyNode(step, control); controlNode != nil {
				owner := fmt.Sprintf("child %d", i+1)
				if id != "" {
					owner += fmt.Sprintf(" (id %q)", id)
				}
				return fmt.Errorf("parse action metadata %q:%d:%d: composite %s declares unsupported control %q", path, controlNode.Line, controlNode.Column, owner, control)
			}
		}
	}
	return nil
}

func validateCompositeSteps(path string, document *yaml.Node) error {
	node := document
	if node.Kind == yaml.DocumentNode && len(node.Content) != 0 {
		node = node.Content[0]
	}
	runs := mappingValue(node, "runs")
	using := mappingValue(runs, "using")
	if using == nil || using.Value != string(RuntimeComposite) {
		return nil
	}
	steps := mappingValue(runs, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return nil
	}
	ids := make(map[string]struct{}, len(steps.Content))
	for i, step := range steps.Content {
		run := mappingKeyNode(step, "run")
		uses := mappingKeyNode(step, "uses")
		switch {
		case run != nil && uses != nil:
			return fmt.Errorf("parse action metadata %q:%d:%d: composite child %d declares both run and uses", path, uses.Line, uses.Column, i+1)
		case run == nil && uses == nil:
			return fmt.Errorf("parse action metadata %q:%d:%d: composite child %d has no run or uses execution", path, step.Line, step.Column, i+1)
		case run != nil && mappingKeyNode(step, "with") != nil:
			with := mappingKeyNode(step, "with")
			return fmt.Errorf("parse action metadata %q:%d:%d: composite run child %d may not declare with", path, with.Line, with.Column, i+1)
		}
		id := mappingValue(step, "id")
		if id == nil || id.Value == "" {
			continue
		}
		key := strings.ToLower(id.Value)
		if _, exists := ids[key]; exists {
			return fmt.Errorf("parse action metadata %q:%d:%d: duplicate case-insensitive composite child id %q", path, id.Line, id.Column, key)
		}
		ids[key] = struct{}{}
	}
	return nil
}

func mappingValue(node *yaml.Node, name string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == name {
			return node.Content[i+1]
		}
	}
	return nil
}

func mappingKeyNode(node *yaml.Node, name string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == name {
			return node.Content[i]
		}
	}
	return nil
}

// Runtime classifies the action execution model and rejects unsupported values
// so compilation and execution share one support boundary.
func (metadata Metadata) Runtime() (Runtime, error) {
	switch metadata.Runs.Using {
	case string(RuntimeNode16):
		return RuntimeNode16, nil
	case string(RuntimeNode20):
		return RuntimeNode24, nil
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

// ValidateEntrypoints confines JavaScript lifecycle programs to the verified
// source tree and requires each declared entry point to be a regular file.
// Remote actions may share repository-level build output across action
// subdirectories; workspace actions bind SourceRoot to the action directory.
// Callers should invoke this after Runtime.
func (metadata Metadata) ValidateEntrypoints(runtime Runtime) error {
	if runtime == RuntimeDocker {
		if metadata.Runs.Image != "Dockerfile" {
			return fmt.Errorf("docker action requires exact runs.image %q", "Dockerfile")
		}
		if metadata.Runs.Main != "" || metadata.Runs.Entrypoint != "" || len(metadata.Runs.Args) != 0 ||
			metadata.Runs.Pre != "" || metadata.Runs.PreIf != "" || metadata.Runs.PreEntrypoint != "" ||
			metadata.Runs.Post != "" || metadata.Runs.PostIf != "" || metadata.Runs.PostEntrypoint != "" {
			return fmt.Errorf("docker action may not declare entrypoint, arguments, or pre/post lifecycle")
		}
		dockerfile := filepath.Join(metadata.Path, "Dockerfile")
		info, err := os.Lstat(dockerfile)
		if err != nil {
			return fmt.Errorf("docker action Dockerfile: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("docker action Dockerfile is not a regular non-symlink file")
		}
		return nil
	}
	if runtime != RuntimeNode16 && runtime != RuntimeNode20 && runtime != RuntimeNode24 {
		return nil
	}
	if metadata.Runs.Main == "" {
		return fmt.Errorf("JavaScript action has no main entry point")
	}
	sourceRoot := metadata.SourceRoot
	if sourceRoot == "" {
		sourceRoot = metadata.Path
	}
	for _, lifecycle := range []struct{ phase, entry string }{
		{phase: "pre", entry: metadata.Runs.Pre},
		{phase: "main", entry: metadata.Runs.Main},
		{phase: "post", entry: metadata.Runs.Post},
	} {
		phase, entry := lifecycle.phase, lifecycle.entry
		if entry == "" {
			continue
		}
		clean := path.Clean(entry)
		if path.IsAbs(entry) || clean == "." || strings.Contains(entry, "\\") {
			return fmt.Errorf("JavaScript action %s entry point %q escapes action source", phase, entry)
		}
		candidate := filepath.Join(metadata.Path, filepath.FromSlash(clean))
		relative, err := filepath.Rel(sourceRoot, candidate)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("JavaScript action %s entry point %q escapes action source", phase, entry)
		}
		folded := strings.ToLower(filepath.ToSlash(relative))
		if folded == ".git" || strings.HasPrefix(folded, ".git/") {
			return fmt.Errorf("JavaScript action %s entry point %q is excluded from verified action source", phase, entry)
		}
		candidate, err = resolveWithinRoot(sourceRoot, candidate)
		if err != nil {
			if strings.Contains(err.Error(), "escapes root") {
				return fmt.Errorf("JavaScript action %s entry point %q escapes action source", phase, entry)
			}
			return fmt.Errorf("JavaScript action %s entry point %q: %w", phase, entry, err)
		}
		relative, err = filepath.Rel(sourceRoot, candidate)
		if err != nil {
			return fmt.Errorf("JavaScript action %s entry point %q: %w", phase, entry, err)
		}
		folded = strings.ToLower(filepath.ToSlash(relative))
		if folded == ".git" || strings.HasPrefix(folded, ".git/") {
			return fmt.Errorf("JavaScript action %s entry point %q is excluded from verified action source", phase, entry)
		}
		info, err := os.Lstat(candidate)
		if err != nil {
			return fmt.Errorf("JavaScript action %s entry point %q: %w", phase, entry, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("JavaScript action %s entry point %q is not a regular file", phase, entry)
		}
	}
	return nil
}

// RequiredCapabilities returns the plan capabilities needed by the runtime.
func (runtime Runtime) RequiredCapabilities() []string {
	if runtime == RuntimeDocker {
		return []string{"docker", "network"}
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
