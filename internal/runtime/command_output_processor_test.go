package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestLiveLogMaskingPrefersLongestMatchInEitherRegistrationOrder(t *testing.T) {
	for _, masks := range [][]string{
		{"credential", "credential-with-suffix\nsecond-line"},
		{"credential-with-suffix\nsecond-line", "credential"},
	} {
		var logs bytes.Buffer
		processor := newCommandOutputProcessor(&logs, &logs)
		for _, mask := range masks {
			processor.addMask(mask)
		}
		processor.addMask("credential-with-suffix")

		processor.writeLiteral(&logs, "credential-with-suffix second-line")
		if got := logs.String(); got != "*** ***\n" {
			t.Fatalf("masks %q produced logs %q, want longest overlapping masks without fragments", masks, got)
		}
	}
}

func TestWorkflowCommandsProduceBoundedMaskedJobAnnotations(t *testing.T) {
	var stdout, stderr bytes.Buffer
	processor := newCommandOutputProcessor(&stdout, &stderr)
	_ = processor.process(&stdout, "::warning title=Unsafe <title>,file=cmd%2Cmain.go,line=12,endLine=12,col=3,endColumn=5::late-secret <late-secret-tag> <warning>")
	_ = processor.process(&stdout, "::add-mask::late-secret")
	_ = processor.process(&stdout, "::add-mask::<late-secret-tag>")
	_ = processor.process(&stderr, "::add-mask::early-secret")
	_ = processor.process(&stderr, "::error file=main.go,line=invalid,endLine=9,col=2,endColumn=4::early-secret <error>")
	_ = processor.process(&stdout, "::warning without a delimiter")

	warnings, warningsTruncated, commandErrors, errorsTruncated := processor.workflowCommandAnnotations()
	result := scrubJobResult(JobResult{
		WarningAnnotations: warnings, warningsTruncated: warningsTruncated,
		ErrorAnnotations: commandErrors, errorsTruncated: errorsTruncated,
	}, processor.maskValues())
	if warningsTruncated || errorsTruncated {
		t.Fatal("small workflow command annotations were truncated")
	}
	for _, secret := range []string{"late-secret", "late-secret-tag", "early-secret"} {
		if strings.Contains(result.WarningAnnotations, secret) || strings.Contains(result.ErrorAnnotations, secret) {
			t.Fatalf("annotations leaked %q: warnings = %q, errors = %q", secret, result.WarningAnnotations, result.ErrorAnnotations)
		}
	}
	for _, fragment := range []string{
		"<h2 class=\"h4 mb2\">GitHub Actions warnings</h2>\n<div class=\"mb2\">", `<div class="border-top border-gray py2"><div><strong>Unsafe &lt;title&gt;:</strong> *** *** &lt;warning&gt;</div>`, `<div class="mt1"><code>cmd,main.go:12:3–12:5</code></div>`,
	} {
		if !strings.Contains(result.WarningAnnotations, fragment) {
			t.Errorf("warning annotation lacks %q: %q", fragment, result.WarningAnnotations)
		}
	}
	for _, fragment := range []string{
		"<h2 class=\"h4 mb2\">GitHub Actions errors</h2>\n<div class=\"mb2\">", `<div class="mt1"><code>main.go:9:2–9:4</code></div>`, "*** &lt;error&gt;",
	} {
		if !strings.Contains(result.ErrorAnnotations, fragment) {
			t.Errorf("error annotation lacks %q: %q", fragment, result.ErrorAnnotations)
		}
	}
	if strings.Contains(stdout.String(), "::warning title=") || !strings.Contains(stdout.String(), "warning: late-secret <late-secret-tag> <warning>") || !strings.Contains(stdout.String(), "::warning without a delimiter") {
		t.Fatalf("stdout = %q, want rendered command message and ordinary malformed command", stdout.String())
	}
	if strings.Contains(stderr.String(), "::error") || !strings.Contains(stderr.String(), "error: *** <error>") {
		t.Fatalf("stderr = %q, want masked rendered error", stderr.String())
	}
}

func TestWorkflowCommandAnnotationsGroupRowsByFile(t *testing.T) {
	processor := newCommandOutputProcessor(io.Discard, io.Discard)
	for _, command := range []string{
		"::warning file=path/to/first.go,line=2,title=First::first message",
		"::warning file=second.go,line=7,col=3::second message",
		"::warning file=path/to/first.go,line=9::another first message",
		"::warning title=General::general message",
	} {
		_ = processor.process(io.Discard, command)
	}

	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if truncated {
		t.Fatal("small grouped annotation was truncated")
	}
	if first, second := strings.LastIndex(warnings, "first.go"), strings.Index(warnings, "second.go"); first < 0 || second < first || strings.Count(warnings, `class="border-top border-gray py2"`) != 4 {
		t.Fatalf("annotation did not retain row order within first-seen file groups: %q", warnings)
	}
	for _, item := range []string{
		"<div><strong>First:</strong> first message</div><div class=\"mt1\"><code>first.go:2</code></div>",
		"<div>another first message</div><div class=\"mt1\"><code>first.go:9</code></div>",
		"<div>second message</div><div class=\"mt1\"><code>second.go:7:3</code></div>",
		"<div><strong>General:</strong> general message</div><div class=\"mt1\">General</div>",
	} {
		if !strings.Contains(warnings, item) {
			t.Errorf("annotation lacks item %q: %q", item, warnings)
		}
	}
}

func TestWorkflowCommandAnnotationRetainsOnlyOwnedRenderedFields(t *testing.T) {
	processor := newCommandOutputProcessor(io.Discard, io.Discard)
	properties := map[string]string{
		"file": "main.go", "title": "Lint", "line": "7", "unknown": strings.Repeat("unused", 100_000),
	}
	processor.mu.Lock()
	processor.appendWorkflowCommandLocked(&processor.warnings, workflowWarningAnnotationHeading, parsedWorkflowCommand{properties: properties, message: "message"})
	processor.mu.Unlock()
	properties["file"] = "changed.go"
	properties["title"] = "Changed"

	if len(processor.warnings.commands) != 1 {
		t.Fatalf("retained commands = %d, want 1", len(processor.warnings.commands))
	}
	got := processor.warnings.commands[0]
	if got.file != "main.go" || got.title != "Lint" || got.location != "7" || got.message != "message" {
		t.Fatalf("retained annotation = %#v", got)
	}
	if processor.warnings.rendered >= len(properties["unknown"]) {
		t.Fatalf("rendered size %d retained unknown property bytes", processor.warnings.rendered)
	}
}

func TestWorkflowCommandLocationLabels(t *testing.T) {
	for _, test := range []struct {
		name       string
		properties map[string]string
		want       string
	}{
		{name: "line", properties: map[string]string{"line": "5"}, want: "5"},
		{name: "point", properties: map[string]string{"line": "5", "col": "3"}, want: "5:3"},
		{name: "same-line range", properties: map[string]string{"line": "5", "col": "3", "endcolumn": "8"}, want: "5:3–5:8"},
		{name: "explicit same point", properties: map[string]string{"line": "5", "endline": "5", "col": "3"}, want: "5:3"},
		{name: "multiline range", properties: map[string]string{"line": "5", "endline": "6", "col": "3", "endcolumn": "8"}, want: "5–6"},
		{name: "end line supplies start", properties: map[string]string{"endline": "5"}, want: "5"},
		{name: "reversed range", properties: map[string]string{"line": "5", "endline": "4"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := workflowCommandLocationLabel(test.properties); got != test.want {
				t.Fatalf("workflowCommandLocationLabel() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWorkflowPresentationCommandsUseBuildkiteSections(t *testing.T) {
	var logs bytes.Buffer
	processor := newCommandOutputProcessor(&logs, &logs)
	for _, line := range []string{
		"::add-mask::---",
		"::add-mask::+++",
		"::add-mask::secret",
		"::add-mask::foo\tbar",
		"::group::Compile secret%0A--- injected foo\tbar",
		"inside group",
		"::endgroup::",
		"::debug::modern debug",
		"::add-matcher::matcher.json",
		"::remove-matcher owner=test::",
		"##[debug]legacy debug",
		"##[add-matcher]legacy.json",
		"##[remove-matcher]test",
	} {
		if err := processor.process(&logs, line); err != nil {
			t.Fatalf("process(%q) error = %v", line, err)
		}
	}
	processor.expandCurrentSection()

	if got, want := logs.String(), "--- Compile *** *** injected ***\ninside group\n^^^ +++\n"; got != want {
		t.Fatalf("logs = %q, want %q", got, want)
	}
}

func TestWorkflowCommandStopTokenPreventsAccidentalAnnotations(t *testing.T) {
	var logs bytes.Buffer
	processor := newCommandOutputProcessor(&logs, &logs)
	_ = processor.process(&logs, "::stop-commands::workflow-stop-token")
	_ = processor.process(&logs, "::warning::untrusted warning-shaped output")
	_ = processor.process(&logs, "::workflow-stop-token::")
	_ = processor.process(&logs, "::warning::collected warning")

	warnings, truncated, commandErrors, _ := processor.workflowCommandAnnotations()
	if truncated || commandErrors != "" || strings.Contains(warnings, "untrusted warning-shaped output") || !strings.Contains(warnings, "collected warning") {
		t.Fatalf("workflow command annotations = %q, errors = %q, truncated = %v", warnings, commandErrors, truncated)
	}
	if !strings.Contains(logs.String(), "::warning::untrusted warning-shaped output") || strings.Contains(logs.String(), "::warning::collected warning") || strings.Contains(logs.String(), "workflow-stop-token") {
		t.Fatalf("logs = %q, want stopped command as masked ordinary output", logs.String())
	}
}

func TestWorkflowCommandStopTokenHandlesCRLFStreams(t *testing.T) {
	var logs bytes.Buffer
	processor := newCommandOutputProcessor(&logs, &logs)
	command := `printf '::stop-commands::crlf-stop-token\r\n'
printf '::warning::untrusted warning-shaped output\r\n'
printf '::crlf-stop-token::\r\n'
printf '::add-mask::crlf-secret\r\n'
printf 'masked after resume: crlf-secret\r\n'`
	if err := (Runner{}).runStreaming(t.Context(), processor, "", nil, "sh", "-c", command); err != nil {
		t.Fatalf("runStreaming() error = %v", err)
	}
	warnings, _, commandErrors, _ := processor.workflowCommandAnnotations()
	if warnings != "" || commandErrors != "" {
		t.Fatalf("workflow command annotations = %q, errors = %q", warnings, commandErrors)
	}
	if !strings.Contains(logs.String(), "::warning::untrusted warning-shaped output") || !strings.Contains(logs.String(), "masked after resume: ***") || strings.Contains(logs.String(), "crlf-secret") || strings.Contains(logs.String(), "\r") {
		t.Fatalf("logs = %q, want resumed masking with normalized CRLF records", logs.String())
	}
	commandWithEscapedCR, ok := parseWorkflowCommand("::warning::preserved%0D")
	if !ok || commandWithEscapedCR.message != "preserved\r" {
		t.Fatalf("parseWorkflowCommand() = %#v, %v, want escaped carriage return", commandWithEscapedCR, ok)
	}
}

func TestRunStreamingScopesWorkflowCommandFailuresToInvocation(t *testing.T) {
	processor := newCommandOutputProcessor(io.Discard, io.Discard)
	ready := filepath.Join(t.TempDir(), "clean-ready")
	release := filepath.Join(t.TempDir(), "release-clean")
	cleanDone := make(chan error, 1)
	go func() {
		cleanDone <- (Runner{}).runStreaming(t.Context(), processor, "", map[string]string{"READY": ready, "RELEASE": release}, "sh", "-c", `
: > "$READY"
while [ ! -e "$RELEASE" ]; do sleep .01; done
printf '%s\n' 'clean invocation completed'`)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("clean invocation did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	invalidErr := (Runner{}).runStreaming(t.Context(), processor, "", map[string]string{"RELEASE": release}, "sh", "-c", `
printf '%s\n' '::stop-commands::warning'
: > "$RELEASE"`)
	cleanErr := <-cleanDone
	if !errors.Is(invalidErr, errInvalidWorkflowCommandStopToken) {
		t.Fatalf("invalid runStreaming() error = %v", invalidErr)
	}
	if cleanErr != nil {
		t.Fatalf("overlapping clean runStreaming() inherited command failure: %v", cleanErr)
	}
}

func TestWorkflowCommandAnnotationsAreConcurrentAndUTF8Bounded(t *testing.T) {
	processor := newCommandOutputProcessor(io.Discard, io.Discard)
	var group sync.WaitGroup
	for worker := range 2 {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for message := range 50 {
				_ = processor.process(io.Discard, fmt.Sprintf("::warning::worker-%d-message-%d", worker, message))
			}
		}(worker)
	}
	group.Wait()
	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if truncated || strings.Count(warnings, `class="border-top border-gray py2"`) != 100 {
		t.Fatalf("concurrent warning annotation count = %d, truncated = %v", strings.Count(warnings, `class="border-top border-gray py2"`), truncated)
	}

	processor = newCommandOutputProcessor(io.Discard, io.Discard)
	_ = processor.process(io.Discard, "::warning::"+strings.Repeat("€", maxJobAnnotationBytes))
	warnings, truncated, _, _ = processor.workflowCommandAnnotations()
	result := scrubJobResult(JobResult{WarningAnnotations: warnings, warningsTruncated: truncated}, nil)
	if !truncated || len(result.WarningAnnotations) > maxJobAnnotationBytes || !utf8.ValidString(result.WarningAnnotations) || !strings.HasSuffix(result.WarningAnnotations, workflowCommandTruncationNotice) {
		t.Fatalf("bounded warnings bytes = %d, truncated = %v, valid UTF-8 = %v", len(result.WarningAnnotations), truncated, utf8.ValidString(result.WarningAnnotations))
	}
}

func TestWorkflowCommandAnnotationsNormalizeInvalidUTF8(t *testing.T) {
	processor := newCommandOutputProcessor(io.Discard, io.Discard)
	command := `printf '::warning title=bad\377,file=bad\376.go::bad\375\n'`
	if err := (Runner{}).runStreaming(t.Context(), processor, "", nil, "sh", "-c", command); err != nil {
		t.Fatalf("runStreaming() error = %v", err)
	}

	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if truncated || !utf8.ValidString(warnings) || strings.Count(warnings, "\uFFFD") != 3 {
		t.Fatalf("warning annotation = %q, truncated = %v, valid UTF-8 = %v", warnings, truncated, utf8.ValidString(warnings))
	}
	for _, fragment := range []string{`<div class="mt1"><code>bad�.go</code></div>`, "<div><strong>bad\uFFFD:</strong> bad\uFFFD</div>"} {
		if !strings.Contains(warnings, fragment) {
			t.Fatalf("warning annotation lacks %q: %q", fragment, warnings)
		}
	}
}

func TestWorkflowCommandAnnotationScrubbingPreservesUTF8(t *testing.T) {
	processor := newCommandOutputProcessor(io.Discard, io.Discard)
	_ = processor.process(io.Discard, "::warning::café")
	_ = processor.process(io.Discard, "::warning::masked "+string([]byte{0xC3}))
	_ = processor.process(io.Discard, "::add-mask::"+string([]byte{0xC3}))

	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	result := scrubJobResult(JobResult{WarningAnnotations: warnings, warningsTruncated: truncated}, processor.maskValues())
	if !utf8.ValidString(result.WarningAnnotations) || !strings.Contains(result.WarningAnnotations, "café") || !strings.Contains(result.WarningAnnotations, "masked ***") || strings.Contains(result.WarningAnnotations, "\uFFFD") {
		t.Fatalf("scrubbed warning annotation = %q, valid UTF-8 = %v", result.WarningAnnotations, utf8.ValidString(result.WarningAnnotations))
	}
}

func TestWorkflowCommandMasksCannotCorruptAnnotationMarkup(t *testing.T) {
	processor := newCommandOutputProcessor(io.Discard, io.Discard)
	_ = processor.process(io.Discard, "::warning file=table.go,title=tr::structured table text")
	_ = processor.process(io.Discard, "::add-mask::tr")
	_ = processor.process(io.Discard, "::add-mask::table")

	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if truncated || !strings.Contains(warnings, `<div class="mt1"><code>***.go</code></div>`) || !strings.Contains(warnings, "<div><strong>***:</strong> s***uctured *** text</div>") {
		t.Fatalf("masked warning annotation = %q, truncated = %v", warnings, truncated)
	}
	if strings.Count(warnings, `class="border-top border-gray py2"`) != 1 || strings.Count(warnings, "<div") != strings.Count(warnings, "</div>") {
		t.Fatalf("masks corrupted annotation markup: %q", warnings)
	}
}

func TestWorkflowCommandAnnotationsRemainBoundedAfterMaskExpansion(t *testing.T) {
	processor := newCommandOutputProcessor(io.Discard, io.Discard)
	for range 5000 {
		_ = processor.process(io.Discard, "::warning file=main.go::"+strings.Repeat("x", 100))
	}
	_ = processor.process(io.Discard, "::add-mask::x")

	warnings, truncated, _, _ := processor.workflowCommandAnnotations()
	if !truncated || strings.Contains(warnings, strings.Repeat("x", 100)) || !strings.HasSuffix(warnings, workflowCommandListEnd) || strings.Count(warnings, `class="border-top border-gray py2"`)*3+1 != strings.Count(warnings, "</div>") {
		t.Fatalf("expanded warning annotation bytes = %d, items = %d, closing divs = %d, truncated = %v", len(warnings), strings.Count(warnings, `class="border-top border-gray py2"`), strings.Count(warnings, "</div>"), truncated)
	}
	result := scrubJobResult(JobResult{WarningAnnotations: warnings, warningsTruncated: truncated}, processor.maskValues())
	if len(result.WarningAnnotations) > maxJobAnnotationBytes || !utf8.ValidString(result.WarningAnnotations) || !strings.HasSuffix(result.WarningAnnotations, workflowCommandTruncationNotice) {
		t.Fatalf("final warning annotation bytes = %d, valid UTF-8 = %v", len(result.WarningAnnotations), utf8.ValidString(result.WarningAnnotations))
	}
}
