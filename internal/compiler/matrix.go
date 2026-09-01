package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const maxMatrixInstances = 256

type matrixPositionError struct {
	err          error
	line, column int
}

func (e matrixPositionError) Error() string { return e.err.Error() }
func (e matrixPositionError) Unwrap() error { return e.err }

func expandMatrix(path string, job workflow.Job, context expression.CompileContext) ([]map[string]any, error) {
	if job.Matrix == nil {
		return []map[string]any{nil}, nil
	}
	matrix, err := resolveMatrix(job.Matrix, context)
	if err != nil {
		line, column := matrixErrorPosition(job, err)
		return nil, locatedJobWrappedError(path, job, line, column, "", err)
	}
	matrices, err := expandMatrixDefinition(matrix)
	if err != nil {
		return nil, jobError(path, job, err.Error())
	}
	return matrices, nil
}

func expandMatrixDefinition(matrix *workflow.Matrix) ([]map[string]any, error) {
	matrices := []map[string]any{{}}
	if len(matrix.Rows) == 0 {
		matrices = nil
	}
	for _, row := range matrix.Rows {
		if len(row.Values) == 0 {
			return nil, fmt.Errorf("matrix dimension %q has no values", row.Name)
		}
		var next []map[string]any
		for _, current := range matrices {
			for _, value := range row.Values {
				combination := make(map[string]any, len(current)+1)
				maps.Copy(combination, current)
				combination[row.Name] = value.Data
				next = append(next, combination)
				if len(next) > maxMatrixInstances {
					return nil, fmt.Errorf("matrix expands beyond %d instances", maxMatrixInstances)
				}
			}
		}
		matrices = next
	}

	matrices = excludeMatrixCombinations(matrices, matrix.Exclude)
	// Includes may overwrite values added by earlier includes, but not original
	// dimensions. Standalone combinations are never candidates for later entries.
	type includedMatrix struct {
		values   map[string]any
		original map[string]any
	}
	included := make([]includedMatrix, len(matrices))
	for i, matrix := range matrices {
		included[i] = includedMatrix{values: matrix, original: cloneAnyMap(matrix)}
	}
	for _, combination := range matrix.Include {
		values := matrixCombinationValues(combination)
		matched := false
		for i := range included {
			if included[i].original == nil || !includeCompatible(included[i].original, values) {
				continue
			}
			maps.Copy(included[i].values, values)
			matched = true
		}
		if !matched {
			included = append(included, includedMatrix{values: cloneAnyMap(values)})
		}
		if len(included) > maxMatrixInstances {
			return nil, fmt.Errorf("matrix expands beyond %d instances", maxMatrixInstances)
		}
	}
	matrices = matrices[:0]
	for _, matrix := range included {
		matrices = append(matrices, matrix.values)
	}
	if len(matrices) == 0 {
		return nil, fmt.Errorf("matrix excludes every combination")
	}
	return matrices, nil
}

func resolveMatrix(matrix *workflow.Matrix, context expression.CompileContext) (*workflow.Matrix, error) {
	if matrix.Expression != nil {
		value, err := evaluateCompileSite(matrix.Expression.Text, expression.ProfileCompile, expression.ResultAny, context)
		if err != nil {
			return nil, matrixPositionError{err: matrixExpressionError(err), line: matrix.Expression.Span.Start.Line, column: matrix.Expression.Span.Start.Column}
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, matrixPositionError{err: fmt.Errorf("matrix expression resolved to %T, want object", value), line: matrix.Expression.Span.Start.Line, column: matrix.Expression.Span.Start.Column}
		}
		return matrixFromObject(object)
	}
	resolved := *matrix
	resolved.Rows = make([]workflow.MatrixRow, len(matrix.Rows))
	for i, row := range matrix.Rows {
		resolved.Rows[i] = row
		if row.Expression == nil {
			resolved.Rows[i].Values = make([]workflow.Value, len(row.Values))
			for j, matrixValue := range row.Values {
				value, err := resolveAuthoredMatrixValue(matrixValue.Data, matrixValue.Span, context)
				if err != nil {
					return nil, matrixPositionError{err: fmt.Errorf("matrix dimension %q: %w", row.Name, matrixExpressionError(err)), line: matrixValue.Span.Start.Line, column: matrixValue.Span.Start.Column}
				}
				resolved.Rows[i].Values[j] = matrixValue
				resolved.Rows[i].Values[j].Data = value
			}
			continue
		}
		value, err := evaluateCompileSite(row.Expression.Text, expression.ProfileCompile, expression.ResultAny, context)
		if err != nil {
			return nil, matrixPositionError{err: fmt.Errorf("matrix dimension %q: %w", row.Name, matrixExpressionError(err)), line: row.Expression.Span.Start.Line, column: row.Expression.Span.Start.Column}
		}
		values, ok := value.([]any)
		if !ok {
			return nil, matrixPositionError{err: fmt.Errorf("matrix dimension %q resolved to %T, want array", row.Name, value), line: row.Expression.Span.Start.Line, column: row.Expression.Span.Start.Column}
		}
		resolved.Rows[i].Expression = nil
		resolved.Rows[i].Values = make([]workflow.Value, len(values))
		for j, value := range values {
			resolved.Rows[i].Values[j] = workflow.Value{Data: value, Span: row.Span}
		}
	}
	var err error
	if resolved.Include, err = resolveMatrixCombinations("include", matrix.Include, matrix.IncludeExpression, context); err != nil {
		if matrix.IncludeExpression != nil {
			return nil, matrixPositionError{err: err, line: matrix.IncludeExpression.Span.Start.Line, column: matrix.IncludeExpression.Span.Start.Column}
		}
		return nil, err
	}
	resolved.IncludeExpression = nil
	if resolved.Exclude, err = resolveMatrixCombinations("exclude", matrix.Exclude, matrix.ExcludeExpression, context); err != nil {
		if matrix.ExcludeExpression != nil {
			return nil, matrixPositionError{err: err, line: matrix.ExcludeExpression.Span.Start.Line, column: matrix.ExcludeExpression.Span.Start.Column}
		}
		return nil, err
	}
	resolved.ExcludeExpression = nil
	return &resolved, nil
}

func resolveMatrixCombinations(name string, combinations []workflow.MatrixCombination, expr *expression.Expression, context expression.CompileContext) ([]workflow.MatrixCombination, error) {
	if expr == nil {
		resolved := make([]workflow.MatrixCombination, len(combinations))
		for i, combination := range combinations {
			resolved[i] = combination
			resolved[i].Values = make(map[string]workflow.Value, len(combination.Values))
			for _, key := range sortedKeys(combination.Values) {
				matrixValue := combination.Values[key]
				value, err := resolveAuthoredMatrixValue(matrixValue.Data, matrixValue.Span, context)
				if err != nil {
					return nil, matrixPositionError{err: fmt.Errorf("matrix %s: %w", name, matrixExpressionError(err)), line: matrixValue.Span.Start.Line, column: matrixValue.Span.Start.Column}
				}
				matrixValue.Data = value
				resolved[i].Values[key] = matrixValue
			}
		}
		return resolved, nil
	}
	value, err := evaluateCompileSite(expr.Text, expression.ProfileCompile, expression.ResultAny, context)
	if err != nil {
		return nil, fmt.Errorf("matrix %s: %w", name, matrixExpressionError(err))
	}
	resolved, err := matrixCombinationsFromValue(name, value)
	if err != nil {
		return nil, err
	}
	return resolved, nil
}

func resolveAuthoredMatrixValue(value any, span workflow.Span, context expression.CompileContext) (any, error) {
	switch value := value.(type) {
	case string:
		if !strings.Contains(value, "${{") {
			return value, nil
		}
		if err := validateCompileSite(value, expression.ProfileCompile, expression.ResultAny); err == nil {
			return evaluateCompileSite(value, expression.ProfileCompile, expression.ResultAny, context)
		}
		return evaluateCompileSite(value, expression.ProfileCompileTemplate, expression.ResultString, context)
	case []any:
		resolved := make([]any, len(value))
		for i, item := range value {
			var err error
			resolved[i], err = resolveAuthoredMatrixValue(item, span, context)
			if err != nil {
				return nil, err
			}
		}
		return resolved, nil
	case map[string]any:
		resolved := make(map[string]any, len(value))
		for _, key := range sortedKeys(value) {
			item := value[key]
			var err error
			resolved[key], err = resolveAuthoredMatrixValue(item, span, context)
			if err != nil {
				return nil, err
			}
		}
		return resolved, nil
	default:
		return value, nil
	}
}

func matrixExpressionError(err error) error {
	if strings.Contains(err.Error(), `compile-time context "needs"`) || strings.Contains(err.Error(), `compile-time context "steps"`) {
		return fmt.Errorf("runtime-dependent matrix expressions are unsupported: %w", err)
	}
	return fmt.Errorf("matrix expression cannot be resolved at compile time: %w", err)
}

func matrixFromObject(object map[string]any) (*workflow.Matrix, error) {
	keys := make([]string, 0, len(object))
	for key := range object {
		if key != "include" && key != "exclude" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	matrix := &workflow.Matrix{}
	for _, key := range keys {
		values, ok := object[key].([]any)
		if !ok {
			return nil, fmt.Errorf("matrix dimension %q resolved to %T, want array", key, object[key])
		}
		row := workflow.MatrixRow{Name: key, Values: make([]workflow.Value, len(values))}
		for i, value := range values {
			row.Values[i] = workflow.Value{Data: value}
		}
		matrix.Rows = append(matrix.Rows, row)
	}
	var err error
	if value, ok := object["include"]; ok {
		if matrix.Include, err = matrixCombinationsFromValue("include", value); err != nil {
			return nil, err
		}
	}
	if value, ok := object["exclude"]; ok {
		if matrix.Exclude, err = matrixCombinationsFromValue("exclude", value); err != nil {
			return nil, err
		}
	}
	return matrix, nil
}

func matrixCombinationsFromValue(name string, value any) ([]workflow.MatrixCombination, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("matrix %s resolved to %T, want array", name, value)
	}
	combinations := make([]workflow.MatrixCombination, len(items))
	for i, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("matrix %s entry %d resolved to %T, want object", name, i, item)
		}
		values := make(map[string]workflow.Value, len(object))
		for key, value := range object {
			values[key] = workflow.Value{Data: value}
		}
		combinations[i] = workflow.MatrixCombination{Values: values}
	}
	return combinations, nil
}

func resolveRunsOn(job workflow.Job, context expression.CompileContext, matrix map[string]any) ([]string, error) {
	context.Matrix = matrix
	if job.RunsOnExpr != nil {
		value, err := evaluateCompileSite(job.RunsOnExpr.Text, expression.ProfileCompile, expression.ResultAny, context)
		if err != nil {
			return nil, fmt.Errorf("runs-on expression cannot be resolved at compile time: %w", err)
		}
		switch value := value.(type) {
		case string:
			return []string{value}, nil
		case []any:
			labels := make([]string, len(value))
			for i, item := range value {
				label, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("runs-on expression item %d resolved to %T, want string", i, item)
				}
				labels[i] = label
			}
			return labels, nil
		default:
			return nil, fmt.Errorf("runs-on expression resolved to %T, want string or array", value)
		}
	}
	labels := make([]string, len(job.RunsOn))
	for i, label := range job.RunsOn {
		value, err := evaluateCompileSite(label, expression.ProfileCompileTemplate, expression.ResultString, context)
		resolved, _ := value.(string)
		if err != nil {
			return nil, fmt.Errorf("runs-on label %q cannot be resolved at compile time: %w", label, err)
		}
		labels[i] = resolved
	}
	return labels, nil
}

func reportableRunnerLabels(job workflow.Job, labels []string) []string {
	if job.RunsOnExpr == nil {
		for _, label := range job.RunsOn {
			if strings.Contains(label, "${{") {
				return nil
			}
		}
		return labels
	}
	text := strings.TrimSpace(job.RunsOnExpr.Text)
	if !strings.HasPrefix(text, "${{") || !strings.HasSuffix(text, "}}") || job.Matrix == nil {
		return nil
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "${{"), "}}"))
	if !strings.HasPrefix(strings.ToLower(body), "matrix.") || strings.ContainsAny(body, " []()|&") {
		return nil
	}
	if matrixContainsExpressions(job.Matrix) {
		return nil
	}
	return labels
}

func matrixContainsExpressions(matrix *workflow.Matrix) bool {
	if matrix == nil {
		return false
	}
	if matrix.Expression != nil || matrix.IncludeExpression != nil || matrix.ExcludeExpression != nil {
		return true
	}
	for _, row := range matrix.Rows {
		if row.Expression != nil {
			return true
		}
		for _, value := range row.Values {
			if containsExpression(value.Data) {
				return true
			}
		}
	}
	for _, combinations := range [][]workflow.MatrixCombination{matrix.Include, matrix.Exclude} {
		for _, combination := range combinations {
			for _, value := range combination.Values {
				if containsExpression(value.Data) {
					return true
				}
			}
		}
	}
	return false
}

func runsOnPosition(job workflow.Job) workflow.Position {
	if job.RunsOnExpr != nil {
		return workflow.Position{Line: job.RunsOnExpr.Span.Start.Line, Column: job.RunsOnExpr.Span.Start.Column}
	}
	return job.Span.Start
}

func excludeMatrixCombinations(matrices []map[string]any, exclusions []workflow.MatrixCombination) []map[string]any {
	if len(exclusions) == 0 {
		return matrices
	}
	out := matrices[:0]
	for _, matrix := range matrices {
		excluded := false
		for _, exclusion := range exclusions {
			if matrixMatches(matrix, matrixCombinationValues(exclusion)) {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, matrix)
		}
	}
	return out
}

func matrixMatches(matrix, pattern map[string]any) bool {
	for key, expected := range pattern {
		actual, ok := matrix[key]
		if !ok || !matrixValuesEqual(actual, expected) {
			return false
		}
	}
	return true
}

func includeCompatible(original, values map[string]any) bool {
	for key, value := range values {
		if originalValue, exists := original[key]; exists && !matrixValuesEqual(originalValue, value) {
			return false
		}
	}
	return true
}

func matrixValuesEqual(left, right any) bool {
	if leftNumber, ok := matrixNumber(left); ok {
		rightNumber, rightOK := matrixNumber(right)
		return rightOK && leftNumber.Cmp(rightNumber) == 0
	}
	switch left := left.(type) {
	case []any:
		right, ok := right.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for i := range left {
			if !matrixValuesEqual(left[i], right[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		right, ok := right.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			other, ok := right[key]
			if !ok || !matrixValuesEqual(value, other) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(left, right)
	}
}

func matrixNumber(value any) (*big.Rat, bool) {
	var text string
	switch value := value.(type) {
	case json.Number:
		text = value.String()
	default:
		reflected := reflect.ValueOf(value)
		switch reflected.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			text = strconv.FormatInt(reflected.Int(), 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			text = strconv.FormatUint(reflected.Uint(), 10)
		case reflect.Float32, reflect.Float64:
			text = strconv.FormatFloat(reflected.Float(), 'g', -1, reflected.Type().Bits())
		default:
			return nil, false
		}
	}
	number, ok := new(big.Rat).SetString(text)
	return number, ok
}

func matrixCombinationValues(combination workflow.MatrixCombination) map[string]any {
	out := make(map[string]any, len(combination.Values))
	for key, value := range combination.Values {
		out[key] = value.Data
	}
	return out
}

func instanceKey(jobID string, matrix map[string]any) (string, error) {
	return namespacedInstanceKey("", jobID, matrix)
}

func namespacedInstanceKey(namespace, jobID string, matrix map[string]any) (string, error) {
	prefix := "gha-"
	if namespace != "" {
		prefix += namespace + "-"
	}
	prefix += sanitize(jobID)
	if len(matrix) == 0 {
		return prefix, nil
	}
	canonical, err := json.Marshal(matrix)
	if err != nil {
		return "", fmt.Errorf("canonicalize matrix: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return prefix + "-" + hex.EncodeToString(digest[:6]), nil
}

func instanceLabel(job workflow.Job, matrix map[string]any, context expression.CompileContext) string {
	label := job.Name
	if label == "" {
		label = job.ID
	} else {
		context.Matrix = matrix
		if value, err := evaluateCompileSite(label, expression.ProfileCompileTemplate, expression.ResultString, context); err == nil {
			resolved, _ := value.(string)
			label = resolved
			withoutMatrix := context
			withoutMatrix.Matrix = nil
			if _, err := evaluateCompileSite(job.Name, expression.ProfileCompileTemplate, expression.ResultString, withoutMatrix); err != nil {
				return label
			}
		}
	}
	if len(matrix) == 0 {
		return label
	}
	return matrixInstanceLabel(label, matrix)
}

func instanceCheckLabel(job JobInstance) string {
	if len(job.Matrix) == 0 {
		return job.LogicalJobID
	}
	keys := make([]string, 0, len(job.Matrix))
	for key := range job.Matrix {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		encoded, _ := json.Marshal(job.Matrix[key])
		values = append(values, key+"="+string(encoded))
	}
	return job.LogicalJobID + " (" + strings.Join(values, ", ") + ")"
}

func matrixInstanceLabel(label string, matrix map[string]any) string {
	if len(matrix) == 0 {
		return label
	}
	keys := make([]string, 0, len(matrix))
	for key := range matrix {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, fmt.Sprintf("%s=%v", key, matrix[key]))
	}
	return label + " (" + strings.Join(values, ", ") + ")"
}

func sanitize(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			out.WriteRune(r)
		} else if out.Len() > 0 {
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}
