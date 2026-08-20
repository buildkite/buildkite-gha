// Package telemetry emits bounded, best-effort buildkite-gha product events.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	EventCommandCompleted = "pipelines:buildkite_gha:command_completed"

	defaultTimeout        = 1500 * time.Millisecond
	responseDrainLimit    = 32 << 10
	maxClientVersionBytes = 64
	maxErrorMessageBytes  = 1024
	maxDurationMS         = int64(1<<31 - 1)
	maxDiagnostics        = 20
)

type Command string

const (
	CommandPluginImport Command = "plugin_import"
	CommandRunJob       Command = "run_job"
)

type Outcome string

const (
	OutcomeSuccess          Outcome = "success"
	OutcomeFailure          Outcome = "failure"
	OutcomeCancelled        Outcome = "cancelled"
	OutcomeSkipped          Outcome = "skipped"
	OutcomeToleratedFailure Outcome = "tolerated_failure"
	OutcomeUsageError       Outcome = "usage_error"
)

type FailurePhase string

const (
	FailurePhaseConfiguration     FailurePhase = "configuration"
	FailurePhaseSourceResolution  FailurePhase = "source_resolution"
	FailurePhaseParsing           FailurePhase = "parsing"
	FailurePhaseEvaluation        FailurePhase = "evaluation"
	FailurePhaseAdmission         FailurePhase = "admission"
	FailurePhaseCompilation       FailurePhase = "compilation"
	FailurePhaseArtifactUpload    FailurePhase = "artifact_upload"
	FailurePhasePipelineUpload    FailurePhase = "pipeline_upload"
	FailurePhaseExecution         FailurePhase = "execution"
	FailurePhaseResultPublication FailurePhase = "result_publication"
	FailurePhaseUnknown           FailurePhase = "unknown"
)

type FailureCode string

const (
	FailureCodeUnknown            FailureCode = "unknown"
	FailureCodeWorkflowSyntax     FailureCode = "E_WORKFLOW_SYNTAX"
	FailureCodeEventInvalid       FailureCode = "E_EVENT_INVALID"
	FailureCodeGraphInvalid       FailureCode = "E_GRAPH_INVALID"
	FailureCodeMatrixInvalid      FailureCode = "E_MATRIX_INVALID"
	FailureCodeExpressionInvalid  FailureCode = "E_EXPRESSION_INVALID"
	FailureCodeActionDiscovery    FailureCode = "E_ACTION_DISCOVERY"
	FailureCodeActionResolution   FailureCode = "E_ACTION_RESOLUTION"
	FailureCodePlanConstruction   FailureCode = "E_PLAN_CONSTRUCTION"
	FailureCodePipelineGeneration FailureCode = "E_PIPELINE_GENERATION"
	FailureCodeEnvironment        FailureCode = "E_ENVIRONMENT"
	FailureCodeProfile            FailureCode = "E_PROFILE"
)

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

type Diagnostic struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
}

type Details struct {
	FailurePhase          FailurePhase
	FailureCode           FailureCode
	ErrorMessage          string
	ErrorMessageTruncated bool
	Diagnostics           []Diagnostic
}

type Properties struct {
	Command               Command      `json:"command"`
	Outcome               Outcome      `json:"outcome"`
	ClientVersion         string       `json:"client_version"`
	DurationMS            int64        `json:"duration_ms,omitempty"`
	FailurePhase          FailurePhase `json:"failure_phase,omitempty"`
	FailureCode           FailureCode  `json:"failure_code,omitempty"`
	ErrorMessage          string       `json:"error_message,omitempty"`
	ErrorMessageTruncated bool         `json:"error_message_truncated,omitempty"`
	Diagnostics           []Diagnostic `json:"diagnostics,omitempty"`
}

type Config struct {
	Endpoint      string
	JobID         string
	JobToken      string
	ClientVersion string
	Disabled      bool
	Client        *http.Client
	Timeout       time.Duration
}

type Client struct {
	eventsURL     string
	jobToken      string
	clientVersion string
	client        *http.Client
	timeout       time.Duration
}

func New(config Config) (*Client, error) {
	if config.Disabled || config.Endpoint == "" || config.JobID == "" || config.JobToken == "" {
		return nil, nil
	}
	if !validJobID(config.JobID) {
		return nil, fmt.Errorf("telemetry Agent job ID is invalid")
	}
	if strings.ContainsAny(config.JobToken, "\r\n") {
		return nil, fmt.Errorf("telemetry Agent job token is invalid")
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || !validAgentURL(u) {
		return nil, fmt.Errorf("safe telemetry Agent endpoint using HTTPS or loopback HTTP is required")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/jobs/" + config.JobID + "/posthog/events"
	u.RawPath = ""

	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	bounded := *client
	bounded.Jar = nil
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	timeout := config.Timeout
	if timeout <= 0 || timeout > defaultTimeout {
		timeout = defaultTimeout
	}
	return &Client{
		eventsURL:     u.String(),
		jobToken:      config.JobToken,
		clientVersion: boundedClientVersion(config.ClientVersion),
		client:        &bounded,
		timeout:       timeout,
	}, nil
}

func (c *Client) Emit(command Command, outcome Outcome, duration time.Duration, details Details) error {
	return c.EmitContext(context.Background(), command, outcome, duration, details)
}

func (c *Client) EmitContext(ctx context.Context, command Command, outcome Outcome, duration time.Duration, details Details) error {
	if c == nil {
		return nil
	}
	if !validCommand(command) {
		return fmt.Errorf("invalid telemetry command")
	}
	if !validOutcome(outcome) {
		return fmt.Errorf("invalid telemetry outcome")
	}
	if details.FailurePhase != "" && !validFailurePhase(details.FailurePhase) {
		return fmt.Errorf("invalid telemetry failure phase")
	}
	if details.FailureCode != "" && !validFailureCode(details.FailureCode) {
		return fmt.Errorf("invalid telemetry failure code")
	}
	diagnostics, err := boundedDiagnostics(details.Diagnostics)
	if err != nil {
		return err
	}
	errorMessage, errorMessageTruncated := boundedErrorMessage(details.ErrorMessage)
	errorMessageTruncated = errorMessage != "" && (errorMessageTruncated || details.ErrorMessageTruncated)
	durationMS := duration.Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	} else if durationMS > maxDurationMS {
		durationMS = maxDurationMS
	}
	body, err := json.Marshal(struct {
		Event      string     `json:"event"`
		Properties Properties `json:"properties"`
	}{
		Event: EventCommandCompleted,
		Properties: Properties{
			Command: command, Outcome: outcome, ClientVersion: c.clientVersion, DurationMS: durationMS,
			FailurePhase: details.FailurePhase, FailureCode: details.FailureCode,
			ErrorMessage: errorMessage, ErrorMessageTruncated: errorMessageTruncated, Diagnostics: diagnostics,
		},
	})
	if err != nil {
		return fmt.Errorf("encode telemetry event: %v", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.eventsURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create telemetry request: %v", err)
	}
	request.Header.Set("Authorization", "Token "+c.jobToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("send telemetry event: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, responseDrainLimit))
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("telemetry service returned HTTP %d", response.StatusCode)
	}
	return nil
}

func validFailurePhase(phase FailurePhase) bool {
	switch phase {
	case FailurePhaseConfiguration, FailurePhaseSourceResolution, FailurePhaseParsing, FailurePhaseEvaluation,
		FailurePhaseAdmission, FailurePhaseCompilation, FailurePhaseArtifactUpload, FailurePhasePipelineUpload,
		FailurePhaseExecution, FailurePhaseResultPublication, FailurePhaseUnknown:
		return true
	default:
		return false
	}
}

func validFailureCode(code FailureCode) bool {
	switch code {
	case FailureCodeUnknown, FailureCodeWorkflowSyntax, FailureCodeEventInvalid, FailureCodeGraphInvalid,
		FailureCodeMatrixInvalid, FailureCodeExpressionInvalid, FailureCodeActionDiscovery,
		FailureCodeActionResolution, FailureCodePlanConstruction, FailureCodePipelineGeneration,
		FailureCodeEnvironment, FailureCodeProfile:
		return true
	default:
		return false
	}
}

func boundedDiagnostics(in []Diagnostic) ([]Diagnostic, error) {
	seen := make(map[string]bool, min(len(in), maxDiagnostics))
	out := make([]Diagnostic, 0, min(len(in), maxDiagnostics))
	for _, diagnostic := range in {
		severity, ok := diagnosticSeverity(diagnostic.Code)
		if !ok || diagnostic.Severity != severity {
			return nil, fmt.Errorf("invalid telemetry diagnostic")
		}
		if seen[diagnostic.Code] {
			continue
		}
		seen[diagnostic.Code] = true
		out = append(out, diagnostic)
		if len(out) == maxDiagnostics {
			break
		}
	}
	return out, nil
}

func diagnosticSeverity(code string) (Severity, bool) {
	switch code {
	case string(FailureCodeWorkflowSyntax), string(FailureCodeEventInvalid), string(FailureCodeGraphInvalid),
		string(FailureCodeMatrixInvalid), string(FailureCodeExpressionInvalid), string(FailureCodeActionDiscovery),
		string(FailureCodeActionResolution), string(FailureCodePlanConstruction), string(FailureCodePipelineGeneration),
		string(FailureCodeEnvironment), string(FailureCodeProfile):
		return SeverityError, true
	case "W_ACTION_RUNTIME_UNKNOWN", "W_WORKFLOW_CONCURRENCY_CANCEL_IN_PROGRESS_IGNORED":
		return SeverityWarning, true
	default:
		return "", false
	}
}

func validCommand(command Command) bool {
	return command == CommandPluginImport || command == CommandRunJob
}

func validOutcome(outcome Outcome) bool {
	switch outcome {
	case OutcomeSuccess, OutcomeFailure, OutcomeCancelled, OutcomeSkipped, OutcomeToleratedFailure, OutcomeUsageError:
		return true
	default:
		return false
	}
}

func boundedClientVersion(version string) string {
	if version == "" {
		return "unknown"
	}
	if len(version) > maxClientVersionBytes {
		version = version[:maxClientVersionBytes]
	}
	for _, character := range version {
		if character < 0x20 || character > 0x7e {
			return "unknown"
		}
	}
	return version
}

func boundedErrorMessage(message string) (string, bool) {
	message = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, strings.ToValidUTF8(message, "�"))
	message = strings.Join(strings.Fields(message), " ")
	if len(message) <= maxErrorMessageBytes {
		return message, false
	}
	end := maxErrorMessageBytes
	for !utf8.ValidString(message[:end]) {
		end--
	}
	return strings.TrimSpace(message[:end]), true
}

func validAgentURL(u *url.URL) bool {
	if u == nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

func validJobID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range []byte(value) {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}
