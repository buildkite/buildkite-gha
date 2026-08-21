package compiler

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

// Event is the explicit provider event snapshot supplied to compilation.
type Event struct {
	Provider   string         `json:"provider"`
	Event      string         `json:"event"`
	Trust      EventTrust     `json:"trust"`
	Repository Repository     `json:"repository"`
	Ref        string         `json:"ref"`
	SHA        string         `json:"sha"`
	Actor      string         `json:"actor"`
	Payload    map[string]any `json:"payload"`
}

// Repository identifies the source repository in the event snapshot.
type Repository struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

// ParseEvent validates and decodes the event snapshot used for compilation.
// Callers that select work before compiling must use this same parser so event
// applicability cannot diverge from compiler semantics.
func ParseEvent(source []byte) (Event, error) {
	return parseEvent(source)
}

func parseEvent(source []byte) (Event, error) {
	if len(bytes.TrimSpace(source)) == 0 {
		return Event{}, fmt.Errorf("event snapshot is required")
	}
	var input struct {
		Provider   string         `json:"provider"`
		Event      string         `json:"event"`
		Repository Repository     `json:"repository"`
		Ref        string         `json:"ref"`
		SHA        string         `json:"sha"`
		Actor      string         `json:"actor"`
		Payload    map[string]any `json:"payload"`
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return Event{}, fmt.Errorf("parse event snapshot: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Event{}, fmt.Errorf("parse event snapshot: multiple JSON values")
		}
		return Event{}, fmt.Errorf("parse event snapshot: %w", err)
	}
	if strings.TrimSpace(input.Provider) == "" || strings.TrimSpace(input.Event) == "" || strings.TrimSpace(input.Repository.Owner) == "" || strings.TrimSpace(input.Repository.Name) == "" || strings.TrimSpace(input.Ref) == "" || strings.TrimSpace(input.SHA) == "" || strings.TrimSpace(input.Actor) == "" {
		return Event{}, fmt.Errorf("event snapshot requires provider, event, repository owner/name, ref, sha, and actor")
	}
	if input.Payload == nil {
		input.Payload = map[string]any{}
	}
	if input.Event == "merge_group" {
		if err := validateMergeGroupEvent(input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
	}
	if input.Event == "release" {
		if err := validateReleaseEvent(input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
	}
	return Event{
		Provider: input.Provider, Event: input.Event, Repository: input.Repository,
		Ref: input.Ref, SHA: input.SHA, Actor: input.Actor, Payload: input.Payload,
	}, nil
}

func validateMergeGroupEvent(ref, sha string, payload map[string]any) error {
	if action, _ := payload["action"].(string); action != "checks_requested" {
		return fmt.Errorf("merge_group event snapshot requires payload.action to be checks_requested")
	}
	mergeGroup, ok := payload["merge_group"].(map[string]any)
	if !ok {
		return fmt.Errorf("merge_group event snapshot requires payload.merge_group")
	}
	headRef, _ := mergeGroup["head_ref"].(string)
	baseRef, _ := mergeGroup["base_ref"].(string)
	headSHA, _ := mergeGroup["head_sha"].(string)
	baseSHA, _ := mergeGroup["base_sha"].(string)
	if !strings.HasPrefix(headRef, "refs/heads/") || strings.TrimPrefix(headRef, "refs/heads/") == "" {
		return fmt.Errorf("merge_group event snapshot requires payload.merge_group.head_ref to be a branch ref")
	}
	if !strings.HasPrefix(baseRef, "refs/heads/") || strings.TrimPrefix(baseRef, "refs/heads/") == "" {
		return fmt.Errorf("merge_group event snapshot requires payload.merge_group.base_ref to be a branch ref")
	}
	if !validEventCommit(headSHA) || !validEventCommit(baseSHA) {
		return fmt.Errorf("merge_group event snapshot requires full lowercase payload.merge_group head and base SHAs")
	}
	if ref != headRef || sha != headSHA {
		return fmt.Errorf("merge_group event snapshot ref and sha must match payload.merge_group head_ref and head_sha")
	}
	return nil
}

func validateReleaseEvent(ref, sha string, payload map[string]any) error {
	action, _ := payload["action"].(string)
	if action != "published" && action != "created" && action != "released" {
		return fmt.Errorf("release event snapshot requires payload.action to be published, created, or released")
	}
	release, ok := payload["release"].(map[string]any)
	if !ok {
		return fmt.Errorf("release event snapshot requires payload.release")
	}
	tag, tagOK := release["tag_name"].(string)
	draft, draftOK := release["draft"].(bool)
	_, prereleaseOK := release["prerelease"].(bool)
	if !tagOK || strings.TrimSpace(tag) == "" || !draftOK || !prereleaseOK {
		return fmt.Errorf("release event snapshot requires payload.release tag_name, draft, and prerelease")
	}
	if draft {
		if action == "created" {
			return fmt.Errorf("release event snapshot draft created activity does not trigger GitHub Actions")
		}
		return fmt.Errorf("release event snapshot %s activity requires a non-draft release", action)
	}
	if ref != "refs/tags/"+tag {
		return fmt.Errorf("release event snapshot ref must match payload.release.tag_name")
	}
	if !validEventCommit(sha) {
		return fmt.Errorf("release event snapshot requires a full lowercase sha")
	}
	return nil
}

func validEventCommit(commit string) bool {
	decoded, err := hex.DecodeString(commit)
	return err == nil && len(decoded) == 20 && commit == strings.ToLower(commit)
}

func compileContext(event Event, vars map[string]string, workflowPath, workflowName string) expression.CompileContext {
	repository := event.Repository.Owner + "/" + event.Repository.Name
	if workflowName == "" {
		workflowName = canonicalWorkflowName(workflowPath)
	}
	return expression.CompileContext{
		GitHub: map[string]any{
			"event_name":       event.Event,
			"event":            event.Payload,
			"head_ref":         eventHeadRef(event),
			"base_ref":         eventBaseRef(event),
			"repository":       repository,
			"repository_owner": event.Repository.Owner,
			"ref":              event.Ref,
			"ref_name":         plan.EventRefName(event.Ref),
			"ref_type":         plan.EventRefType(event.Ref),
			"sha":              event.SHA,
			"actor":            event.Actor,
			"workflow":         workflowName,
		},
		Event: event.Payload,
		Vars:  vars,
	}
}

func workflowDispatchInputs(parsed *workflow.Workflow, event Event) map[string]any {
	var declarations []workflow.DispatchInput
	for _, trigger := range parsed.Triggers {
		if trigger.Event == "workflow_dispatch" && trigger.Dispatch != nil {
			declarations = trigger.Dispatch.Inputs
			break
		}
	}
	result := make(map[string]any, len(declarations))
	if event.Event != "workflow_dispatch" && event.Event != "validation" {
		for _, declaration := range declarations {
			result[declaration.Name] = zeroInputValue(declaration.Type)
		}
		return result
	}
	provided, _ := event.Payload["inputs"].(map[string]any)
	for _, declaration := range declarations {
		value, ok := provided[declaration.Name]
		if !ok {
			if declaration.Default != "" {
				value = declaration.Default
			} else {
				value = zeroInputValue(declaration.Type)
			}
		}
		switch declaration.Type {
		case "boolean":
			if text, isString := value.(string); isString {
				if parsed, err := strconv.ParseBool(text); err == nil {
					value = parsed
				}
			}
		case "number":
			if text, isString := value.(string); isString {
				if parsed, err := strconv.ParseFloat(text, 64); err == nil {
					value = parsed
				}
			}
		}
		result[declaration.Name] = value
	}
	return result
}

func eventHeadRef(event Event) string {
	if event.Event != "pull_request" && event.Event != "pull_request_target" {
		return ""
	}
	pullRequest, ok := event.Payload["pull_request"].(map[string]any)
	if !ok {
		return ""
	}
	head, ok := pullRequest["head"].(map[string]any)
	if !ok {
		return ""
	}
	ref, _ := head["ref"].(string)
	return ref
}

func eventBaseRef(event Event) string {
	if event.Event != "pull_request" && event.Event != "pull_request_target" {
		return ""
	}
	pullRequest, ok := event.Payload["pull_request"].(map[string]any)
	if !ok {
		return ""
	}
	base, ok := pullRequest["base"].(map[string]any)
	if !ok {
		return ""
	}
	ref, _ := base["ref"].(string)
	return ref
}

func canonicalWorkflowName(path string) string {
	if isRepositoryWorkflowPath(path) {
		root, canonicalPath, err := workflowRepository(path)
		if err == nil {
			if relative, err := repositoryWorkflowPath(root, canonicalPath); err == nil {
				return strings.TrimPrefix(relative, "./")
			}
		}
	}
	return filepath.ToSlash(filepath.Clean(path))
}
