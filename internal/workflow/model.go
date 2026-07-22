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

// Workflow is the actionlint-independent syntax needed by the Phase 0 compiler.
type Workflow struct {
	Name string `json:"name,omitempty"`
	Jobs []Job  `json:"jobs"`
}

// Job is one logical GitHub Actions job.
type Job struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name,omitempty"`
	Needs       []string               `json:"needs,omitempty"`
	RunsOn      []string               `json:"runs_on,omitempty"`
	RunsOnExpr  *expression.Expression `json:"runs_on_expression,omitempty"`
	Matrix      *Matrix                `json:"matrix,omitempty"`
	FailFast    *bool                  `json:"fail_fast,omitempty"`
	MaxParallel *int                   `json:"max_parallel,omitempty"`
	Reusable    bool                   `json:"reusable_workflow,omitempty"`
	Steps       []Step                 `json:"steps"`
	Span        Span                   `json:"span"`
}

// Matrix retains either static rows or a deferred expression.
type Matrix struct {
	Rows       []MatrixRow            `json:"rows,omitempty"`
	Expression *expression.Expression `json:"expression,omitempty"`
	HasInclude bool                   `json:"has_include,omitempty"`
	HasExclude bool                   `json:"has_exclude,omitempty"`
	Span       Span                   `json:"span"`
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

// Step is the execution data retained in the Phase 0 IR.
type Step struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind"`
	Run  string `json:"run,omitempty"`
	Uses string `json:"uses,omitempty"`
	Span Span   `json:"span"`
}
