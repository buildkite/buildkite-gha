package expression

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/rhysd/actionlint"
)

func validatePureFunction(validator *semanticValidator, node *actionlint.FuncCallNode) (bool, error) {
	name := strings.ToLower(node.Callee)
	argc := len(node.Args)
	valid := false
	switch name {
	case "startswith", "contains", "endswith":
		valid = argc == 2
	case "format":
		valid = argc >= 1 && argc <= 255
	case "join":
		valid = argc == 1 || argc == 2
	case "tojson", "fromjson":
		valid = argc == 1
	case "case":
		valid = argc >= 3 && argc <= 255 && argc%2 == 1
	default:
		return false, nil
	}
	if !valid {
		return true, fmt.Errorf("function %q received an unsupported number of arguments", node.Callee)
	}
	for _, argument := range node.Args {
		if err := validator.validate(argument); err != nil {
			return true, err
		}
	}
	return true, nil
}

func evaluatePureFunction[T any](evaluator *expressionEvaluator[T], node *actionlint.FuncCallNode) (T, bool, error) {
	var zero T
	name := strings.ToLower(node.Callee)
	argc := len(node.Args)
	switch name {
	case "case":
		if argc < 3 || argc > 255 || argc%2 != 1 {
			return zero, true, fmt.Errorf("function %q requires 3 to 255 odd-numbered arguments", node.Callee)
		}
		value, err := evaluateCaseFunction(evaluator, node, 0)
		return value, true, err
	case "startswith", "contains", "endswith":
		if argc != 2 {
			return zero, true, fmt.Errorf("function %q requires 2 arguments", node.Callee)
		}
		first, err := evaluator.evaluate(node.Args[0])
		if err != nil {
			return zero, true, err
		}
		value, known := evaluator.domain.value(first)
		if !known {
			second, err := evaluator.evaluate(node.Args[1])
			if err != nil {
				return zero, true, err
			}
			return evaluator.domain.unknown(first, second), true, nil
		}
		if name == "contains" {
			if items, ok := expressionCollection(value); ok {
				if len(items) == 0 {
					return evaluator.domain.derive(false, first), true, nil
				}
				searchValue, err := evaluator.evaluate(node.Args[1])
				if err != nil {
					return zero, true, err
				}
				search, searchKnown := evaluator.domain.value(searchValue)
				if !searchKnown {
					return evaluator.domain.unknown(first, searchValue), true, nil
				}
				for _, item := range items {
					if githubEqual(item, search) {
						return evaluator.domain.derive(true, first, searchValue), true, nil
					}
				}
				return evaluator.domain.derive(false, first, searchValue), true, nil
			}
		}
		valueText, ok := expressionString(value)
		if !ok {
			return evaluator.domain.derive(false, first), true, nil
		}
		searchValue, err := evaluator.evaluate(node.Args[1])
		if err != nil {
			return zero, true, err
		}
		search, searchKnown := evaluator.domain.value(searchValue)
		if !searchKnown {
			return evaluator.domain.unknown(first, searchValue), true, nil
		}
		searchText, ok := expressionString(search)
		if !ok {
			return evaluator.domain.derive(false, first, searchValue), true, nil
		}
		valueText, searchText = strings.ToLower(valueText), strings.ToLower(searchText)
		switch name {
		case "startswith":
			return evaluator.domain.derive(strings.HasPrefix(valueText, searchText), first, searchValue), true, nil
		case "contains":
			return evaluator.domain.derive(strings.Contains(valueText, searchText), first, searchValue), true, nil
		default:
			return evaluator.domain.derive(strings.HasSuffix(valueText, searchText), first, searchValue), true, nil
		}
	case "format":
		if argc < 1 || argc > 255 {
			return zero, true, fmt.Errorf("function %q requires 1 to 255 arguments", node.Callee)
		}
		formatValue, err := evaluator.evaluate(node.Args[0])
		if err != nil {
			return zero, true, err
		}
		value, known := evaluator.domain.value(formatValue)
		if !known {
			values := []T{formatValue}
			for _, argument := range node.Args[1:] {
				value, err := evaluator.evaluate(argument)
				if err != nil {
					return zero, true, err
				}
				values = append(values, value)
			}
			return evaluator.domain.unknown(values...), true, nil
		}
		format, ok := expressionAggregateString(value)
		if !ok {
			return zero, true, fmt.Errorf("function %q cannot convert %T to a format string", node.Callee, value)
		}
		values := []T{formatValue}
		unknown := false
		formatted, err := expressionFormat(format, len(node.Args)-1, func(index int) (any, error) {
			value, err := evaluator.evaluate(node.Args[index+1])
			if err != nil {
				return nil, err
			}
			values = append(values, value)
			concrete, known := evaluator.domain.value(value)
			if !known {
				unknown = true
				return "", nil
			}
			return concrete, nil
		})
		if err != nil {
			return zero, true, err
		}
		if unknown {
			return evaluator.domain.unknown(values...), true, nil
		}
		return evaluator.domain.derive(formatted, values...), true, nil
	case "join":
		if argc != 1 && argc != 2 {
			return zero, true, fmt.Errorf("function %q requires 1 or 2 arguments", node.Callee)
		}
		first, err := evaluator.evaluate(node.Args[0])
		if err != nil {
			return zero, true, err
		}
		value, known := evaluator.domain.value(first)
		if !known {
			if argc == 1 {
				return evaluator.domain.unknown(first), true, nil
			}
			separator, err := evaluator.evaluate(node.Args[1])
			if err != nil {
				return zero, true, err
			}
			return evaluator.domain.unknown(first, separator), true, nil
		}
		items, ok := expressionCollection(value)
		if !ok {
			if reflected := reflect.ValueOf(value); reflected.IsValid() && reflected.Kind() == reflect.Map {
				return evaluator.domain.derive("", first), true, nil
			}
			joined, convertible := expressionString(value)
			if !convertible {
				return zero, true, fmt.Errorf("function %q cannot convert %T to a string", node.Callee, value)
			}
			return evaluator.domain.derive(joined, first), true, nil
		}
		separator := ","
		values := []T{first}
		if argc == 2 && len(items) > 1 {
			separatorValue, err := evaluator.evaluate(node.Args[1])
			if err != nil {
				return zero, true, err
			}
			value, separatorKnown := evaluator.domain.value(separatorValue)
			if !separatorKnown {
				return evaluator.domain.unknown(first, separatorValue), true, nil
			}
			values = append(values, separatorValue)
			separator, ok = expressionString(value)
			if !ok {
				separator = ","
			}
		}
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i], ok = expressionAggregateString(item)
			if !ok {
				return zero, true, fmt.Errorf("function %q item %d cannot convert %T to a string", node.Callee, i, item)
			}
		}
		return evaluator.domain.derive(strings.Join(parts, separator), values...), true, nil
	case "tojson":
		if argc != 1 {
			return zero, true, fmt.Errorf("function %q requires 1 argument", node.Callee)
		}
		argument, err := evaluator.evaluate(node.Args[0])
		if err != nil {
			return zero, true, err
		}
		value, known := evaluator.domain.value(argument)
		if !known {
			return evaluator.domain.unknown(argument), true, nil
		}
		encoded, err := encodeExpressionJSON(value)
		if err != nil {
			return zero, true, fmt.Errorf("function %q: %w", node.Callee, err)
		}
		return evaluator.domain.derive(encoded, argument), true, nil
	case "fromjson":
		if argc != 1 {
			return zero, true, fmt.Errorf("function %q requires 1 argument", node.Callee)
		}
		argument, err := evaluator.evaluate(node.Args[0])
		if err != nil {
			return zero, true, err
		}
		value, known := evaluator.domain.value(argument)
		if !known {
			return evaluator.domain.unknown(argument), true, nil
		}
		text, ok := expressionString(value)
		if !ok {
			return zero, true, fmt.Errorf("function %q cannot convert %T to a string", node.Callee, value)
		}
		decoded, err := decodeJSONValue(text)
		if err != nil {
			return zero, true, err
		}
		return evaluator.domain.derive(decoded, argument), true, nil
	default:
		return zero, false, nil
	}
}

func evaluateCaseFunction[T any](evaluator *expressionEvaluator[T], node *actionlint.FuncCallNode, index int) (T, error) {
	var zero T
	if index == len(node.Args)-1 {
		return evaluator.evaluate(node.Args[index])
	}
	predicate, err := evaluator.evaluate(node.Args[index])
	if err != nil {
		return zero, err
	}
	predicateValue, known := evaluator.domain.value(predicate)
	if !known {
		selected, err := evaluator.evaluate(node.Args[index+1])
		if err != nil {
			return zero, err
		}
		remainder, err := evaluateCaseFunction(evaluator, node, index+2)
		if err != nil {
			return zero, err
		}
		return evaluator.result(evaluator.domain.join(selected, remainder), predicate), nil
	}
	selected, ok := predicateValue.(bool)
	if !ok {
		return zero, fmt.Errorf("function %q predicate %d resolved to %T, want boolean", node.Callee, index/2+1, predicateValue)
	}
	if selected {
		value, err := evaluator.evaluate(node.Args[index+1])
		if err != nil {
			return zero, err
		}
		return evaluator.result(value, predicate), nil
	}
	remainder, err := evaluateCaseFunction(evaluator, node, index+2)
	if err != nil {
		return zero, err
	}
	return evaluator.result(remainder, predicate), nil
}

func expressionAggregateString(value any) (string, bool) {
	if text, ok := expressionString(value); ok {
		return text, true
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return "", true
	}
	switch reflected.Kind() {
	case reflect.Map:
		return "Object", true
	case reflect.Array, reflect.Slice:
		return "Array", true
	default:
		return "", false
	}
}

func encodeExpressionJSON(value any) (string, error) {
	value = expressionJSONNumbers(value)
	var encoded strings.Builder
	if err := writeExpressionJSON(&encoded, value, 0); err != nil {
		return "", err
	}
	return encoded.String(), nil
}

func writeExpressionJSON(encoded *strings.Builder, value any, depth int) error {
	indent := func(level int) { encoded.WriteString(strings.Repeat("  ", level)) }
	switch value := value.(type) {
	case json.Number:
		encoded.WriteString(value.String())
		return nil
	case []any:
		if len(value) == 0 {
			encoded.WriteString("[]")
			return nil
		}
		encoded.WriteString("[\n")
		for i, item := range value {
			indent(depth + 1)
			if err := writeExpressionJSON(encoded, item, depth+1); err != nil {
				return err
			}
			if i < len(value)-1 {
				encoded.WriteByte(',')
			}
			encoded.WriteByte('\n')
		}
		indent(depth)
		encoded.WriteByte(']')
		return nil
	case map[string]any:
		if len(value) == 0 {
			encoded.WriteString("{}")
			return nil
		}
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		encoded.WriteString("{\n")
		for i, key := range keys {
			indent(depth + 1)
			keyJSON, err := encodeExpressionJSONScalar(key)
			if err != nil {
				return err
			}
			encoded.WriteString(keyJSON)
			encoded.WriteString(": ")
			if err := writeExpressionJSON(encoded, value[key], depth+1); err != nil {
				return err
			}
			if i < len(keys)-1 {
				encoded.WriteByte(',')
			}
			encoded.WriteByte('\n')
		}
		indent(depth)
		encoded.WriteByte('}')
		return nil
	default:
		scalar, err := encodeExpressionJSONScalar(value)
		if err != nil {
			return err
		}
		encoded.WriteString(scalar)
		return nil
	}
}

func encodeExpressionJSONScalar(value any) (string, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(encoded.String(), "\n"), nil
}

func expressionJSONNumbers(value any) any {
	switch value := value.(type) {
	case float64:
		return json.Number(expressionNumberString(value))
	case float32:
		return json.Number(expressionNumberString(float64(value)))
	case int:
		return json.Number(expressionNumberString(float64(value)))
	case int64:
		return json.Number(expressionNumberString(float64(value)))
	case uint64:
		return json.Number(expressionNumberString(float64(value)))
	case json.Number:
		parsed, err := strconv.ParseFloat(value.String(), 64)
		if err == nil || math.IsInf(parsed, 0) {
			return json.Number(expressionNumberString(parsed))
		}
		return value
	case []any:
		result := make([]any, len(value))
		for i, item := range value {
			result[i] = expressionJSONNumbers(item)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(value))
		for name, item := range value {
			result[name] = expressionJSONNumbers(item)
		}
		return result
	default:
		reflected := reflect.ValueOf(value)
		if !reflected.IsValid() {
			return value
		}
		switch reflected.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return json.Number(expressionNumberString(float64(reflected.Int())))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return json.Number(expressionNumberString(float64(reflected.Uint())))
		case reflect.Float32, reflect.Float64:
			return json.Number(expressionNumberString(reflected.Float()))
		case reflect.Array, reflect.Slice:
			result := make([]any, reflected.Len())
			for i := range result {
				result[i] = expressionJSONNumbers(reflected.Index(i).Interface())
			}
			return result
		case reflect.Map:
			if reflected.Type().Key().Kind() != reflect.String {
				return value
			}
			result := make(map[string]any, reflected.Len())
			iterator := reflected.MapRange()
			for iterator.Next() {
				result[iterator.Key().String()] = expressionJSONNumbers(iterator.Value().Interface())
			}
			return result
		}
		return value
	}
}

func expressionCollection(value any) ([]any, bool) {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.Array && reflected.Kind() != reflect.Slice {
		return nil, false
	}
	items := make([]any, reflected.Len())
	for i := range items {
		items[i] = reflected.Index(i).Interface()
	}
	return items, true
}

func expressionString(value any) (string, bool) {
	switch value := value.(type) {
	case nil:
		return "", true
	case bool:
		return strconv.FormatBool(value), true
	case string:
		return value, true
	case json.Number:
		parsed, err := strconv.ParseFloat(value.String(), 64)
		if err != nil && !math.IsInf(parsed, 0) {
			return "", false
		}
		return expressionNumberString(parsed), true
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return "", true
	}
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return expressionNumberString(float64(reflected.Int())), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return expressionNumberString(float64(reflected.Uint())), true
	case reflect.Float32, reflect.Float64:
		return expressionNumberString(reflected.Float()), true
	default:
		return "", false
	}
}

func expressionNumberString(value float64) string {
	switch {
	case value == 0:
		return "0"
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	default:
		return strconv.FormatFloat(value, 'G', 15, 64)
	}
}

func expressionFormat(format string, valueCount int, value func(int) (any, error)) (string, error) {
	var formatted strings.Builder
	values := make(map[int]any)
	for i := 0; i < len(format); {
		switch {
		case strings.HasPrefix(format[i:], "{{"):
			formatted.WriteByte('{')
			i += 2
		case strings.HasPrefix(format[i:], "}}"):
			formatted.WriteByte('}')
			i += 2
		case format[i] == '{':
			end := strings.IndexByte(format[i+1:], '}')
			if end < 0 {
				return "", fmt.Errorf("format string contains an unmatched '{'")
			}
			end += i + 1
			indexText := format[i+1 : end]
			// GitHub requires placeholder indexes to be bare digits; signs
			// and whitespace are rejected, so strconv.Atoi alone is too loose.
			if indexText == "" || strings.ContainsFunc(indexText, func(r rune) bool { return r < '0' || r > '9' }) {
				return "", fmt.Errorf("format placeholder %q is invalid", format[i:end+1])
			}
			index, err := strconv.Atoi(indexText)
			if err != nil || index >= valueCount {
				return "", fmt.Errorf("format placeholder %q is invalid", format[i:end+1])
			}
			argument, exists := values[index]
			if !exists {
				argument, err = value(index)
				if err != nil {
					return "", err
				}
				values[index] = argument
			}
			text, ok := expressionAggregateString(argument)
			if !ok {
				return "", fmt.Errorf("format argument %d cannot convert %T to a string", index, argument)
			}
			formatted.WriteString(text)
			i = end + 1
		case format[i] == '}':
			return "", fmt.Errorf("format string contains an unmatched '}'")
		default:
			formatted.WriteByte(format[i])
			i++
		}
	}
	return formatted.String(), nil
}
