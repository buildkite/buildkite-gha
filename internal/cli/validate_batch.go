package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
)

const batchResultIdentity = "buildkite-gha-corpus-result/v1"

type batchValidationArgs struct {
	manifest, outputDir, corpusID, actionCacheDir string
	jobs                                          int
}

type batchValidationRecord struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Hash       string `json:"hash"`
	Source     string `json:"source"`
}

type synchronizedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *synchronizedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(data)
}

func validateBatch(args []string, stderr io.Writer, version string) int {
	options, err := parseBatchValidationArgs(args)
	if err != nil {
		return usageError(stderr, "validate-batch: %v", err)
	}
	records, err := loadBatchValidationManifest(options.manifest)
	if err != nil {
		return usageError(stderr, "validate-batch: %v", err)
	}
	if options.actionCacheDir != "" {
		if _, err := actionsource.NewStore(options.actionCacheDir, nil); err != nil {
			return usageError(stderr, "validate-batch: --action-cache-dir: %v", err)
		}
	}
	actionSource, cleanup, err := newHostedActionSource(options.actionCacheDir, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate-batch: %v\n", err)
		return 1
	}
	defer cleanup()
	_, _, distributionDigest, err := executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate-batch: inspect validator executable: %v\n", err)
		return 1
	}
	runtime := &profileValidationRuntime{actionSource: actionSource, distributionDigest: distributionDigest}
	workerStderr := &synchronizedWriter{w: stderr}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer stopSignals()
	work := make(chan batchValidationRecord)
	failures := make(chan error, 1)
	var completed atomic.Int64
	var resumed atomic.Int64
	var workers sync.WaitGroup
	for range options.jobs {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for record := range work {
				if ctx.Err() != nil {
					return
				}
				resultPath := batchValidationResultPath(options, record, distributionDigest)
				if validBatchValidationResult(resultPath, record.Source) {
					resumed.Add(1)
					continue
				}
				if err := writeBatchValidationResult(resultPath, record, version, options.actionCacheDir, runtime, workerStderr); err != nil {
					select {
					case failures <- fmt.Errorf("%s: %w", record.ID, err):
						cancel()
					default:
					}
					return
				}
				completed.Add(1)
			}
		}()
	}
sendRecords:
	for _, record := range records {
		select {
		case <-ctx.Done():
			break sendRecords
		case work <- record:
		}
	}
	close(work)
	workers.Wait()
	select {
	case err := <-failures:
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate-batch: %v\n", err)
		return 1
	default:
	}
	if err := ctx.Err(); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate-batch: interrupted: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate-batch: wrote %d reports; resumed %d\n", completed.Load(), resumed.Load())
	return 0
}

func parseBatchValidationArgs(args []string) (batchValidationArgs, error) {
	options := batchValidationArgs{jobs: runtime.NumCPU()}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		name := args[i]
		if name != "--manifest" && name != "--output-dir" && name != "--corpus-id" && name != "--action-cache-dir" && name != "--jobs" {
			return options, fmt.Errorf("unknown option %q", name)
		}
		if seen[name] {
			return options, fmt.Errorf("%s may only be specified once", name)
		}
		seen[name] = true
		i++
		if i == len(args) || strings.TrimSpace(args[i]) == "" {
			return options, fmt.Errorf("%s requires a value", name)
		}
		switch name {
		case "--manifest":
			options.manifest = args[i]
		case "--output-dir":
			options.outputDir = args[i]
		case "--corpus-id":
			options.corpusID = args[i]
		case "--action-cache-dir":
			options.actionCacheDir = args[i]
		case "--jobs":
			jobs, parseErr := strconv.Atoi(args[i])
			if parseErr != nil || jobs <= 0 {
				return options, fmt.Errorf("--jobs must be a positive integer")
			}
			options.jobs = jobs
		}
	}
	if options.manifest == "" || options.outputDir == "" || options.corpusID == "" {
		return options, fmt.Errorf("--manifest, --output-dir, and --corpus-id are required")
	}
	return options, nil
}

func loadBatchValidationManifest(path string) ([]batchValidationRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer func() { _ = file.Close() }()
	var records []batchValidationRecord
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		var record batchValidationRecord
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			return nil, fmt.Errorf("manifest line %d: %w", line, err)
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return nil, fmt.Errorf("manifest line %d contains multiple JSON values", line)
		}
		if record.ID == "" || record.Repository == "" || record.Path == "" || record.Hash == "" || record.Source == "" {
			return nil, fmt.Errorf("manifest line %d requires non-empty id, repository, path, hash, and source", line)
		}
		if strings.ContainsRune(record.ID+record.Repository+record.Path+record.Hash, '\x00') {
			return nil, fmt.Errorf("manifest line %d identity fields must not contain NUL", line)
		}
		if seen[record.ID] {
			return nil, fmt.Errorf("manifest line %d repeats id %q", line, record.ID)
		}
		seen[record.ID] = true
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("manifest contains no records")
	}
	return records, nil
}

func batchValidationResultPath(options batchValidationArgs, record batchValidationRecord, validatorDigest string) string {
	identity := strings.Join([]string{batchResultIdentity, options.corpusID, validatorDigest, record.ID, record.Repository, record.Path, record.Hash}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	key := hex.EncodeToString(digest[:])
	return filepath.Join(options.outputDir, key[:2], key+".json")
}

func validBatchValidationResult(path, workflow string) bool {
	report, valid := loadBatchValidationResult(path, workflow)
	if !valid || report.Validation.Result == "indeterminate" {
		return false
	}
	for _, evaluation := range report.Evaluations {
		if evaluation.Report.Result == "indeterminate" {
			return false
		}
	}
	return true
}

func validBatchProcessingReport(path, workflow string) bool {
	_, valid := loadBatchValidationResult(path, workflow)
	return valid
}

func loadBatchValidationResult(path, workflow string) (compatibility.ProcessingReportV3, bool) {
	file, err := os.Open(path)
	if err != nil {
		return compatibility.ProcessingReportV3{}, false
	}
	defer func() { _ = file.Close() }()
	var report compatibility.ProcessingReportV3
	decoder := json.NewDecoder(file)
	if decoder.Decode(&report) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return compatibility.ProcessingReportV3{}, false
	}
	if report.Schema != compatibility.ProcessingSchemaV3 || report.Profile != hostedProfile || report.Workflow != workflow ||
		report.Validation.Schema != compatibility.ProcessingSchema {
		return compatibility.ProcessingReportV3{}, false
	}
	seen := make(map[string]bool, len(report.Evaluations))
	for _, evaluation := range report.Evaluations {
		if seen[evaluation.Event] || evaluation.Source != "generated" || evaluation.Report.Schema != compatibility.ProcessingSchema ||
			(evaluation.Event != "push" && evaluation.Event != "pull_request" && evaluation.Event != "workflow_dispatch" && evaluation.Event != "schedule") {
			return compatibility.ProcessingReportV3{}, false
		}
		seen[evaluation.Event] = true
	}
	return report, true
}

func writeBatchValidationResult(path string, record batchValidationRecord, version, actionCacheDir string, runtime *profileValidationRuntime, stderr io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".report-*.tmp")
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	out := processingOutput{command: "validate-batch", format: "json", reports: temporary, stderr: stderr}
	_ = validateAllEvents(out, record.Source, version, actionCacheDir, runtime, stderr)
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync report: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close report: %w", err)
	}
	if !validBatchProcessingReport(temporaryPath, record.Source) {
		return fmt.Errorf("validation did not produce a valid processing report")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	return nil
}
