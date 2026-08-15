package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/transport"
)

const (
	migrationManifestPrefix = "# buildkite-gha-migration: "
	maxMigrationFileBytes   = 1 << 20
	maxMigrationSecrets     = 40
)

var migrationGrantIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,200}$`)

type migrateSecretsPrepareOptions struct {
	organization string
	cluster      string
	pipeline     string
	policyFile   string
	secretNames  []string
	matches      []string
	output       string
}

type migrationManifest struct {
	Version           int      `json:"version"`
	Organization      string   `json:"organization"`
	Cluster           string   `json:"cluster"`
	Policy            string   `json:"policy"`
	Repository        string   `json:"repository"`
	RepositoryID      int64    `json:"repository_id"`
	RepositoryOwnerID int64    `json:"repository_owner_id"`
	DefaultBranch     string   `json:"default_branch"`
	SecretNames       []string `json:"secret_names"`
}

type githubRepository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		ID int64 `json:"id"`
	} `json:"owner"`
}

type buildkitePipeline struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	ClusterID  string `json:"cluster_id"`
	Repository string `json:"repository"`
}

func migrateSecrets(args []string, stdin io.Reader, stdout, stderr io.Writer, runner transport.Runner) int {
	if len(args) == 0 {
		return usageError(stderr, "migrate-secrets requires prepare or run")
	}
	ctx := context.Background()
	switch args[0] {
	case "prepare":
		options, err := parseMigrateSecretsPrepareArgs(args[1:])
		if err != nil {
			return usageError(stderr, "migrate-secrets prepare: %v", err)
		}
		if err := prepareSecretsMigration(ctx, options, bufio.NewReader(stdin), stdout, stderr, runner); err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: migrate-secrets prepare: %v\n", err)
			return 1
		}
		return 0
	case "run":
		workflowPath, err := parseMigrateSecretsRunArgs(args[1:])
		if err != nil {
			return usageError(stderr, "migrate-secrets run: %v", err)
		}
		if err := runSecretsMigration(ctx, workflowPath, stdout, runner); err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: migrate-secrets run: %v\n", err)
			return 1
		}
		return 0
	default:
		return usageError(stderr, "migrate-secrets: unknown subcommand %q", args[0])
	}
}

func parseMigrateSecretsPrepareArgs(args []string) (migrateSecretsPrepareOptions, error) {
	var options migrateSecretsPrepareOptions
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		repeatable := flag == "--secret" || flag == "--match"
		switch flag {
		case "--organization", "--cluster", "--pipeline", "--policy-file", "--secret", "--match", "--output":
		default:
			return options, fmt.Errorf("unknown option %q", flag)
		}
		if seen[flag] && !repeatable {
			return options, fmt.Errorf("%s may only be specified once", flag)
		}
		seen[flag] = true
		i++
		if i == len(args) || args[i] == "" {
			return options, fmt.Errorf("%s requires a non-empty value", flag)
		}
		switch flag {
		case "--organization":
			options.organization = args[i]
		case "--cluster":
			options.cluster = args[i]
		case "--pipeline":
			options.pipeline = args[i]
		case "--policy-file":
			options.policyFile = args[i]
		case "--secret":
			options.secretNames = append(options.secretNames, args[i])
		case "--match":
			if _, err := path.Match(args[i], "NAME"); err != nil {
				return options, fmt.Errorf("invalid --match glob %q: %w", args[i], err)
			}
			options.matches = append(options.matches, args[i])
		case "--output":
			options.output = args[i]
		}
	}
	if options.organization != "" && !organizationSlugPattern.MatchString(options.organization) {
		return options, fmt.Errorf("--organization must be a lowercase Buildkite organization slug")
	}
	if options.cluster != "" && !clusterIDPattern.MatchString(options.cluster) {
		return options, fmt.Errorf("--cluster must be a Buildkite cluster UUID")
	}
	if options.pipeline != "" && !organizationSlugPattern.MatchString(options.pipeline) {
		return options, fmt.Errorf("--pipeline must be a lowercase Buildkite pipeline slug")
	}
	if options.policyFile == "-" {
		return options, fmt.Errorf("--policy-file must be a file path, not stdin")
	}
	if options.output != "" {
		if err := validateMigrationWorkflowPath(options.output); err != nil {
			return options, fmt.Errorf("--output: %w", err)
		}
	}
	return options, nil
}

func prepareSecretsMigration(ctx context.Context, options migrateSecretsPrepareOptions, input *bufio.Reader, stdout, stderr io.Writer, runner transport.Runner) error {
	repository, err := inspectGitHubRepository(ctx, runner)
	if err != nil {
		return err
	}
	availableNames, err := listGitHubSecretNames(ctx, runner)
	if err != nil {
		return err
	}
	if len(availableNames) == 0 {
		return errors.New("the GitHub repository has no Actions secrets")
	}
	selectedNames, err := selectMigrationSecretNames(availableNames, options.secretNames, options.matches, input, stderr)
	if err != nil {
		return err
	}

	if options.organization == "" {
		options.organization, err = currentBuildkiteOrganization(ctx, runner)
		if err != nil {
			return err
		}
	}
	pipeline, err := selectBuildkitePipeline(ctx, runner, options.organization, options.pipeline, repository.FullName, input, stderr)
	if err != nil {
		return err
	}
	if !clusterIDPattern.MatchString(pipeline.ClusterID) {
		return fmt.Errorf("buildkite pipeline %s does not report a cluster UUID", pipeline.Slug)
	}
	if options.cluster != "" && options.cluster != pipeline.ClusterID {
		return fmt.Errorf("--cluster %s does not match pipeline %s cluster %s", options.cluster, pipeline.Slug, pipeline.ClusterID)
	}
	options.cluster = pipeline.ClusterID

	policy := "- pipeline_id: " + pipeline.ID + "\n"
	if options.policyFile != "" {
		policy, err = readSecretsPolicy(options.policyFile)
		if err != nil {
			return err
		}
	}
	if err := rejectExistingBuildkiteSecrets(ctx, runner, options.organization, options.cluster, selectedNames); err != nil {
		return err
	}

	manifest := migrationManifest{
		Version:           1,
		Organization:      options.organization,
		Cluster:           options.cluster,
		Policy:            policy,
		Repository:        repository.FullName,
		RepositoryID:      repository.ID,
		RepositoryOwnerID: repository.Owner.ID,
		DefaultBranch:     repository.DefaultBranch,
		SecretNames:       selectedNames,
	}
	workflow, err := renderOIDCSecretsMigrationWorkflow(manifest)
	if err != nil {
		return err
	}
	if options.output == "" {
		_, err = stdout.Write(workflow)
		return err
	}
	file, err := os.OpenFile(options.output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create workflow: %w", err)
	}
	_, writeErr := file.Write(workflow)
	writeErr = errors.Join(writeErr, file.Close())
	if writeErr != nil {
		_ = os.Remove(options.output)
		return fmt.Errorf("write workflow: %w", writeErr)
	}
	_, _ = fmt.Fprintf(stdout, "Wrote %s with %d explicitly named secrets. Review and commit it to %s, then run:\n  buildkite-gha migrate-secrets run --workflow %s\n", options.output, len(selectedNames), repository.DefaultBranch, options.output)
	return nil
}

func inspectGitHubRepository(ctx context.Context, runner transport.Runner) (githubRepository, error) {
	output, err := runner.Run(ctx, "", "gh", []string{"api", "repos/{owner}/{repo}"}, nil)
	if err != nil {
		return githubRepository{}, fmt.Errorf("inspect GitHub repository with gh: %w", err)
	}
	var repository githubRepository
	if err := json.Unmarshal(output, &repository); err != nil {
		return repository, fmt.Errorf("decode gh repository response: %w", err)
	}
	if repository.ID == 0 || repository.Owner.ID == 0 || repository.FullName == "" || repository.DefaultBranch == "" {
		return repository, errors.New("gh repository response is missing identity or default branch")
	}
	if !strings.EqualFold(strings.TrimSuffix(repository.HTMLURL, "/"), "https://github.com/"+repository.FullName) {
		return repository, errors.New("GitHub Enterprise Server repositories are not supported")
	}
	return repository, nil
}

func listGitHubSecretNames(ctx context.Context, runner transport.Runner) ([]string, error) {
	output, err := runner.Run(ctx, "", "gh", []string{"api", "--paginate", "--jq", ".secrets[].name", "repos/{owner}/{repo}/actions/secrets?per_page=100"}, nil)
	if err != nil {
		return nil, fmt.Errorf("list GitHub Actions secrets with gh: %w", err)
	}
	names := strings.Fields(string(output))
	for _, name := range names {
		if !githubSecretPattern.MatchString(name) {
			return nil, fmt.Errorf("gh returned invalid secret name %q", name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	return names, nil
}

func selectMigrationSecretNames(available, explicit, matches []string, input *bufio.Reader, prompt io.Writer) ([]string, error) {
	availableSet := make(map[string]bool, len(available))
	for _, name := range available {
		availableSet[strings.ToUpper(name)] = true
	}
	selected := map[string]bool{}
	for _, name := range explicit {
		name = strings.ToUpper(name)
		if !availableSet[name] {
			return nil, fmt.Errorf("GitHub Actions secret %q does not exist", name)
		}
		selected[name] = true
	}
	for _, pattern := range matches {
		matched := false
		for name := range availableSet {
			if ok, _ := path.Match(strings.ToUpper(pattern), name); ok {
				selected[name] = true
				matched = true
			}
		}
		if !matched {
			return nil, fmt.Errorf("--match %q selected no GitHub Actions secrets", pattern)
		}
	}
	if len(explicit) == 0 && len(matches) == 0 {
		_, _ = fmt.Fprintln(prompt, "GitHub Actions repository secrets:")
		for index, name := range available {
			_, _ = fmt.Fprintf(prompt, "  %d) %s\n", index+1, name)
		}
		_, _ = fmt.Fprint(prompt, "Select secrets by number (comma-separated), or type all: ")
		line, err := input.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read secret selection: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "all" {
			for name := range availableSet {
				selected[name] = true
			}
		} else {
			for _, field := range strings.Split(line, ",") {
				index, parseErr := strconv.Atoi(strings.TrimSpace(field))
				if parseErr != nil || index < 1 || index > len(available) {
					return nil, fmt.Errorf("invalid secret selection %q", strings.TrimSpace(field))
				}
				selected[strings.ToUpper(available[index-1])] = true
			}
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("at least one secret must be selected")
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		if err := validateMigrationSecretName(name); err != nil {
			return nil, err
		}
		if name == "GITHUB_TOKEN" {
			return nil, errors.New("GITHUB_TOKEN cannot be migrated; buildkite-gha obtains it through its separate permission-scoped workflow-token path")
		}
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) > maxMigrationSecrets {
		return nil, fmt.Errorf("at most %d secrets can be migrated at once; use multiple workflows for larger sets", maxMigrationSecrets)
	}
	return names, nil
}

func currentBuildkiteOrganization(ctx context.Context, runner transport.Runner) (string, error) {
	output, err := runner.Run(ctx, "", "bk", []string{"auth", "status", "--json", "--no-input"}, nil)
	if err != nil {
		return "", fmt.Errorf("inspect Buildkite authentication with bk: %w", err)
	}
	var status struct {
		Organization string `json:"organization_slug"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return "", fmt.Errorf("decode bk authentication response: %w", err)
	}
	if !organizationSlugPattern.MatchString(status.Organization) {
		return "", errors.New("bk has no current Buildkite organization; pass --organization or run bk auth switch")
	}
	return status.Organization, nil
}

func selectBuildkitePipeline(ctx context.Context, runner transport.Runner, organization, requested, repository string, input *bufio.Reader, prompt io.Writer) (buildkitePipeline, error) {
	if requested != "" {
		output, err := runner.Run(ctx, "", "bk", []string{"pipeline", "view", organization + "/" + requested, "--json", "--no-input"}, nil)
		if err != nil {
			return buildkitePipeline{}, fmt.Errorf("inspect Buildkite pipeline with bk: %w", err)
		}
		var pipeline buildkitePipeline
		if err := json.Unmarshal(output, &pipeline); err != nil {
			return pipeline, fmt.Errorf("decode bk pipeline response: %w", err)
		}
		if !clusterIDPattern.MatchString(pipeline.ID) || pipeline.Slug != requested {
			return pipeline, fmt.Errorf("bk returned an invalid pipeline for %q", requested)
		}
		return pipeline, nil
	}
	output, err := runner.Run(ctx, "", "bk", []string{"pipeline", "list", "--org", organization, "--repository", repository, "--limit", "3000", "--json", "--no-input"}, nil)
	if err != nil {
		return buildkitePipeline{}, fmt.Errorf("list Buildkite pipelines with bk: %w", err)
	}
	var pipelines []buildkitePipeline
	if err := json.Unmarshal(output, &pipelines); err != nil {
		return buildkitePipeline{}, fmt.Errorf("decode bk pipeline response: %w", err)
	}
	for index := len(pipelines) - 1; index >= 0; index-- {
		if !clusterIDPattern.MatchString(pipelines[index].ID) || pipelines[index].Slug == "" || !clusterIDPattern.MatchString(pipelines[index].ClusterID) || !buildkitePipelineMatchesGitHubRepository(pipelines[index].Repository, repository) {
			pipelines = slices.Delete(pipelines, index, index+1)
		}
	}
	if len(pipelines) == 0 {
		return buildkitePipeline{}, fmt.Errorf("no Buildkite pipeline found for repository %s; pass --pipeline", repository)
	}
	slices.SortFunc(pipelines, func(left, right buildkitePipeline) int { return strings.Compare(left.Slug, right.Slug) })
	if len(pipelines) == 1 {
		_, _ = fmt.Fprintf(prompt, "Using Buildkite pipeline %s.\n", pipelines[0].Slug)
		return pipelines[0], nil
	}
	_, _ = fmt.Fprintln(prompt, "Buildkite pipelines:")
	for index, pipeline := range pipelines {
		_, _ = fmt.Fprintf(prompt, "  %d) %s\n", index+1, pipeline.Slug)
	}
	_, _ = fmt.Fprint(prompt, "Select a pipeline by number: ")
	line, readErr := input.ReadString('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return buildkitePipeline{}, fmt.Errorf("read pipeline selection: %w", readErr)
	}
	index, parseErr := strconv.Atoi(strings.TrimSpace(line))
	if parseErr != nil || index < 1 || index > len(pipelines) {
		return buildkitePipeline{}, fmt.Errorf("invalid pipeline selection %q", strings.TrimSpace(line))
	}
	return pipelines[index-1], nil
}

func buildkitePipelineMatchesGitHubRepository(pipelineRepository, repository string) bool {
	normalized := strings.TrimSuffix(pipelineRepository, ".git")
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		if strings.HasPrefix(normalized, prefix) {
			return strings.EqualFold(strings.TrimPrefix(normalized, prefix), repository)
		}
	}
	return false
}

func rejectExistingBuildkiteSecrets(ctx context.Context, runner transport.Runner, organization, cluster string, selected []string) error {
	var secrets []struct {
		Key string `json:"key"`
	}
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("/organizations/%s/clusters/%s/secrets?per_page=100&page=%d", organization, cluster, page)
		output, err := runner.Run(ctx, "", "bk", []string{"api", endpoint, "--no-input"}, nil)
		if err != nil {
			return fmt.Errorf("list Buildkite secrets with bk: %w", err)
		}
		var batch []struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(output, &batch); err != nil {
			return fmt.Errorf("decode bk secrets response: %w", err)
		}
		secrets = append(secrets, batch...)
		if len(batch) < 100 {
			break
		}
	}
	existing := map[string]bool{}
	for _, secret := range secrets {
		existing[strings.ToUpper(secret.Key)] = true
	}
	var conflicts []string
	for _, name := range selected {
		if existing[name] {
			conflicts = append(conflicts, name)
		}
	}
	if len(conflicts) != 0 {
		return fmt.Errorf("destination keys already exist and will not be overwritten: %s", strings.Join(conflicts, ", "))
	}
	return nil
}

func validateMigrationWorkflowPath(workflowPath string) error {
	clean := filepath.ToSlash(filepath.Clean(workflowPath))
	if clean != workflowPath || !strings.HasPrefix(clean, ".github/workflows/") || strings.Count(strings.TrimPrefix(clean, ".github/workflows/"), "/") != 0 {
		return errors.New("workflow must be a clean path directly under .github/workflows")
	}
	extension := filepath.Ext(clean)
	if extension != ".yml" && extension != ".yaml" {
		return errors.New("workflow must have a .yml or .yaml extension")
	}
	return nil
}

func encodeMigrationManifest(manifest migrationManifest) (string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeMigrationManifest(workflow []byte) (migrationManifest, error) {
	line, _, found := bytes.Cut(workflow, []byte("\n"))
	if !found || !bytes.HasPrefix(line, []byte(migrationManifestPrefix)) {
		return migrationManifest{}, errors.New("workflow was not generated by migrate-secrets prepare")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(string(line), migrationManifestPrefix))
	if err != nil {
		return migrationManifest{}, errors.New("workflow has an invalid migration manifest")
	}
	var manifest migrationManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, errors.New("workflow has an invalid migration manifest")
	}
	if manifest.Version != 1 || !organizationSlugPattern.MatchString(manifest.Organization) || !clusterIDPattern.MatchString(manifest.Cluster) || manifest.RepositoryID == 0 || manifest.RepositoryOwnerID == 0 || manifest.Repository == "" || manifest.DefaultBranch == "" || len(manifest.SecretNames) == 0 {
		return manifest, errors.New("workflow migration manifest is incomplete")
	}
	policy, err := validateSecretsPolicy(manifest.Policy)
	if err != nil || policy != manifest.Policy {
		return manifest, errors.New("workflow migration manifest has an invalid access policy")
	}
	for index, name := range manifest.SecretNames {
		if err := validateMigrationSecretName(name); err != nil || name == "GITHUB_TOKEN" || (index > 0 && name <= manifest.SecretNames[index-1]) {
			return manifest, errors.New("workflow migration manifest has an invalid secret allowlist")
		}
	}
	return manifest, nil
}

func renderOIDCSecretsMigrationWorkflow(manifest migrationManifest) ([]byte, error) {
	encodedManifest, err := encodeMigrationManifest(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode migration manifest: %w", err)
	}
	var output strings.Builder
	fmt.Fprintf(&output, `%s%s
# One-use GitHub-to-Buildkite secrets migration.
# Source: %s (repository ID %d, owner ID %d)
# Destination: %s cluster %s
# Access policy:
`, migrationManifestPrefix, encodedManifest, manifest.Repository, manifest.RepositoryID, manifest.RepositoryOwnerID, manifest.Organization, manifest.Cluster)
	for line := range strings.SplitSeq(strings.TrimSuffix(manifest.Policy, "\n"), "\n") {
		fmt.Fprintf(&output, "#   %s\n", line)
	}
	fmt.Fprintf(&output, `#
# Review and commit this file on %s. After a successful run, remove it.
name: Migrate GitHub secrets to Buildkite

on:
  workflow_dispatch:
    inputs:
      grant_id:
        description: One-use Buildkite migration grant
        required: true
        type: string

permissions:
  id-token: write

jobs:
  migrate:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    env:
      DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}
      GRANT_ID: ${{ inputs.grant_id }}
`, manifest.DefaultBranch)
	for index, name := range manifest.SecretNames {
		fmt.Fprintf(&output, "      MIGRATION_SECRET_%03d: ${{ secrets.%s }}\n", index, name)
	}
	output.WriteString(`    steps:
      - name: Create Buildkite secrets
        shell: bash
        run: |
          set +x
          python3 - <<'PY'
          import json
          import os
          import re
          import sys
          import urllib.error
          import urllib.parse
          import urllib.request

          class NoRedirects(urllib.request.HTTPRedirectHandler):
              def redirect_request(self, request, file_pointer, code, message, headers, new_url):
                  return None

`)
	output.WriteString("          secrets = [\n")
	for index, name := range manifest.SecretNames {
		fmt.Fprintf(&output, "              (%q, %q),\n", name, fmt.Sprintf("MIGRATION_SECRET_%03d", index))
	}
	output.WriteString("          ]\n")
	migrationURLPrefix := fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/clusters/%s/github-actions-secret-migrations/", manifest.Organization, manifest.Cluster)
	fmt.Fprintf(&output, "          migration_url_prefix = %q\n", migrationURLPrefix)
	output.WriteString(`
          if os.environ.get("GITHUB_REF") != "refs/heads/" + os.environ.get("DEFAULT_BRANCH", ""):
              sys.exit("Run this migration from the repository default branch")
          grant_id = os.environ.get("GRANT_ID", "")
          if not re.fullmatch(r"[A-Za-z0-9_-]{16,200}", grant_id):
              sys.exit("The migration grant ID is invalid")

          values = {}
          for key, variable in secrets:
              value = os.environ.get(variable, "")
              size = len(value.encode("utf-8"))
              if size == 0:
                  sys.exit(f"GitHub secret {key} is missing or empty; no Buildkite secrets were created")
              if size >= 32 * 1024:
                  sys.exit(f"GitHub secret {key} is too large for the Buildkite API; no Buildkite secrets were created")
              values[key] = value

          migration_url = migration_url_prefix + grant_id + "/secrets"
          audience = migration_url
          token_url = os.environ.get("ACTIONS_ID_TOKEN_REQUEST_URL", "")
          request_token = os.environ.get("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
          if not token_url or not request_token:
              sys.exit("GitHub OIDC is unavailable")
          separator = "&" if "?" in token_url else "?"
          oidc_request = urllib.request.Request(
              token_url + separator + urllib.parse.urlencode({"audience": audience}),
              headers={"Authorization": f"Bearer {request_token}", "Accept": "application/json"},
          )
          opener = urllib.request.build_opener(NoRedirects)
          try:
              with opener.open(oidc_request, timeout=30) as response:
                  oidc_token = json.load(response).get("value", "")
          except Exception:
              sys.exit("Could not obtain GitHub OIDC identity")
          if not oidc_token:
              sys.exit("GitHub returned an empty OIDC identity")

          body = json.dumps({"secrets": values}, ensure_ascii=False).encode("utf-8")
          request = urllib.request.Request(
              migration_url,
              data=body,
              method="POST",
              headers={
                  "Authorization": f"Bearer {oidc_token}",
                  "Content-Type": "application/json",
                  "Accept": "application/json",
              },
          )
          try:
              with opener.open(request, timeout=30) as response:
                  if response.status != 201:
                      raise RuntimeError("unexpected response")
          except urllib.error.HTTPError as error:
              sys.exit(f"Buildkite rejected the migration with HTTP {error.code}; no existing value was overwritten")
          except Exception:
              sys.exit("The Buildkite migration request failed")

          for key in values:
              print(f"Created Buildkite secret {key}")
          print("Migration complete. Remove this workflow from the default branch.")
          PY
`)
	return []byte(output.String()), nil
}

func parseMigrateSecretsRunArgs(args []string) (string, error) {
	if len(args) != 2 || args[0] != "--workflow" || args[1] == "" {
		return "", errors.New("run requires --workflow <path>")
	}
	if err := validateMigrationWorkflowPath(args[1]); err != nil {
		return "", fmt.Errorf("--workflow: %w", err)
	}
	return args[1], nil
}

func runSecretsMigration(ctx context.Context, workflowPath string, stdout io.Writer, runner transport.Runner) error {
	file, err := os.Open(workflowPath)
	if err != nil {
		return fmt.Errorf("read workflow: %w", err)
	}
	workflow, readErr := io.ReadAll(io.LimitReader(file, maxMigrationFileBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return fmt.Errorf("read workflow: %w", err)
	}
	if len(workflow) > maxMigrationFileBytes {
		return fmt.Errorf("workflow exceeds %d bytes", maxMigrationFileBytes)
	}
	manifest, err := decodeMigrationManifest(workflow)
	if err != nil {
		return err
	}
	expectedWorkflow, err := renderOIDCSecretsMigrationWorkflow(manifest)
	if err != nil || !bytes.Equal(workflow, expectedWorkflow) {
		return errors.New("workflow differs from the deterministic migrate-secrets output")
	}
	repository, err := inspectGitHubRepository(ctx, runner)
	if err != nil {
		return err
	}
	if repository.ID != manifest.RepositoryID || repository.Owner.ID != manifest.RepositoryOwnerID || repository.FullName != manifest.Repository || repository.DefaultBranch != manifest.DefaultBranch {
		return errors.New("workflow migration manifest does not match the current GitHub repository")
	}
	remoteWorkflow, commitSHA, err := readDefaultBranchWorkflow(ctx, runner, repository, workflowPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(workflow, remoteWorkflow) {
		return errors.New("local workflow differs from the file on the GitHub default branch")
	}

	request := struct {
		Policy            string   `json:"policy"`
		SecretNames       []string `json:"secret_names"`
		RepositoryID      int64    `json:"repository_id"`
		RepositoryOwnerID int64    `json:"repository_owner_id"`
		WorkflowPath      string   `json:"workflow_path"`
		DefaultBranchRef  string   `json:"default_branch_ref"`
		WorkflowSHA       string   `json:"workflow_sha"`
	}{
		Policy:            manifest.Policy,
		SecretNames:       manifest.SecretNames,
		RepositoryID:      manifest.RepositoryID,
		RepositoryOwnerID: manifest.RepositoryOwnerID,
		WorkflowPath:      workflowPath,
		DefaultBranchRef:  "refs/heads/" + manifest.DefaultBranch,
		WorkflowSHA:       commitSHA,
	}
	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode migration grant: %w", err)
	}
	endpoint := fmt.Sprintf("/organizations/%s/clusters/%s/github-actions-secret-migrations", manifest.Organization, manifest.Cluster)
	output, err := runner.Run(ctx, "", "bk", []string{"api", endpoint, "--method", "POST", "--data", string(data), "--no-input"}, nil)
	if err != nil {
		return fmt.Errorf("create Buildkite migration grant with bk: %w", err)
	}
	var grant struct {
		ID           string `json:"id"`
		MigrationURL string `json:"migration_url"`
		Audience     string `json:"audience"`
	}
	if err := json.Unmarshal(output, &grant); err != nil || grant.ID == "" {
		return errors.New("buildkite returned an invalid migration grant")
	}
	if !migrationGrantIDPattern.MatchString(grant.ID) {
		return errors.New("buildkite returned an invalid migration grant")
	}
	expectedURL := fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/clusters/%s/github-actions-secret-migrations/%s/secrets", manifest.Organization, manifest.Cluster, grant.ID)
	if grant.MigrationURL != expectedURL || grant.Audience != expectedURL {
		return errors.New("buildkite returned an invalid migration grant URL or audience")
	}
	dispatchInput, _ := json.Marshal(map[string]string{"grant_id": grant.ID})
	_, err = runner.Run(ctx, "", "gh", []string{"workflow", "run", workflowPath, "--repo", manifest.Repository, "--ref", manifest.DefaultBranch, "--json"}, dispatchInput)
	if err != nil {
		return fmt.Errorf("dispatch migration workflow with gh: %w; grant %s will expire unused", err, grant.ID)
	}
	_, _ = fmt.Fprintf(stdout, "Dispatched %s on %s for %d secrets. Remove the workflow after the run succeeds.\n", workflowPath, manifest.DefaultBranch, len(manifest.SecretNames))
	return nil
}

func readDefaultBranchWorkflow(ctx context.Context, runner transport.Runner, repository githubRepository, workflowPath string) ([]byte, string, error) {
	commitEndpoint := fmt.Sprintf("repos/%s/commits/%s", repository.FullName, url.PathEscape(repository.DefaultBranch))
	commitOutput, err := runner.Run(ctx, "", "gh", []string{"api", "--jq", ".sha", commitEndpoint}, nil)
	if err != nil {
		return nil, "", fmt.Errorf("resolve GitHub default-branch commit with gh: %w", err)
	}
	commitSHA := strings.TrimSpace(string(commitOutput))
	if len(commitSHA) != 40 {
		return nil, "", errors.New("GitHub returned an invalid default-branch commit")
	}
	workflowName := strings.TrimPrefix(workflowPath, ".github/workflows/")
	endpoint := fmt.Sprintf("repos/%s/contents/.github/workflows/%s?ref=%s", repository.FullName, url.PathEscape(workflowName), url.QueryEscape(commitSHA))
	output, err := runner.Run(ctx, "", "gh", []string{"api", endpoint}, nil)
	if err != nil {
		return nil, "", fmt.Errorf("read workflow from GitHub default branch with gh: %w", err)
	}
	var content struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(output, &content); err != nil || content.Encoding != "base64" {
		return nil, "", errors.New("GitHub returned an invalid workflow file")
	}
	workflow, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
	if err != nil {
		return nil, "", errors.New("GitHub returned an invalid workflow file")
	}
	return workflow, commitSHA, nil
}
