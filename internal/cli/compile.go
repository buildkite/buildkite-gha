package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"github.com/buildkite/buildkite-gha/internal/workflowprocessing"
)

func compile(args []string, stdout, stderr io.Writer, clientVersion string, agent transport.Agent) int {
	version := commandVersion(clientVersion)
	workflowPath, eventPath, format, err := compileArgs(args)
	if err != nil {
		return usageError(stderr, "compile: %v", err)
	}
	if eventPath == "" {
		return usageError(stderr, "compile: --event-path is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out := newProcessingOutput(ctx, "compile", "text", stderr, stderr, agent)
	event, eventErr := os.ReadFile(eventPath)
	if parsedEvent, parseErr := compiler.ParseEvent(event); eventErr == nil && parseErr == nil {
		out.sourceLinks = sourceLinksForEvent(parsedEvent)
	}
	source, event, ok := loadProcessingInputs(ctx, out, workflowPath, "", "event input could not be read", func() ([]byte, error) { return event, eventErr })
	if !ok {
		return 1
	}
	repositorySource, cleanup, sourceErr := newHostedActionSource(ctx, "", clientVersion, nil, nil)
	if sourceErr != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: configure public repository source: %v\n", sourceErr)
		return 1
	}
	defer cleanup()
	options := compiler.DefaultOptions()
	options.RepositorySource = repositorySource
	// Environments resolve only through the job-scoped Agent API, so compile
	// resolves them when it runs inside a Buildkite job and otherwise leaves
	// workflows that declare environments to fail at compile time.
	options.EnvironmentSource = environmentSourceFromAgent(clientVersion)
	processingReport, ok := validatedProcessingReportWithOptions(ctx, out, workflowPath, "", source, event, true, &options)
	if !ok {
		return 1
	}
	var result []byte
	var warnings []compiler.Warning
	if format == "ir-json" {
		result, err = compiler.CompileWithOptionsContext(ctx, workflowPath, source, event, options)
		if err == nil {
			var ir compiler.IR
			if decodeErr := json.Unmarshal(result, &ir); decodeErr != nil {
				err = fmt.Errorf("decode compiler IR: %w", decodeErr)
			} else {
				warnings = ir.Warnings
			}
		}
	} else {
		digest, digestErr := executableDigest()
		if digestErr != nil {
			processingReport.AddEnvironmentFailure("compiler executable could not be inspected")
			processingReport.Result = "indeterminate"
			_ = out.write(ctx, processingReport)
			return 1
		}
		bundle, compileErr := compiler.CompileBundleContext(ctx, workflowPath, source, event, version, digest, "gha-importer", options)
		processingReport.ApplyEvidence(bundle.Processing)
		err = compileErr
		result = bundle.Pipeline
		warnings = bundle.IR.Warnings
	}
	if err != nil {
		processingReport.AddFailure(workflowPath, workflowprocessing.StagePlans, workflowprocessing.CodePlanConstruction, "compatibility", err)
		processingReport.Result = "incompatible"
		_ = out.write(ctx, processingReport)
		return 1
	}
	processingReport.Result = "compilable"
	_ = out.write(ctx, processingReport)
	writeCompilerWarnings(stderr, "compile", workflowPath, warnings)
	if _, err := stdout.Write(result); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: write output: %v\n", err)
		return 1
	}
	return 0
}

func writeCompilerWarnings(stderr io.Writer, command, path string, warnings []compiler.Warning) {
	for _, warning := range warnings {
		warningPath := warning.Path
		if warningPath == "" {
			warningPath = path
		}
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: %s: warning: %s:%d:%d: [%s] %s\n", command, warningPath, warning.Line, warning.Column, warning.Code, warning.Message)
	}
}

func compileArgs(args []string) (workflowPath, eventPath, format string, err error) {
	format = "pipeline"
	filtered := make([]string, 0, len(args))
	formatSeen := false
	for i := 0; i < len(args); i++ {
		if args[i] != "--format" {
			filtered = append(filtered, args[i])
			continue
		}
		if formatSeen {
			return "", "", "", fmt.Errorf("--format may only be specified once")
		}
		formatSeen = true
		i++
		if i == len(args) {
			return "", "", "", fmt.Errorf("--format requires pipeline or ir-json")
		}
		format = args[i]
		if format != "pipeline" && format != "ir-json" {
			return "", "", "", fmt.Errorf("--format must be pipeline or ir-json")
		}
	}
	workflowPath, eventPath, err = workflowArgs(filtered)
	return workflowPath, eventPath, format, err
}

func workflowArgs(args []string) (workflowPath, eventPath string, err error) {
	eventPathSeen := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--event-path":
			if eventPathSeen {
				return "", "", fmt.Errorf("--event-path may only be specified once")
			}
			eventPathSeen = true
			i++
			if i == len(args) {
				return "", "", fmt.Errorf("--event-path requires a path")
			}
			eventPath = args[i]
		case "-h", "--help":
			return "", "", fmt.Errorf("help must be requested immediately after the command")
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", "", fmt.Errorf("unknown option %q", args[i])
			}
			if workflowPath != "" {
				return "", "", fmt.Errorf("expected one workflow path")
			}
			workflowPath = args[i]
		}
	}
	if workflowPath == "" {
		return "", "", fmt.Errorf("workflow path is required")
	}
	return workflowPath, eventPath, nil
}
