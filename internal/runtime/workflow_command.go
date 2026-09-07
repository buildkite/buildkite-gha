package runtime

import (
	"html"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

func parseWorkflowCommand(line string) (parsedWorkflowCommand, bool) {
	line = strings.TrimLeftFunc(line, unicode.IsSpace)
	if !strings.HasPrefix(line, "::") {
		return parsedWorkflowCommand{}, false
	}
	separator := strings.Index(line[2:], "::")
	if separator < 0 {
		return parsedWorkflowCommand{}, false
	}
	separator += 2
	header := line[2:separator]
	name, propertyList, hasProperties := strings.Cut(header, " ")
	if name == "" {
		return parsedWorkflowCommand{}, false
	}
	properties := map[string]string{}
	if hasProperties {
		for property := range strings.SplitSeq(strings.TrimSpace(propertyList), ",") {
			name, value, ok := strings.Cut(property, "=")
			if !ok || name == "" || value == "" {
				continue
			}
			properties[strings.ToLower(name)] = decodeCommandProperty(value)
		}
	}
	return parsedWorkflowCommand{name: name, properties: properties, message: decodeCommandValue(line[separator+2:])}, true
}

func decodeCommandValue(value string) string {
	replacer := strings.NewReplacer("%0D", "\r", "%0A", "\n", "%25", "%")
	return replacer.Replace(value)
}

func decodeCommandProperty(value string) string {
	replacer := strings.NewReplacer("%0D", "\r", "%0A", "\n", "%3A", ":", "%2C", ",", "%25", "%")
	return replacer.Replace(value)
}

func renderWorkflowCommandAnnotation(heading string, commands []workflowCommandAnnotation, truncated bool) (string, bool) {
	if len(commands) == 0 {
		return "", truncated
	}
	type commandGroup struct {
		file     string
		commands []workflowCommandAnnotation
	}
	groups := make([]commandGroup, 0)
	groupIndexes := make(map[string]int)
	for _, command := range commands {
		file := command.file
		index, ok := groupIndexes[file]
		if !ok {
			index = len(groups)
			groupIndexes[file] = index
			groups = append(groups, commandGroup{file: file})
		}
		groups[index].commands = append(groups[index].commands, command)
	}

	var rendered strings.Builder
	rendered.WriteString(heading)
	rendered.WriteString(workflowCommandListHeading)
	for _, group := range groups {
		for _, command := range group.commands {
			rendered.WriteString(renderWorkflowCommandListItem(command))
		}
	}
	rendered.WriteString(workflowCommandListEnd)

	var body string
	var bodyTruncated bool
	appendBoundedText(&body, &bodyTruncated, rendered.String(), truncated, maxJobAnnotationBytes, workflowCommandTruncationNotice)
	return body, bodyTruncated
}

func renderWorkflowCommandListItem(command workflowCommandAnnotation) string {
	source := "General"
	if command.file != "" {
		location := filepath.Base(strings.ReplaceAll(command.file, "\\", "/"))
		if command.location != "" {
			location += ":" + command.location
		}
		source = "<code>" + commandHTML(location) + "</code>"
	}
	detail := commandHTML(command.message)
	if command.title != "" {
		detail = "<strong>" + commandHTML(command.title) + ":</strong> " + detail
	}
	return "<div class=\"border-top border-gray py2\"><div>" + detail +
		"</div><div class=\"mt1\">" + source + "</div></div>\n"
}

func workflowCommandLocationLabel(properties map[string]string) string {
	line, endLine, column, endColumn := workflowCommandLocation(properties)
	if line == "" {
		return ""
	}
	if endLine == "" {
		endLine = line
	}
	if endColumn == "" {
		endColumn = column
	}
	start := line
	if column != "" {
		start += ":" + column
	}
	end := endLine
	if endColumn != "" {
		end += ":" + endColumn
	}
	if end == "" || end == start || endLine == line && endColumn == column {
		return start
	}
	return start + "–" + end
}

func normalizedMasks(masks []string) []string {
	normalized := make([]string, 0, len(masks))
	for _, mask := range masks {
		if mask = commandText(mask); mask != "" {
			normalized = append(normalized, mask)
		}
	}
	sort.Slice(normalized, func(i, j int) bool {
		if len(normalized[i]) != len(normalized[j]) {
			return len(normalized[i]) > len(normalized[j])
		}
		return normalized[i] < normalized[j]
	})
	return normalized
}

func maskWorkflowCommandAnnotation(command workflowCommandAnnotation, masks []string) workflowCommandAnnotation {
	values := []*string{&command.file, &command.title, &command.location, &command.message}
	for _, mask := range masks {
		for _, value := range values {
			*value = strings.ReplaceAll(*value, mask, "***")
		}
	}
	return command
}

func commandText(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\r", "\n")
}

func commandHTML(value string) string {
	value = commandText(value)
	return strings.ReplaceAll(html.EscapeString(value), "\n", "<br>\n")
}

func workflowCommandLocation(properties map[string]string) (line, endLine, column, endColumn string) {
	line, lineNumberOK := workflowCommandCoordinate(properties["line"])
	endLine, endLineNumberOK := workflowCommandCoordinate(properties["endline"])
	column, columnNumberOK := workflowCommandCoordinate(properties["col"])
	endColumn, endColumnNumberOK := workflowCommandCoordinate(properties["endcolumn"])
	lineProperty, endLineProperty := properties["line"], properties["endline"]

	if !lineNumberOK && endLineNumberOK {
		line, lineNumberOK = endLine, true
		lineProperty = endLineProperty
	}
	if !columnNumberOK && endColumnNumberOK {
		column, columnNumberOK = endColumn, true
	}
	if !lineNumberOK && (columnNumberOK || endColumnNumberOK) {
		column, endColumn = "", ""
		columnNumberOK, endColumnNumberOK = false, false
	}
	// Match actions/runner's original-property comparison: textual forms such
	// as line=01,endLine=1 describe different lines for column-range purposes.
	if lineNumberOK && endLineNumberOK && lineProperty != endLineProperty {
		column, endColumn = "", ""
		columnNumberOK, endColumnNumberOK = false, false
	}
	if lineNumberOK && endLineNumberOK && coordinateNumber(endLine) < coordinateNumber(line) {
		line, endLine = "", ""
	}
	if columnNumberOK && endColumnNumberOK && coordinateNumber(endColumn) < coordinateNumber(column) {
		column, endColumn = "", ""
	}
	return line, endLine, column, endColumn
}

func workflowCommandCoordinate(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	if _, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32); err != nil {
		return "", false
	}
	return value, true
}

func coordinateNumber(value string) int64 {
	number, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
	return number
}
