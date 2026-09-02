package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
)

// A tally interprets the corpus validation reports and aggregates them into
// deterministic repository outcomes, diagnostic counts, console summaries,
// and JSON output. main.go owns corpus acquisition and materialization plus
// sampling and validator orchestration.
var supportedEvents = map[string]bool{
	"push":              true,
	"pull_request":      true,
	"merge_group":       true,
	"release":           true,
	"issues":            true,
	"workflow_dispatch": true,
	"schedule":          true,
}

var supportedWorkflowResults = map[string]bool{
	"admitted":         true,
	"not-applicable":   true,
	"context-required": true,
	"incompatible":     true,
	"not-admitted":     true,
	"indeterminate":    true,
}

type sampleMetadata struct {
	Key  string `json:"key"`
	Seed string `json:"seed"`
	Size int    `json:"size"`
}

type tallyOutput struct {
	AdmissionScope       string          `json:"admission_scope"`
	ByFinding            map[string]int  `json:"by_finding"`
	ByRepo               map[string]int  `json:"by_repo"`
	CompatibleRepos      int             `json:"compatible_repos"`
	ContextRequiredRepos int             `json:"context_required_repos"`
	Evaluations          map[string]int  `json:"evaluations"`
	IncompatibleRepos    int             `json:"incompatible_repos"`
	IndeterminateRepos   int             `json:"indeterminate_repos"`
	MeasuredRepos        int             `json:"measured_repos"`
	NotAdmittedRepos     int             `json:"not_admitted_repos"`
	Repos                int             `json:"repos"`
	Sample               *sampleMetadata `json:"sample,omitempty"`
	UnparseableReports   int             `json:"unparseable_reports"`
	ValidatorCommit      string          `json:"validator_commit"`
	ValidatorDigest      string          `json:"validator_digest"`
	ValidatorVersion     string          `json:"validator_version"`
	WorkflowResults      map[string]int  `json:"workflow_results"`
}

type tallyState struct {
	resultsByRepo   map[string]map[string]bool
	codesByRepo     map[string]map[string]bool
	findings        map[string]int
	evaluations     map[string]int
	workflowResults map[string]int
	seenWorkflows   map[string]bool
	badReports      int
}

func newTallyState() *tallyState {
	return &tallyState{
		resultsByRepo: map[string]map[string]bool{}, codesByRepo: map[string]map[string]bool{}, findings: map[string]int{},
		evaluations: map[string]int{}, workflowResults: map[string]int{}, seenWorkflows: map[string]bool{},
	}
}

func (state *tallyState) reportError(repository string) {
	state.badReports++
	state.findings["E_REPORT"]++
	if repository == "" {
		return
	}
	state.addResult(repository, "indeterminate")
	state.addCode(repository, "E_REPORT")
}

func (state *tallyState) addResult(repository, result string) {
	if state.resultsByRepo[repository] == nil {
		state.resultsByRepo[repository] = map[string]bool{}
	}
	state.resultsByRepo[repository][result] = true
}

func (state *tallyState) addCode(repository, code string) {
	if state.codesByRepo[repository] == nil {
		state.codesByRepo[repository] = map[string]bool{}
	}
	state.codesByRepo[repository][code] = true
}

func tallyReports(manifestPath, reports, tallyPath string, validator validatorInfo, sampleKey, sampleSeed string, sampleSize int, stdout io.Writer) error {
	records, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}
	repositories := map[string]string{}
	for _, record := range records {
		repositories[record.Source] = record.Repository
	}
	state := newTallyState()
	err = filepath.WalkDir(reports, func(reportPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(reportPath) != ".json" {
			return nil
		}
		contents, err := os.ReadFile(reportPath)
		if err != nil {
			return nil
		}
		var report compatibility.ProcessingReportV3
		if json.Unmarshal(contents, &report) != nil {
			return nil
		}
		var shape struct {
			Validation  json.RawMessage `json:"validation"`
			Evaluations json.RawMessage `json:"evaluations"`
		}
		if json.Unmarshal(contents, &shape) != nil {
			return nil
		}
		repository, known := repositories[report.Workflow]
		if !known || state.seenWorkflows[report.Workflow] {
			state.reportError("")
			return nil
		}
		state.seenWorkflows[report.Workflow] = true
		validShape := jsonKind(shape.Validation) == '{' && jsonKind(shape.Evaluations) == '['
		state.inspectReport(report, repository, validShape)
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan reports: %w", err)
	}
	for workflow, repository := range repositories {
		if !state.seenWorkflows[workflow] {
			state.reportError(repository)
			state.workflowResults["missing"]++
		}
	}

	allRepos := map[string]bool{}
	for _, repository := range repositories {
		allRepos[repository] = true
	}
	repoResults := map[string]int{}
	for repository := range allRepos {
		outcomes := state.resultsByRepo[repository]
		switch {
		case outcomes["incompatible"]:
			repoResults["incompatible"]++
		case outcomes["not-admitted"]:
			repoResults["not-admitted"]++
		case outcomes["indeterminate"]:
			repoResults["indeterminate"]++
		case outcomes["context-required"]:
			repoResults["context-required"]++
		default:
			repoResults["compatible"]++
		}
	}
	measured := repoResults["compatible"] + repoResults["incompatible"] + repoResults["not-admitted"]
	compatiblePercent := 0.0
	if measured != 0 {
		compatiblePercent = 100 * float64(repoResults["compatible"]) / float64(measured)
	}
	if _, err := fmt.Fprintf(stdout,
		"repos: %s   measured: %s   compatible: %s (%.2f%% of measured)   context required: %s   indeterminate: %s   incompatible: %s   not admitted: %s   unparseable reports: %s\n",
		comma(len(allRepos)), comma(measured), comma(repoResults["compatible"]), compatiblePercent, comma(repoResults["context-required"]),
		comma(repoResults["indeterminate"]), comma(repoResults["incompatible"]), comma(repoResults["not-admitted"]), comma(state.badReports),
	); err != nil {
		return fmt.Errorf("write repository summary: %w", err)
	}
	if _, err := fmt.Fprintln(stdout, "admission scope: generated event snapshots, not arbitrary real payloads"); err != nil {
		return fmt.Errorf("write admission scope: %w", err)
	}
	workflowResultNames := make([]string, 0, len(state.workflowResults))
	for result := range state.workflowResults {
		workflowResultNames = append(workflowResultNames, result)
	}
	slices.Sort(workflowResultNames)
	workflowResultSummary := make([]string, 0, len(workflowResultNames))
	for _, result := range workflowResultNames {
		workflowResultSummary = append(workflowResultSummary, fmt.Sprintf("%s=%s", result, comma(state.workflowResults[result])))
	}
	if _, err := fmt.Fprintf(stdout, "workflow results: %s\n", strings.Join(workflowResultSummary, ", ")); err != nil {
		return fmt.Errorf("write workflow result summary: %w", err)
	}
	if _, err := fmt.Fprintln(stdout, "\ndiagnostic code -> repos affected"); err != nil {
		return fmt.Errorf("write diagnostic heading: %w", err)
	}
	byRepo := map[string]int{}
	for _, codes := range state.codesByRepo {
		for code := range codes {
			byRepo[code]++
		}
	}
	type codeCount struct {
		code  string
		count int
	}
	counts := make([]codeCount, 0, len(byRepo))
	for code, count := range byRepo {
		counts = append(counts, codeCount{code: code, count: count})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].count != counts[j].count {
			return counts[i].count > counts[j].count
		}
		return counts[i].code < counts[j].code
	})
	for index, item := range counts {
		if index == 40 {
			break
		}
		if _, err := fmt.Fprintf(stdout, "%6.2f%%  %6s  %s\n", 100*float64(item.count)/float64(len(allRepos)), comma(item.count), item.code); err != nil {
			return fmt.Errorf("write diagnostic summary: %w", err)
		}
	}

	output := tallyOutput{
		AdmissionScope: "generated-event-snapshots", ByFinding: state.findings, ByRepo: byRepo,
		CompatibleRepos: repoResults["compatible"], ContextRequiredRepos: repoResults["context-required"], Evaluations: state.evaluations,
		IncompatibleRepos: repoResults["incompatible"], IndeterminateRepos: repoResults["indeterminate"], MeasuredRepos: measured,
		NotAdmittedRepos: repoResults["not-admitted"], Repos: len(allRepos), UnparseableReports: state.badReports,
		ValidatorCommit: validator.commit, ValidatorDigest: "sha256:" + validator.digest, ValidatorVersion: validator.version,
		WorkflowResults: state.workflowResults,
	}
	if sampleKey != "full" {
		output.Sample = &sampleMetadata{Key: sampleKey, Seed: sampleSeed, Size: sampleSize}
	}
	return writeTally(tallyPath, output)
}

func jsonKind(value json.RawMessage) byte {
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		return 0
	}
	return value[0]
}

func (state *tallyState) inspectReport(report compatibility.ProcessingReportV3, repository string, validShape bool) {
	if report.Schema != compatibility.ProcessingSchemaV3 || report.Profile != "hosted" || !validShape {
		state.reportError(repository)
		state.workflowResults["invalid-report"]++
		return
	}
	workflowResult := report.Result
	if workflowResult == "" {
		state.workflowResults["missing"]++
	} else {
		state.workflowResults[workflowResult]++
	}
	if supportedWorkflowResults[workflowResult] {
		state.addResult(repository, workflowResult)
	} else {
		state.addResult(repository, "indeterminate")
	}
	reports := []compatibility.ProcessingReport{report.Validation}
	seenEvents := map[string]bool{}
	for _, evaluation := range report.Evaluations {
		if !supportedEvents[evaluation.Event] || seenEvents[evaluation.Event] || evaluation.Source != "generated" {
			state.reportError(repository)
			return
		}
		seenEvents[evaluation.Event] = true
		state.evaluations[evaluation.Event]++
		reports = append(reports, evaluation.Report)
	}
	for _, child := range reports {
		if child.Schema != compatibility.ProcessingSchema {
			state.reportError(repository)
			break
		}
	}
	for _, child := range reports {
		for _, diagnostic := range child.Diagnostics {
			code := diagnostic.Code
			if code == "" {
				code = "unknown"
			}
			state.addCode(repository, code)
			state.findings[code]++
		}
	}
}

func writeTally(path string, tally tallyOutput) error {
	destination, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create tally: %w", err)
	}
	encoder := json.NewEncoder(destination)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(tally); err != nil {
		_ = destination.Close()
		return fmt.Errorf("write tally: %w", err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close tally: %w", err)
	}
	return nil
}
