package runtime

import (
	"maps"
	"strings"
	"testing"
)

func TestWorkflowCommandParsingIsCaseInsensitiveAndExact(t *testing.T) {
	mask, ok := parseWorkflowCommand(" \t::ADD-MASK::secret%250Avalue")
	if !ok || !strings.EqualFold(mask.name, "add-mask") || mask.message != "secret%0Avalue" {
		t.Fatalf("parseWorkflowCommand() = %#v, %v", mask, ok)
	}
	extra, ok := parseWorkflowCommand("::add-mask-extra::secret")
	if !ok || strings.EqualFold(extra.name, "add-mask") {
		t.Fatalf("parseWorkflowCommand() accepted %q as add-mask", extra.name)
	}
	command, ok := parseWorkflowCommand("::WaRnInG title=Deploy%3A prod,file=src%2Cmain.go,line=12,endLine=12,col=3,endColumn=5,broken,unknown=value::first%0Asecond%250A")
	if !ok || !strings.EqualFold(command.name, "warning") || command.message != "first\nsecond%0A" {
		t.Fatalf("parseWorkflowCommand() = %#v, %v", command, ok)
	}
	wantProperties := map[string]string{
		"title": "Deploy: prod", "file": "src,main.go", "line": "12", "endline": "12", "col": "3", "endcolumn": "5", "unknown": "value",
	}
	if !maps.Equal(command.properties, wantProperties) {
		t.Fatalf("properties = %#v, want %#v", command.properties, wantProperties)
	}
	for _, malformed := range []string{"", "prefix::warning::message", "::warning without a delimiter", "::::message"} {
		if _, ok := parseWorkflowCommand(malformed); ok {
			t.Fatalf("parseWorkflowCommand(%q) succeeded", malformed)
		}
	}
}
