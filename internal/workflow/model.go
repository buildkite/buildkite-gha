// Package workflow owns the parsed workflow model used by the compiler.
package workflow

import "github.com/buildkite/buildkite-gha/internal/expression"

// Position is a one-based source position.
type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Span identifies a half-open region in a workflow source file.
type Span struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Workflow is the actionlint-independent syntax needed by the workflow compiler.
type Workflow struct {
	Name                    string                `json:"name,omitempty"`
	Triggers                []Trigger             `json:"triggers,omitempty"`
	Env                     map[string]string     `json:"env,omitempty"`
	Permissions             *Permissions          `json:"permissions,omitempty"`
	Concurrency             *Concurrency          `json:"concurrency,omitempty"`
	DefaultShell            string                `json:"default_shell,omitempty"`
	DefaultWorkingDirectory string                `json:"default_working_directory,omitempty"`
	CallInputs              map[string]CallInput  `json:"call_inputs,omitempty"`
	CallOutputs             map[string]CallOutput `json:"call_outputs,omitempty"`
	RequiredCallSecrets     []string              `json:"required_call_secrets,omitempty"`
	Callable                bool                  `json:"callable,omitempty"`
	Jobs                    []Job                 `json:"jobs"`
}

// ReusableOnly reports whether the workflow can only be invoked through
// workflow_call and must not become a directly runnable aggregate group.
func (w Workflow) ReusableOnly() bool {
	if len(w.Triggers) == 0 {
		return false
	}
	for _, trigger := range w.Triggers {
		if trigger.Event != "workflow_call" {
			return false
		}
	}
	return true
}

// Trigger is one configured entry in the workflow's on section. Slice fields
// are nil when omitted and non-nil when explicitly configured (including an
// empty list), which is significant for GitHub's defaults.
type Trigger struct {
	Event          string           `json:"event"`
	Types          []string         `json:"types,omitempty"`
	Branches       []string         `json:"branches,omitempty"`
	BranchesIgnore []string         `json:"branches_ignore,omitempty"`
	Tags           []string         `json:"tags,omitempty"`
	TagsIgnore     []string         `json:"tags_ignore,omitempty"`
	Paths          []string         `json:"paths,omitempty"`
	PathsIgnore    []string         `json:"paths_ignore,omitempty"`
	Workflows      []string         `json:"workflows,omitempty"`
	Schedules      []Schedule       `json:"schedules,omitempty"`
	Dispatch       *DispatchTrigger `json:"dispatch,omitempty"`
}

type Schedule struct{ Cron, Timezone string }

// DispatchTrigger retains workflow_dispatch configuration for later adapters.
type DispatchTrigger struct {
	Inputs []DispatchInput `json:"inputs,omitempty"`
}

type DispatchInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Type        string   `json:"type,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// Concurrency is the supported subset of a GitHub Actions concurrency group.
// Cancellation is retained for workflow warnings and job compatibility
// diagnostics; generated Buildkite concurrency groups provide queue semantics only.
type Concurrency struct {
	Group                      string                 `json:"group"`
	CancelInProgress           bool                   `json:"cancel_in_progress,omitempty"`
	CancelInProgressExpression *expression.Expression `json:"cancel_in_progress_expression,omitempty"`
	CancelInProgressPosition   Position               `json:"-"`
	Span                       Span                   `json:"span"`
}

// Permissions is an explicitly declared GitHub token permission set. Omitted
// permissions remain nil so the compiler can distinguish them from an explicit
// empty permission map when applying its narrow product default.
type Permissions struct {
	Scopes map[string]string `json:"scopes"`
	Span   Span              `json:"span"`
}

// CallInput declares one statically resolvable workflow_call input.
type CallInput struct {
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Default  *Value `json:"default,omitempty"`
}

// CallOutput declares one caller-visible workflow_call output.
type CallOutput struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Span  Span   `json:"span"`
}

// Job is one logical GitHub Actions job.
type Job struct {
	ID                      string                 `json:"id"`
	Name                    string                 `json:"name,omitempty"`
	Needs                   []string               `json:"needs,omitempty"`
	RunsOn                  []string               `json:"runs_on,omitempty"`
	RunsOnExpr              *expression.Expression `json:"runs_on_expression,omitempty"`
	Matrix                  *Matrix                `json:"matrix,omitempty"`
	FailFast                *bool                  `json:"fail_fast,omitempty"`
	MaxParallel             *int                   `json:"max_parallel,omitempty"`
	Concurrency             *Concurrency           `json:"concurrency,omitempty"`
	Reusable                *ReusableWorkflowCall  `json:"reusable_workflow,omitempty"`
	Env                     map[string]string      `json:"env,omitempty"`
	Permissions             *Permissions           `json:"permissions,omitempty"`
	If                      string                 `json:"if,omitempty"`
	IfSpan                  Span                   `json:"-"`
	ContinueOnError         bool                   `json:"continue_on_error,omitempty"`
	TimeoutMinutes          float64                `json:"timeout_minutes,omitempty"`
	Outputs                 map[string]string      `json:"outputs,omitempty"`
	Container               *Container             `json:"container,omitempty"`
	Services                []Service              `json:"services,omitempty"`
	ServicesExpression      string                 `json:"services_expression,omitempty"`
	DefaultShell            string                 `json:"default_shell,omitempty"`
	DefaultWorkingDirectory string                 `json:"default_working_directory,omitempty"`
	Steps                   []Step                 `json:"steps"`
	Span                    Span                   `json:"span"`
}

// Container is the statically owned subset of a GitHub Actions container.
type Container struct {
	Image string            `json:"image"`
	Env   map[string]string `json:"env,omitempty"`
	Ports []string          `json:"ports,omitempty"`
	Span  Span              `json:"span"`
}

// Service is a named service container. Services retain workflow declaration
// order even though actionlint exposes them as a map.
type Service struct {
	Name      string           `json:"name"`
	Container ServiceContainer `json:"container"`
}

// ServiceContainer is the GitHub Actions service-container definition.
type ServiceContainer struct {
	Image       string                `json:"image"`
	Credentials *ContainerCredentials `json:"credentials,omitempty"`
	Env         map[string]string     `json:"env,omitempty"`
	Ports       []string              `json:"ports,omitempty"`
	Volumes     []string              `json:"volumes,omitempty"`
	Options     string                `json:"options,omitempty"`
	Command     string                `json:"command,omitempty"`
	Entrypoint  string                `json:"entrypoint,omitempty"`
	Span        Span                  `json:"span"`
}

type ContainerCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// ReusableWorkflowCall is a job-level invocation of another workflow.
type ReusableWorkflowCall struct {
	Uses           string           `json:"uses"`
	Inputs         map[string]Value `json:"inputs,omitempty"`
	Secrets        bool             `json:"secrets,omitempty"`
	InheritSecrets bool             `json:"inherit_secrets,omitempty"`
	Span           Span             `json:"span"`
}

// Matrix retains either static rows or a deferred expression.
type Matrix struct {
	Rows              []MatrixRow            `json:"rows,omitempty"`
	Expression        *expression.Expression `json:"expression,omitempty"`
	Include           []MatrixCombination    `json:"include,omitempty"`
	IncludeExpression *expression.Expression `json:"include_expression,omitempty"`
	Exclude           []MatrixCombination    `json:"exclude,omitempty"`
	Span              Span                   `json:"span"`
}

// MatrixCombination is one include or exclude entry with source-located values.
type MatrixCombination struct {
	Values map[string]Value `json:"values"`
	Span   Span             `json:"span"`
}

// MatrixRow is one named matrix dimension.
type MatrixRow struct {
	Name       string                 `json:"name"`
	Values     []Value                `json:"values,omitempty"`
	Expression *expression.Expression `json:"expression,omitempty"`
	Span       Span                   `json:"span"`
}

// Value is an owned JSON-compatible matrix value with its source span.
type Value struct {
	Data any  `json:"data"`
	Span Span `json:"span"`
}

// Step is the execution data retained in the workflow compiler IR.
type Step struct {
	ID                        string            `json:"id,omitempty"`
	Name                      string            `json:"name,omitempty"`
	Kind                      string            `json:"kind"`
	Background                bool              `json:"background,omitempty"`
	Targets                   []string          `json:"targets,omitempty"`
	Run                       string            `json:"run,omitempty"`
	Uses                      string            `json:"uses,omitempty"`
	Shell                     string            `json:"shell,omitempty"`
	WorkingDirectory          string            `json:"working_directory,omitempty"`
	Env                       map[string]string `json:"env,omitempty"`
	With                      map[string]string `json:"with,omitempty"`
	If                        string            `json:"if,omitempty"`
	IfSpan                    Span              `json:"-"`
	ContinueOnError           bool              `json:"continue_on_error,omitempty"`
	ContinueOnErrorExpression string            `json:"continue_on_error_expression,omitempty"`
	TimeoutMinutes            float64           `json:"timeout_minutes,omitempty"`
	TimeoutMinutesExpression  string            `json:"timeout_minutes_expression,omitempty"`
	Span                      Span              `json:"span"`
}
