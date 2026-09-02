package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	defaultRecordID            = "20340547"
	defaultActionCacheMaxBytes = "21474836480"
	sampleIdentityVersion      = "buildkite-gha-public-workflow-sample-v1\x00"
)

type config struct {
	root, workdir, recordID, sampleSeed string
	sampleSize                          int
	refresh                             bool
}

type corpusRow struct {
	committed                    int64
	repository, path, hash       string
	validWorkflow, gitChangeType string
}

type corpusWorkflow struct {
	repository, path, hash string
}

type workflowDestination struct {
	id, filename string
}

type manifestRecord struct {
	Hash       string `json:"hash"`
	ID         string `json:"id"`
	Path       string `json:"path"`
	Repository string `json:"repository"`
	Source     string `json:"source"`
}

type rankedRecord struct {
	rank   [sha256.Size]byte
	record manifestRecord
}

type validatorInfo struct {
	path, commit, version, digest string
}

type statusError struct {
	code    int
	message string
}

func (e *statusError) Error() string { return e.message }

func main() {
	root, err := os.Getwd()
	if err == nil {
		err = execute(root, os.Stdout, os.Stderr)
	}
	if err == nil {
		return
	}
	var status *statusError
	if errors.As(err, &status) {
		fmt.Fprintln(os.Stderr, status.message)
		os.Exit(status.code)
	}
	var commandExit *exec.ExitError
	if errors.As(err, &commandExit) {
		os.Exit(commandExit.ExitCode())
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func execute(root string, stdout, stderr io.Writer) error {
	options, err := loadConfig(root)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("curl"); err != nil {
		return &statusError{code: 127, message: "curl is required"}
	}
	if err := os.MkdirAll(options.workdir, 0o755); err != nil {
		return fmt.Errorf("create work directory: %w", err)
	}
	options.workdir, err = filepath.Abs(options.workdir)
	if err != nil {
		return fmt.Errorf("resolve work directory: %w", err)
	}
	corpusDir := filepath.Join(options.workdir, "records", options.recordID)
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		return fmt.Errorf("create corpus directory: %w", err)
	}
	for _, name := range []string{"workflows.csv.gz", "workflows.tar.gz"} {
		if err := download(corpusDir, options.recordID, name, stdout, stderr); err != nil {
			return err
		}
	}
	if err := extractCorpus(corpusDir, stdout); err != nil {
		return err
	}
	manifest, err := ensureManifest(corpusDir)
	if err != nil {
		return err
	}

	corpusID := "zenodo:" + options.recordID
	sampleKey := "full"
	tallyPath := filepath.Join(corpusDir, "validate-tally.json")
	if options.sampleSize > 0 {
		sampleKey, manifest, err = sampleManifest(manifest, filepath.Join(corpusDir, "samples"), options.sampleSize, options.sampleSeed, stderr)
		if err != nil {
			return err
		}
		corpusID += ":sample:" + sampleKey
		tallyPath = filepath.Join(corpusDir, "samples", sampleKey, "validate-tally.json")
	}

	validator, err := buildValidator(options.root, options.workdir, stdout, stderr)
	if err != nil {
		return err
	}
	actionCache := filepath.Join(options.workdir, "action-cache")
	resolutionSnapshot := filepath.Join(options.workdir, "action-resolution-snapshot")
	reportRoot := filepath.Join(options.workdir, "reports", options.recordID)
	reports := filepath.Join(reportRoot, validator.digest)
	if sampleKey != "full" {
		reports = filepath.Join(reportRoot, "samples", sampleKey, validator.digest)
	}
	if err := os.MkdirAll(actionCache, 0o755); err != nil {
		return fmt.Errorf("create action cache: %w", err)
	}
	if options.refresh {
		if err := os.RemoveAll(reportRoot); err != nil {
			return fmt.Errorf("remove stale reports: %w", err)
		}
	}
	if err := os.MkdirAll(reports, 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}

	args := []string{
		"validate-batch",
		"--manifest", manifest,
		"--output-dir", reports,
		"--corpus-id", corpusID,
		"--action-cache-dir", actionCache,
		"--action-cache-max-bytes", envDefault("ACTION_CACHE_MAX_BYTES", defaultActionCacheMaxBytes),
		"--action-resolution-snapshot", resolutionSnapshot,
	}
	if options.refresh {
		args = append(args, "--refresh-action-resolution-snapshot")
	}
	if os.Getenv("GITHUB_TOKEN") != "" {
		args = append(args, "--github-token-env", "GITHUB_TOKEN")
	}
	if jobs := os.Getenv("JOBS"); jobs != "" {
		args = append(args, "--jobs", jobs)
	}
	if err := runCommand(options.root, nil, stdout, stderr, validator.path, args...); err != nil {
		return err
	}
	return tallyReports(manifest, reports, tallyPath, validator, sampleKey, options.sampleSeed, options.sampleSize, stdout)
}

func loadConfig(root string) (config, error) {
	sampleSize, err := parseSampleSize(os.Getenv("SAMPLE_SIZE"))
	if err != nil {
		return config{}, &statusError{code: 2, message: err.Error()}
	}
	workdir := os.Getenv("WORKDIR")
	if workdir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return config{}, fmt.Errorf("find home directory: %w", err)
		}
		workdir = filepath.Join(home, "gha-corpus")
	}
	return config{
		root:       root,
		workdir:    workdir,
		recordID:   envDefault("RECORD_ID", defaultRecordID),
		sampleSize: sampleSize,
		sampleSeed: envDefault("SAMPLE_SEED", "default"),
		refresh:    envDefault("REFRESH_ACTION_RESOLUTIONS", "0") == "1",
	}, nil
}

func parseSampleSize(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	if value[0] < '1' || value[0] > '9' {
		return 0, errors.New("SAMPLE_SIZE must be a positive integer")
	}
	for i := 1; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, errors.New("SAMPLE_SIZE must be a positive integer")
		}
	}
	size, err := strconv.Atoi(value)
	if err != nil {
		return 0, errors.New("SAMPLE_SIZE must be a positive integer")
	}
	return size, nil
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func download(corpusDir, recordID, name string, stdout, stderr io.Writer) error {
	destination := filepath.Join(corpusDir, name)
	if info, err := os.Stat(destination); err == nil && info.Mode().IsRegular() {
		return nil
	}
	partial := destination + ".partial"
	url := fmt.Sprintf("https://zenodo.org/api/records/%s/files/%s/content", recordID, name)
	if err := runCommand("", nil, stdout, stderr, "curl", "--fail", "--location", "--retry", "3", "--output", partial, url); err != nil {
		return err
	}
	if err := os.Rename(partial, destination); err != nil {
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}

func extractCorpus(corpusDir string, stdout io.Writer) error {
	marker := filepath.Join(corpusDir, "files", ".complete-v3")
	if info, err := os.Stat(marker); err == nil && info.Mode().IsRegular() {
		return nil
	}
	for _, name := range []string{"files", "reports", "workflows.tsv", "workflows.jsonl"} {
		if err := os.RemoveAll(filepath.Join(corpusDir, name)); err != nil {
			return fmt.Errorf("reset corpus %s: %w", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(corpusDir, "files"), 0o755); err != nil {
		return fmt.Errorf("create corpus files directory: %w", err)
	}

	latest, err := latestWorkflows(filepath.Join(corpusDir, "workflows.csv.gz"))
	if err != nil {
		return err
	}
	workflows := make([]corpusWorkflow, 0, len(latest))
	repositories := map[string]bool{}
	for _, row := range latest {
		if row.validWorkflow == "True" && row.gitChangeType != "D" && row.path != "" && row.hash != "" {
			workflows = append(workflows, corpusWorkflow{repository: row.repository, path: row.path, hash: row.hash})
			repositories[row.repository] = true
		}
	}
	slices.SortFunc(workflows, func(left, right corpusWorkflow) int {
		if result := cmp.Compare(left.repository, right.repository); result != 0 {
			return result
		}
		if result := cmp.Compare(left.path, right.path); result != 0 {
			return result
		}
		return cmp.Compare(left.hash, right.hash)
	})

	wanted := map[string][]workflowDestination{}
	manifest, err := os.Create(filepath.Join(corpusDir, "workflows.tsv"))
	if err != nil {
		return fmt.Errorf("create TSV manifest: %w", err)
	}
	buffered := bufio.NewWriter(manifest)
	for index, workflow := range workflows {
		id := fmt.Sprintf("%06d", index)
		wanted[workflow.hash] = append(wanted[workflow.hash], workflowDestination{id: id, filename: path.Base(workflow.path)})
		if _, err := fmt.Fprintf(buffered, "%s\t%s\t%s\t%s\n", id, workflow.repository, workflow.path, workflow.hash); err != nil {
			_ = manifest.Close()
			return fmt.Errorf("write TSV manifest: %w", err)
		}
	}
	if err := buffered.Flush(); err != nil {
		_ = manifest.Close()
		return fmt.Errorf("flush TSV manifest: %w", err)
	}
	if err := manifest.Close(); err != nil {
		return fmt.Errorf("close TSV manifest: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "%s live workflow files across %s repos\n", comma(len(workflows)), comma(len(repositories))); err != nil {
		return fmt.Errorf("write corpus summary: %w", err)
	}

	extracted, err := extractWorkflowFiles(corpusDir, wanted)
	if err != nil {
		return err
	}
	if len(wanted) != 0 {
		missing := 0
		for _, destinations := range wanted {
			missing += len(destinations)
		}
		return fmt.Errorf("corpus archive is missing %s selected workflows", comma(missing))
	}
	if _, err := fmt.Fprintf(stdout, "extracted %s\n", comma(extracted)); err != nil {
		return fmt.Errorf("write extraction summary: %w", err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return fmt.Errorf("mark corpus extraction complete: %w", err)
	}
	return nil
}

func latestWorkflows(csvPath string) (map[string]corpusRow, error) {
	compressed, err := os.Open(csvPath)
	if err != nil {
		return nil, fmt.Errorf("open workflow metadata: %w", err)
	}
	defer func() { _ = compressed.Close() }()
	source, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, fmt.Errorf("open workflow metadata gzip stream: %w", err)
	}
	defer func() { _ = source.Close() }()
	reader := csv.NewReader(source)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read workflow metadata header: %w", err)
	}
	columns := map[string]int{}
	for index, name := range header {
		columns[name] = index
	}
	required := []string{"uid", "committed_date", "valid_workflow", "git_change_type", "file_path", "file_hash", "repository"}
	for _, name := range required {
		if _, ok := columns[name]; !ok {
			return nil, fmt.Errorf("workflow metadata is missing %q", name)
		}
	}
	latest := map[string]corpusRow{}
	for line := 2; ; line++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read workflow metadata line %d: %w", line, err)
		}
		committed, err := strconv.ParseInt(record[columns["committed_date"]], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse committed date on line %d: %w", line, err)
		}
		uid := record[columns["uid"]]
		if previous, ok := latest[uid]; ok && previous.committed >= committed {
			continue
		}
		latest[uid] = corpusRow{
			committed: committed, repository: record[columns["repository"]], path: record[columns["file_path"]],
			hash: record[columns["file_hash"]], validWorkflow: record[columns["valid_workflow"]], gitChangeType: record[columns["git_change_type"]],
		}
	}
	return latest, nil
}

func extractWorkflowFiles(corpusDir string, wanted map[string][]workflowDestination) (int, error) {
	compressed, err := os.Open(filepath.Join(corpusDir, "workflows.tar.gz"))
	if err != nil {
		return 0, fmt.Errorf("open workflow archive: %w", err)
	}
	defer func() { _ = compressed.Close() }()
	source, err := gzip.NewReader(compressed)
	if err != nil {
		return 0, fmt.Errorf("open workflow archive gzip stream: %w", err)
	}
	defer func() { _ = source.Close() }()
	archive := tar.NewReader(source)
	extracted := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read workflow archive: %w", err)
		}
		if !header.FileInfo().Mode().IsRegular() {
			continue
		}
		hash := path.Base(header.Name)
		destinations := wanted[hash]
		if len(destinations) == 0 {
			continue
		}
		contents, err := io.ReadAll(archive)
		if err != nil {
			return 0, fmt.Errorf("read archived workflow %s: %w", hash, err)
		}
		for _, destination := range destinations {
			directory := filepath.Join(corpusDir, "files", destination.id[:3], destination.id, ".github", "workflows")
			if err := os.MkdirAll(directory, 0o755); err != nil {
				return 0, fmt.Errorf("create workflow directory: %w", err)
			}
			if err := os.WriteFile(filepath.Join(directory, destination.filename), contents, 0o644); err != nil {
				return 0, fmt.Errorf("write workflow: %w", err)
			}
			extracted++
		}
		delete(wanted, hash)
	}
	return extracted, nil
}

func ensureManifest(corpusDir string) (string, error) {
	manifestPath := filepath.Join(corpusDir, "workflows.jsonl")
	if info, err := os.Stat(manifestPath); err == nil && info.Mode().IsRegular() {
		return manifestPath, nil
	}
	source, err := os.Open(filepath.Join(corpusDir, "workflows.tsv"))
	if err != nil {
		return "", fmt.Errorf("open TSV manifest: %w", err)
	}
	defer func() { _ = source.Close() }()
	var records []manifestRecord
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 4 {
			return "", fmt.Errorf("invalid TSV manifest record")
		}
		id, repository, workflowPath, hash := fields[0], fields[1], fields[2], fields[3]
		records = append(records, manifestRecord{
			Hash: hash, ID: id, Path: workflowPath, Repository: repository,
			Source: filepath.Join(corpusDir, "files", id[:3], id, ".github", "workflows", path.Base(workflowPath)),
		})
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read TSV manifest: %w", err)
	}
	if err := writeJSONLines(manifestPath, records); err != nil {
		return "", err
	}
	return manifestPath, nil
}

func loadManifest(manifestPath string) ([]manifestRecord, error) {
	source, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer func() { _ = source.Close() }()
	decoder := json.NewDecoder(source)
	var records []manifestRecord
	for {
		var record manifestRecord
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("read manifest: %w", err)
		}
		records = append(records, record)
	}
	return records, nil
}

func writeJSONLines(destinationPath string, records []manifestRecord) error {
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destinationPath), "."+filepath.Base(destinationPath)+"-*")
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			_ = temporary.Close()
			return fmt.Errorf("write manifest: %w", err)
		}
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close manifest: %w", err)
	}
	if err := os.Rename(temporaryPath, destinationPath); err != nil {
		return fmt.Errorf("publish manifest: %w", err)
	}
	return nil
}

func sampleManifest(sourcePath, samplesRoot string, size int, seed string, stderr io.Writer) (string, string, error) {
	records, err := loadManifest(sourcePath)
	if err != nil {
		return "", "", err
	}
	ranked := make([]rankedRecord, 0, len(records))
	for _, record := range records {
		hash := sha256.New()
		_, _ = io.WriteString(hash, sampleIdentityVersion)
		_, _ = io.WriteString(hash, seed)
		_, _ = hash.Write([]byte{'\x00'})
		_, _ = hash.Write(workflowIdentity(record))
		var rank [sha256.Size]byte
		copy(rank[:], hash.Sum(nil))
		ranked = append(ranked, rankedRecord{rank: rank, record: record})
	}
	if size > len(ranked) {
		return "", "", fmt.Errorf("SAMPLE_SIZE %s exceeds the %s-workflow corpus", comma(size), comma(len(ranked)))
	}
	slices.SortFunc(ranked, func(left, right rankedRecord) int {
		if result := bytes.Compare(left.rank[:], right.rank[:]); result != 0 {
			return result
		}
		return cmp.Compare(left.record.ID, right.record.ID)
	})
	selected := append([]rankedRecord(nil), ranked[:size]...)
	slices.SortFunc(selected, func(left, right rankedRecord) int {
		return cmp.Compare(left.record.ID, right.record.ID)
	})
	selectionHash := sha256.New()
	selectedRecords := make([]manifestRecord, 0, size)
	for _, item := range selected {
		_, _ = selectionHash.Write(workflowIdentity(item.record))
		_, _ = selectionHash.Write([]byte{'\n'})
		selectedRecords = append(selectedRecords, item.record)
	}
	seedDigest := sha256.Sum256([]byte(seed))
	key := fmt.Sprintf("n%d-s%s-m%s", size, hex.EncodeToString(seedDigest[:])[:12], hex.EncodeToString(selectionHash.Sum(nil))[:12])
	manifestPath := filepath.Join(samplesRoot, key, "workflows.jsonl")
	if err := writeJSONLines(manifestPath, selectedRecords); err != nil {
		return "", "", err
	}
	if _, err := fmt.Fprintf(stderr, "sample: %s workflows, seed %s, key %s\n", comma(size), pythonRepr(seed), key); err != nil {
		return "", "", fmt.Errorf("write sample summary: %w", err)
	}
	return key, manifestPath, nil
}

func workflowIdentity(record manifestRecord) []byte {
	return []byte(strings.Join([]string{record.Repository, record.Path, record.Hash}, "\x00"))
}

func pythonRepr(value string) string {
	if strings.Contains(value, "'") && !strings.Contains(value, `"`) {
		return strconv.QuoteToGraphic(value)
	}
	quoted := strconv.QuoteToGraphic(value)
	inner := strings.ReplaceAll(quoted[1:len(quoted)-1], `\"`, `"`)
	inner = strings.ReplaceAll(inner, "'", `\'`)
	return "'" + inner + "'"
}

func buildValidator(root, workdir string, stdout, stderr io.Writer) (validatorInfo, error) {
	validatorPath := filepath.Join(workdir, "buildkite-gha")
	environment := replaceEnvironment(os.Environ(), "CGO_ENABLED", "0")
	if err := runCommand(root, environment, stdout, stderr, "go", "build", "-trimpath", "-buildvcs=false", "-o", validatorPath, "./cmd/buildkite-gha"); err != nil {
		return validatorInfo{}, err
	}
	commit, err := commandOutput(root, stderr, "git", "rev-parse", "HEAD")
	if err != nil {
		return validatorInfo{}, err
	}
	version, err := commandOutput("", stderr, validatorPath, "--version")
	if err != nil {
		return validatorInfo{}, err
	}
	digest, err := fileSHA256(validatorPath)
	if err != nil {
		return validatorInfo{}, err
	}
	if _, err := fmt.Fprintf(stdout, "validator: %s (%s, sha256:%s)\n", version, commit, digest); err != nil {
		return validatorInfo{}, fmt.Errorf("write validator summary: %w", err)
	}
	return validatorInfo{path: validatorPath, commit: commit, version: version, digest: digest}, nil
}

func replaceEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func runCommand(directory string, environment []string, stdout, stderr io.Writer, name string, args ...string) error {
	command := exec.Command(name, args...)
	command.Dir = directory
	command.Stdout = stdout
	command.Stderr = stderr
	if environment != nil {
		command.Env = environment
	}
	return command.Run()
}

func commandOutput(directory string, stderr io.Writer, name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	command.Dir = directory
	command.Stderr = stderr
	output, err := command.Output()
	return strings.TrimRight(string(output), "\n"), err
}

func fileSHA256(path string) (string, error) {
	source, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open validator: %w", err)
	}
	defer func() { _ = source.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, source); err != nil {
		return "", fmt.Errorf("hash validator: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func comma(value int) string {
	digits := strconv.Itoa(value)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}
