package expression

import (
	"encoding/json"
	"fmt"
	"reflect"
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

func evaluatePureFunction(evaluator *semanticEvaluator, node *actionlint.FuncCallNode) (any, bool, error) {
	name := strings.ToLower(node.Callee)
	argc := len(node.Args)
	switch name {
	case "case":
		if argc < 3 || argc > 255 || argc%2 != 1 {
			return nil, true, fmt.Errorf("function %q requires 3 to 255 odd-numbered arguments", node.Callee)
		}
		for i := 0; i < argc-1; i += 2 {
			predicate, err := evaluator.evaluate(node.Args[i])
			if err != nil {
				return nil, true, err
			}
			selected, ok := predicate.(bool)
			if !ok {
				return nil, true, fmt.Errorf("function %q predicate %d resolved to %T, want boolean", node.Callee, i/2+1, predicate)
			}
			if selected {
				value, err := evaluator.evaluate(node.Args[i+1])
				return value, true, err
			}
		}
		value, err := evaluator.evaluate(node.Args[argc-1])
		return value, true, err
	case "startswith", "contains", "endswith":
		if argc != 2 {
			return nil, true, fmt.Errorf("function %q requires 2 arguments", node.Callee)
		}
		value, err := evaluator.evaluate(node.Args[0])
		if err != nil {
			return nil, true, err
		}
		if name == "contains" {
			if items, ok := expressionCollection(value); ok {
				if len(items) == 0 {
					return false, true, nil
				}
				search, err := evaluator.evaluate(node.Args[1])
				if err != nil {
					return nil, true, err
				}
				for _, item := range items {
					if githubEqual(item, search) {
						return true, true, nil
					}
				}
				return false, true, nil
			}
		}
		valueText, ok := expressionString(value)
		if !ok {
			return false, true, nil
		}
		search, err := evaluator.evaluate(node.Args[1])
		if err != nil {
			return nil, true, err
		}
		searchText, ok := expressionString(search)
		if !ok {
			return false, true, nil
		}
		valueText, searchText = strings.ToLower(valueText), strings.ToLower(searchText)
		switch name {
		case "startswith":
			return strings.HasPrefix(valueText, searchText), true, nil
		case "contains":
			return strings.Contains(valueText, searchText), true, nil
		default:
			return strings.HasSuffix(valueText, searchText), true, nil
		}
	case "format":
		if argc < 1 || argc > 255 {
			return nil, true, fmt.Errorf("function %q requires 1 to 255 arguments", node.Callee)
		}
		value, err := evaluator.evaluate(node.Args[0])
		if err != nil {
			return nil, true, err
		}
		format, ok := expressionString(value)
		if !ok {
			return nil, true, fmt.Errorf("function %q cannot convert %T to a format string", node.Callee, value)
		}
		formatted, err := expressionFormat(format, len(node.Args)-1, func(index int) (any, error) {
			return evaluator.evaluate(node.Args[index+1])
		})
		return formatted, true, err
	case "join":
		if argc != 1 && argc != 2 {
			return nil, true, fmt.Errorf("function %q requires 1 or 2 arguments", node.Callee)
		}
		value, err := evaluator.evaluate(node.Args[0])
		if err != nil {
			return nil, true, err
		}
		items, ok := expressionCollection(value)
		if !ok {
			joined, convertible := expressionString(value)
			if !convertible {
				return nil, true, fmt.Errorf("function %q cannot convert %T to a string", node.Callee, value)
			}
			return joined, true, nil
		}
		separator := ","
		if argc == 2 {
			value, err := evaluator.evaluate(node.Args[1])
			if err != nil {
				return nil, true, err
			}
			separator, ok = expressionString(value)
			if !ok {
				return nil, true, fmt.Errorf("function %q separator cannot convert %T to a string", node.Callee, value)
			}
		}
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i], ok = expressionString(item)
			if !ok {
				return nil, true, fmt.Errorf("function %q item %d cannot convert %T to a string", node.Callee, i, item)
			}
		}
		return strings.Join(parts, separator), true, nil
	case "tojson":
		if argc != 1 {
			return nil, true, fmt.Errorf("function %q requires 1 argument", node.Callee)
		}
		value, err := evaluator.evaluate(node.Args[0])
		if err != nil {
			return nil, true, err
		}
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return nil, true, fmt.Errorf("function %q: %w", node.Callee, err)
		}
		return string(encoded), true, nil
	case "fromjson":
		if argc != 1 {
			return nil, true, fmt.Errorf("function %q requires 1 argument", node.Callee)
		}
		value, err := evaluator.evaluate(node.Args[0])
		if err != nil {
			return nil, true, err
		}
		text, ok := expressionString(value)
		if !ok {
			return nil, true, fmt.Errorf("function %q cannot convert %T to a string", node.Callee, value)
		}
		decoded, err := decodeJSONValue(text)
		return decoded, true, err
	default:
		return nil, false, nil
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
		return value.String(), true
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return "", true
	}
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(reflected.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(reflected.Uint(), 10), true
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(reflected.Float(), 'g', -1, reflected.Type().Bits()), true
	default:
		return "", false
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
			index, err := strconv.Atoi(format[i+1 : end])
			if err != nil || index < 0 || index >= valueCount {
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
			text, ok := expressionString(argument)
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
