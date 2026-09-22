package compiler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/git"
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
	deployment := input.Event == "deployment" || input.Event == "deployment_status"
	if strings.TrimSpace(input.Provider) == "" || strings.TrimSpace(input.Event) == "" || strings.TrimSpace(input.Repository.Owner) == "" || strings.TrimSpace(input.Repository.Name) == "" || (!deployment && strings.TrimSpace(input.Ref) == "") || strings.TrimSpace(input.SHA) == "" || strings.TrimSpace(input.Actor) == "" {
		return Event{}, fmt.Errorf("event snapshot requires provider, event, repository owner/name, ref, sha, and actor")
	}
	if input.Payload == nil {
		input.Payload = map[string]any{}
	}
	if input.Event == "milestone" {
		if err := validateMilestoneEvent(input.Provider, input.Repository, input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
	}
	if input.Event == "watch" {
		if err := validateWatchEvent(input.Provider, input.Repository, input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
	}
	if input.Event == "page_build" {
		if err := validatePageBuildEvent(input.Provider, input.Repository, input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
	}
	if input.Event == "gollum" {
		if err := validateGollumEvent(input.Provider, input.Repository, input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
	}
	if input.Event == "public" {
		if err := validatePublicEvent(input.Provider, input.Repository, input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
	}
	if input.Event == "fork" {
		if err := validateForkEvent(input.Provider, input.Repository, input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
	}
	if input.Event == "label" {
		if err := validateLabelEvent(input.Provider, input.Repository, input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
	}
	if input.Event == "create" || input.Event == "delete" {
		if err := validateRefLifecycleEvent(input.Provider, input.Event, input.Repository, input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
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
	if deployment {
		if err := validateDeploymentEvent(input.Provider, input.Event, input.Repository, input.Ref, input.SHA, input.Payload); err != nil {
			return Event{}, err
		}
	}
	return Event{
		Provider: input.Provider, Event: input.Event, Repository: input.Repository,
		Ref: input.Ref, SHA: input.SHA, Actor: input.Actor, Payload: input.Payload,
	}, nil
}

func validateGollumEvent(provider string, repository Repository, ref, sha string, payload map[string]any) error {
	repo, _ := payload["repository"].(map[string]any)
	fullName, _ := repo["full_name"].(string)
	id, _ := repo["id"].(json.Number)
	number, err := id.Int64()
	if provider != "github" || err != nil || number <= 0 || !strings.EqualFold(fullName, repository.Owner+"/"+repository.Name) || !git.ValidObjectID(sha) {
		return fmt.Errorf("gollum requires the original repository identity and a full commit SHA")
	}
	if repository.DefaultBranch == "" || ref != "refs/heads/"+repository.DefaultBranch {
		return fmt.Errorf("gollum must execute the resolved default branch")
	}
	if _, exists := payload["action"]; exists {
		return fmt.Errorf("gollum has no activity types")
	}
	pages, _ := payload["pages"].([]any)
	if len(pages) == 0 {
		return fmt.Errorf("gollum requires payload.pages")
	}
	for _, value := range pages {
		page, _ := value.(map[string]any)
		name, _ := page["page_name"].(string)
		action, _ := page["action"].(string)
		commit, _ := page["sha"].(string)
		if strings.TrimSpace(name) == "" || (action != "created" && action != "edited") || !git.ValidObjectID(commit) {
			return fmt.Errorf("gollum requires valid wiki page names, actions, and commits")
		}
	}
	return nil
}

func validateMilestoneEvent(provider string, repository Repository, ref, sha string, payload map[string]any) error {
	repo, _ := payload["repository"].(map[string]any)
	fullName, _ := repo["full_name"].(string)
	id, _ := repo["id"].(json.Number)
	number, err := id.Int64()
	if provider != "github" || err != nil || number <= 0 || !strings.EqualFold(fullName, repository.Owner+"/"+repository.Name) || !git.ValidObjectID(sha) {
		return fmt.Errorf("milestone requires the original repository identity and a full commit SHA")
	}
	if repository.DefaultBranch == "" || ref != "refs/heads/"+repository.DefaultBranch {
		return fmt.Errorf("milestone must execute the resolved default branch")
	}
	milestone, _ := payload["milestone"].(map[string]any)
	for _, key := range []string{"id", "number"} {
		id, _ = milestone[key].(json.Number)
		number, err = id.Int64()
		if err != nil || number <= 0 {
			return fmt.Errorf("milestone requires payload.milestone %s", key)
		}
	}
	if _, ok := milestone["title"].(string); !ok {
		return fmt.Errorf("milestone requires payload.milestone title")
	}
	action, _ := payload["action"].(string)
	if !buildkitepipeline.SupportedMilestoneAction(action) {
		return fmt.Errorf("milestone action must be created, closed, opened, edited, or deleted")
	}
	return nil
}

func validateWatchEvent(provider string, repository Repository, ref, sha string, payload map[string]any) error {
	repo, _ := payload["repository"].(map[string]any)
	fullName, _ := repo["full_name"].(string)
	id, _ := repo["id"].(json.Number)
	number, err := id.Int64()
	if provider != "github" || err != nil || number <= 0 || !strings.EqualFold(fullName, repository.Owner+"/"+repository.Name) || !git.ValidObjectID(sha) {
		return fmt.Errorf("watch requires the original repository identity and a full commit SHA")
	}
	if repository.DefaultBranch == "" || ref != "refs/heads/"+repository.DefaultBranch {
		return fmt.Errorf("watch must execute the resolved default branch")
	}
	if payload["action"] != "started" {
		return fmt.Errorf("watch action must be started")
	}
	return nil
}

func validatePageBuildEvent(provider string, repository Repository, ref, sha string, payload map[string]any) error {
	repo, _ := payload["repository"].(map[string]any)
	fullName, _ := repo["full_name"].(string)
	id, _ := repo["id"].(json.Number)
	number, err := id.Int64()
	if provider != "github" || err != nil || number <= 0 || !strings.EqualFold(fullName, repository.Owner+"/"+repository.Name) || !git.ValidObjectID(sha) {
		return fmt.Errorf("page_build requires the original repository identity and a full commit SHA")
	}
	if repository.DefaultBranch == "" || ref != "refs/heads/"+repository.DefaultBranch {
		return fmt.Errorf("page_build must execute the resolved default branch")
	}
	if _, exists := payload["action"]; exists {
		return fmt.Errorf("page_build has no activity types")
	}
	id, _ = payload["id"].(json.Number)
	number, err = id.Int64()
	build, _ := payload["build"].(map[string]any)
	commit, _ := build["commit"].(string)
	status, _ := build["status"].(string)
	if err != nil || number <= 0 || !git.ValidObjectID(commit) || strings.TrimSpace(status) == "" {
		return fmt.Errorf("page_build requires a build id, commit, and status")
	}
	return nil
}

func validatePublicEvent(provider string, repository Repository, ref, sha string, payload map[string]any) error {
	repo, _ := payload["repository"].(map[string]any)
	fullName, _ := repo["full_name"].(string)
	id, _ := repo["id"].(json.Number)
	number, err := id.Int64()
	if provider != "github" || err != nil || number <= 0 || !strings.EqualFold(fullName, repository.Owner+"/"+repository.Name) || !git.ValidObjectID(sha) {
		return fmt.Errorf("public requires the original repository identity and a full commit SHA")
	}
	if repository.DefaultBranch == "" || ref != "refs/heads/"+repository.DefaultBranch {
		return fmt.Errorf("public must execute the resolved default branch")
	}
	if repo["private"] != false {
		return fmt.Errorf("public requires a public repository payload")
	}
	if _, exists := payload["action"]; exists {
		return fmt.Errorf("public has no activity types")
	}
	return nil
}

func validateForkEvent(provider string, repository Repository, ref, sha string, payload map[string]any) error {
	repo, _ := payload["repository"].(map[string]any)
	fullName, _ := repo["full_name"].(string)
	id, _ := repo["id"].(json.Number)
	number, err := id.Int64()
	if provider != "github" || err != nil || number <= 0 || !strings.EqualFold(fullName, repository.Owner+"/"+repository.Name) || !git.ValidObjectID(sha) {
		return fmt.Errorf("fork requires the original repository identity and a full commit SHA")
	}
	if repository.DefaultBranch == "" || ref != "refs/heads/"+repository.DefaultBranch {
		return fmt.Errorf("fork must execute the resolved source default branch")
	}
	forkee, _ := payload["forkee"].(map[string]any)
	id, _ = forkee["id"].(json.Number)
	number, err = id.Int64()
	name, _ := forkee["full_name"].(string)
	if err != nil || number <= 0 || strings.TrimSpace(name) == "" {
		return fmt.Errorf("fork requires payload.forkee id and full_name")
	}
	if _, exists := payload["action"]; exists {
		return fmt.Errorf("fork has no activity types")
	}
	return nil
}

func validateLabelEvent(provider string, repository Repository, ref, sha string, payload map[string]any) error {
	repo, _ := payload["repository"].(map[string]any)
	fullName, _ := repo["full_name"].(string)
	id, _ := repo["id"].(json.Number)
	number, err := id.Int64()
	if provider != "github" || err != nil || number <= 0 || !strings.EqualFold(fullName, repository.Owner+"/"+repository.Name) || !git.ValidObjectID(sha) {
		return fmt.Errorf("label requires the original repository identity and a full commit SHA")
	}
	if repository.DefaultBranch == "" || ref != "refs/heads/"+repository.DefaultBranch {
		return fmt.Errorf("label must execute the resolved default branch")
	}
	label, _ := payload["label"].(map[string]any)
	id, _ = label["id"].(json.Number)
	number, err = id.Int64()
	_, named := label["name"].(string)
	if err != nil || number <= 0 || !named {
		return fmt.Errorf("label requires payload.label id and name")
	}
	if action := payload["action"]; action != "created" && action != "edited" && action != "deleted" {
		return fmt.Errorf("label action must be created, edited, or deleted")
	}
	return nil
}

func validateRefLifecycleEvent(provider, event string, repository Repository, ref, sha string, payload map[string]any) error {
	repo, _ := payload["repository"].(map[string]any)
	fullName, _ := repo["full_name"].(string)
	id, _ := repo["id"].(json.Number)
	number, err := id.Int64()
	if provider != "github" || err != nil || number <= 0 || !strings.EqualFold(fullName, repository.Owner+"/"+repository.Name) || !git.ValidObjectID(sha) {
		return fmt.Errorf("%s requires the original repository identity and a full commit SHA", event)
	}
	rawRef, _ := payload["ref"].(string)
	kind, _ := payload["ref_type"].(string)
	if rawRef == "" || strings.ContainsAny(rawRef, " \t\r\n") || (kind != "branch" && kind != "tag") {
		return fmt.Errorf("%s requires a branch or tag payload.ref and ref_type", event)
	}
	if _, exists := payload["action"]; exists {
		return fmt.Errorf("%s has no activity types", event)
	}
	if event == "delete" {
		if repository.DefaultBranch == "" || ref != "refs/heads/"+repository.DefaultBranch {
			return fmt.Errorf("delete must execute the resolved default branch, not the deleted ref")
		}
	} else if plan.EventRefName(ref) != rawRef || plan.EventRefType(event, ref) != kind || strings.HasPrefix(ref, "refs/pull/") {
		return fmt.Errorf("create ref must match the created branch or tag")
	}
	return nil
}

func validateDeploymentEvent(provider, event string, repository Repository, ref, sha string, payload map[string]any) error {
	positiveID := func(value any) bool {
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		id, err := number.Int64()
		return err == nil && id > 0
	}
	repo, _ := payload["repository"].(map[string]any)
	fullName, _ := repo["full_name"].(string)
	if provider != "github" || !positiveID(repo["id"]) || !strings.EqualFold(fullName, repository.Owner+"/"+repository.Name) {
		return fmt.Errorf("%s repository does not match the event snapshot", event)
	}
	if action, exists := payload["action"]; exists && action != "created" {
		return fmt.Errorf("%s payload.action must be created when present", event)
	}
	deployment, _ := payload["deployment"].(map[string]any)
	if !positiveID(deployment["id"]) || !git.ValidObjectID(sha) || deployment["sha"] != sha {
		return fmt.Errorf("%s requires deployment.id and deployment.sha matching the full snapshot SHA", event)
	}
	rawRef, ok := deployment["ref"].(string)
	if !ok || rawRef == "" || rawRef != strings.TrimSpace(rawRef) {
		return fmt.Errorf("%s requires deployment.ref", event)
	}
	if ref == "" {
		if rawRef != sha {
			return fmt.Errorf("%s empty ref requires a SHA-only deployment", event)
		}
	} else if plan.EventRefType(event, ref) == "" || plan.EventRefName(ref) == "" || strings.HasPrefix(ref, "refs/pull/") ||
		(ref != rawRef && plan.EventRefName(ref) != rawRef) {
		return fmt.Errorf("%s ref must match the deployment branch or tag", event)
	}
	if event == "deployment_status" {
		status, _ := payload["deployment_status"].(map[string]any)
		if !positiveID(status["id"]) {
			return fmt.Errorf("deployment_status requires deployment_status.id")
		}
		switch status["state"] {
		case "error", "failure", "in_progress", "queued", "pending", "success", "waiting":
		case "inactive":
			return fmt.Errorf("inactive deployment_status does not trigger GitHub Actions")
		default:
			return fmt.Errorf("deployment_status state is unsupported")
		}
	} else if _, exists := payload["deployment_status"]; exists {
		return fmt.Errorf("deployment_status payload does not match deployment event")
	}
	return nil
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
	if !git.ValidObjectID(headSHA) || !git.ValidObjectID(baseSHA) {
		return fmt.Errorf("merge_group event snapshot requires full lowercase payload.merge_group head and base SHAs")
	}
	if ref != headRef || sha != headSHA {
		return fmt.Errorf("merge_group event snapshot ref and sha must match payload.merge_group head_ref and head_sha")
	}
	return nil
}

func validateReleaseEvent(ref, sha string, payload map[string]any) error {
	action, _ := payload["action"].(string)
	if !buildkitepipeline.SupportedReleaseAction(action) {
		return fmt.Errorf("release event snapshot has unsupported payload.action %q", action)
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
	if draft && slices.Contains([]string{"created", "edited", "deleted", "unpublished"}, action) {
		return fmt.Errorf("release event snapshot draft %s activity does not trigger GitHub Actions", action)
	}
	if ref != "refs/tags/"+tag {
		return fmt.Errorf("release event snapshot ref must match payload.release.tag_name")
	}
	if !git.ValidObjectID(sha) {
		return fmt.Errorf("release event snapshot requires a full lowercase sha")
	}
	return nil
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
			"ref_type":         plan.EventRefType(event.Event, event.Ref),
			"sha":              event.SHA,
			"actor":            event.Actor,
			"workflow":         workflowName,
		},
		Event:          event.Payload,
		Vars:           vars,
		InputsComplete: true,
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
