package compiler

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

// Max-parallel resolves before expansion; unlike timeout it cannot read a
// matrix row or strategy. Leave the existing output-derived subset deferred.
func resolveJobMaxParallel(job workflow.Job, context expression.CompileContext) (workflow.Job, error) {
	expr := job.MaxParallelExpression
	if expr == nil {
		return job, nil
	}
	needs, err := referencesContext(expr.Text, expression.ProfileStrategyParallel, "needs", false)
	if err != nil {
		return job, err
	}
	if needs {
		if _, err := expression.RuntimeMatrixOutput(*expr); err != nil {
			return job, errors.New("needs-derived max-parallel requires fromJSON(needs.<job>.outputs.<name>) on a needs-derived matrix")
		}
		return job, nil
	}
	number, err := compileKnownJobNumber(expr.Text, expression.ProfileStrategyParallel, context)
	if err != nil {
		return job, err
	}
	if number < 1 || math.Trunc(number) != number || number >= math.Exp2(strconv.IntSize-1) {
		return job, errors.New("max-parallel expression must produce a positive integer within the supported integer range")
	}
	limit := int(number)
	job.MaxParallel, job.MaxParallelExpression = &limit, nil
	return job, nil
}

func resolveJobTimeout(job workflow.Job, context expression.CompileContext) (workflow.Job, error) {
	if job.TimeoutMinutesExpression == nil {
		return job, nil
	}
	minutes, err := compileKnownJobNumber(job.TimeoutMinutesExpression.Text, expression.ProfileJobControl, context)
	if err != nil {
		return job, err
	}
	if minutes <= 0 || minutes > 360 {
		return job, errors.New("timeout-minutes expression must produce a number greater than 0 and at most 360")
	}
	job.TimeoutMinutes, job.TimeoutMinutesExpression = minutes, nil
	return job, nil
}

func compileKnownJobNumber(source string, profile expression.ProfileID, context expression.CompileContext) (float64, error) {
	// Validate the authored field before folding, including unreachable branches.
	if err := validateCompileSite(source, profile, expression.ResultNumber); err != nil {
		return 0, err
	}
	reduced, err := reduceCompileSite(source, expression.ProfileCompile, expression.ResultAny, context)
	if err != nil {
		if text, safe := expression.AuthoredErrorText(err); safe {
			return 0, fmt.Errorf("expression must resolve at compile time: %s", text)
		}
		// Evaluator errors can quote input, event, or variable values.
		return 0, errors.New("expression could not be evaluated at compile time")
	}
	if !reduced.Known {
		return 0, errors.New("expression must resolve at compile time")
	}
	number, ok := staticNumber(reduced.Value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, errors.New("expression must produce a finite number")
	}
	return number, nil
}
