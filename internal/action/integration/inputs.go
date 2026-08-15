package integration

import (
	"sort"
	"strings"
)

func actionBoolean(value string) bool {
	return actionTrue(value) || actionFalse(value)
}

func actionTrue(value string) bool {
	return value == "true" || value == "True" || value == "TRUE"
}

func actionFalse(value string) bool {
	return value == "false" || value == "False" || value == "FALSE"
}

func sortedNames(inputs map[string]string) []string {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func inputFold(inputs map[string]string, wanted string) (string, bool) {
	for name, value := range inputs {
		if strings.EqualFold(name, wanted) {
			return value, true
		}
	}
	return "", false
}
