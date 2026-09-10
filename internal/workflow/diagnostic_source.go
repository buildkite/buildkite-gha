package workflow

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"
)

const (
	maxDiagnosticSourceSize = 1 << 20
	maxDiagnosticLineSize   = 240
	maxDiagnosticCopySize   = 16 << 10
)

var (
	usesReferencePattern = regexp.MustCompile(`^(?:\./[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*|[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*@[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*)$`)
	runnerLabelPattern   = regexp.MustCompile(`^(?:ubuntu|windows|macos)-(?:latest|[0-9]+(?:\.[0-9]+)?)(?:-(?:large|xlarge|arm|arm64))?$`)
)

// DiagnosticSource retains only configuration lines eligible for display.
type DiagnosticSource struct {
	lines map[int]string
}

// CaptureDiagnosticSource retains a deliberately narrow set of safe workflow
// configuration lines for diagnostics.
func CaptureDiagnosticSource(source []byte) *DiagnosticSource {
	if len(source) > maxDiagnosticSourceSize {
		return nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil || len(document.Content) != 1 {
		return nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode || root.Style&yaml.FlowStyle != 0 || root.Alias != nil || root.Anchor != "" {
		return nil
	}
	physical := bytes.Split(source, []byte{'\n'})
	type candidate struct {
		key  string
		node *yaml.Node
		step bool
	}
	var candidates []candidate
	add := func(key string, node *yaml.Node, valid func(string) bool, step bool) {
		if node == nil || node.Kind != yaml.ScalarNode || node.Alias != nil || node.Anchor != "" || node.Style&(yaml.FlowStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 || !valid(node.Value) {
			return
		}
		candidates = append(candidates, candidate{key: key, node: node, step: step})
	}

	for _, job := range orderedMappingValues(mappingEntries(root)["jobs"]) {
		if job.Kind != yaml.MappingNode || job.Style&yaml.FlowStyle != 0 || job.Alias != nil || job.Anchor != "" {
			continue
		}
		fields := mappingEntries(job)
		add("uses", fields["uses"], validUses, false)
		add("runs-on", fields["runs-on"], validRunner, false)
		steps := fields["steps"]
		if steps == nil || steps.Kind != yaml.SequenceNode || steps.Style&yaml.FlowStyle != 0 || steps.Alias != nil || steps.Anchor != "" {
			continue
		}
		for _, item := range steps.Content {
			if item.Kind != yaml.MappingNode || item.Style&yaml.FlowStyle != 0 || item.Alias != nil || item.Anchor != "" {
				continue
			}
			stepFields := mappingEntries(item)
			add("uses", stepFields["uses"], validUses, true)
			add("shell", stepFields["shell"], validShell, true)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].node.Line < candidates[j].node.Line })
	eligible := make(map[int]string)
	copied := 0
	for _, candidate := range candidates {
		line := candidate.node.Line
		if line < 1 || line > len(physical) {
			continue
		}
		raw := bytes.TrimSuffix(physical[line-1], []byte{'\r'})
		if len(raw) > maxDiagnosticLineSize || !physicalField(raw, candidate.key, candidate.node.Value, candidate.step) || copied+len(raw) > maxDiagnosticCopySize {
			continue
		}
		eligible[line] = strings.Clone(string(raw))
		copied += len(raw)
	}
	if len(eligible) == 0 {
		return nil
	}
	return &DiagnosticSource{lines: eligible}
}

func orderedMappingValues(node *yaml.Node) []*yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode || node.Style&yaml.FlowStyle != 0 || node.Alias != nil || node.Anchor != "" {
		return nil
	}
	values := make([]*yaml.Node, 0, len(node.Content)/2)
	for i := 1; i < len(node.Content); i += 2 {
		values = append(values, node.Content[i])
	}
	return values
}

func validUses(value string) bool   { return usesReferencePattern.MatchString(value) }
func validRunner(value string) bool { return runnerLabelPattern.MatchString(value) }
func validShell(value string) bool {
	switch value {
	case "bash", "sh", "pwsh", "powershell", "cmd", "python":
		return true
	default:
		return false
	}
}

func physicalField(line []byte, key, value string, step bool) bool {
	for _, b := range line {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	trimmed := strings.TrimSpace(string(line))
	if step && strings.HasPrefix(trimmed, "- ") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
	}
	prefix := key + ":"
	if !strings.HasPrefix(trimmed, prefix) {
		return false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
	if raw == value {
		return true
	}
	if len(raw) < 2 || raw[0] != raw[len(raw)-1] || (raw[0] != '\'' && raw[0] != '"') {
		return false
	}
	inner := raw[1 : len(raw)-1]
	return inner == value && !strings.ContainsAny(inner, `\\`)
}

// Excerpt returns the target line and at most one adjacent eligible line on
// either side. It returns an empty string when the target is not eligible.
func (source *DiagnosticSource) Excerpt(line int) string {
	if source == nil || source.lines == nil {
		return ""
	}
	if _, ok := source.lines[line]; !ok {
		return ""
	}
	first, last := line, line
	if _, ok := source.lines[line-1]; ok {
		first--
	}
	if _, ok := source.lines[line+1]; ok {
		last++
	}
	width := len(fmt.Sprint(last))
	var excerpt strings.Builder
	for number := first; number <= last; number++ {
		text, ok := source.lines[number]
		if !ok {
			continue
		}
		marker := " "
		if number == line {
			marker = ">"
		}
		fmt.Fprintf(&excerpt, "%s %*d | %s", marker, width, number, text)
		if number != last {
			excerpt.WriteByte('\n')
		}
	}
	return excerpt.String()
}
