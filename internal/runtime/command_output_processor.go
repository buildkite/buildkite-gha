package runtime

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

const (
	workflowCommandTruncationNotice  = "\n\n---\n_Workflow command annotations truncated at the 1 MiB limit._\n"
	workflowWarningAnnotationHeading = "<h2 class=\"h4 mb2\">GitHub Actions warnings</h2>\n"
	workflowErrorAnnotationHeading   = "<h2 class=\"h4 mb2\">GitHub Actions errors</h2>\n"
	workflowCommandListHeading       = "<div class=\"mb2\">\n"
	workflowCommandListEnd           = "</div>\n"
)

// A command output processor consumes process output and runner messages. It
// masks logs, interprets GitHub workflow commands parsed in workflow_command.go,
// and collects bounded Buildkite annotations consistently across streams.
type commandOutputProcessor struct {
	mu              sync.Mutex
	stdout          io.Writer
	stderr          io.Writer
	masks           []string
	trustedWarnings workflowCommandAnnotationBuffer
	warnings        workflowCommandAnnotationBuffer
	errors          workflowCommandAnnotationBuffer
	stopToken       string
	discard         bool
}

type workflowCommandAnnotationBuffer struct {
	commands  []workflowCommandAnnotation
	rendered  int
	truncated bool
}

type workflowCommandAnnotation struct {
	file     string
	title    string
	location string
	message  string
}

type parsedWorkflowCommand struct {
	name       string
	properties map[string]string
	message    string
}

func newCommandOutputProcessor(stdout, stderr io.Writer) *commandOutputProcessor {
	return &commandOutputProcessor{stdout: stdout, stderr: stderr}
}

var errInvalidWorkflowCommandStopToken = errors.New("invalid ::stop-commands workflow command")

func (p *commandOutputProcessor) process(target io.Writer, line string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discard {
		return nil
	}
	command, isCommand := parseWorkflowCommand(line)
	if p.stopToken != "" {
		if isCommand && strings.EqualFold(command.name, p.stopToken) {
			p.stopToken = ""
		}
		p.writeMaskedLineLocked(target, line)
		return nil
	}
	if isCommand {
		switch {
		case strings.EqualFold(command.name, "add-mask"):
			p.addMaskLocked(command.message)
			return nil
		case strings.EqualFold(command.name, "stop-commands"):
			if !validWorkflowCommandStopToken(command.message) {
				const message = "invalid ::stop-commands token: token is empty or collides with a workflow command"
				p.appendWorkflowCommandLocked(&p.errors, workflowErrorAnnotationHeading, parsedWorkflowCommand{message: message})
				p.writeWorkflowCommandMessageLocked(target, "error", message)
				return errInvalidWorkflowCommandStopToken
			}
			p.stopToken = command.message
			if len(command.message) > 6 {
				p.addMaskLocked(command.message)
			}
			p.writeMaskedLineLocked(target, line)
			return nil
		case strings.EqualFold(command.name, "warning"):
			p.appendWorkflowCommandLocked(&p.warnings, workflowWarningAnnotationHeading, command)
			p.writeWorkflowCommandMessageLocked(target, "warning", command.message)
			return nil
		case strings.EqualFold(command.name, "error"):
			p.appendWorkflowCommandLocked(&p.errors, workflowErrorAnnotationHeading, command)
			p.writeWorkflowCommandMessageLocked(target, "error", command.message)
			return nil
		case strings.EqualFold(command.name, "group"):
			p.writeLogSectionLocked(target, command.message)
			return nil
		case strings.EqualFold(command.name, "endgroup"),
			strings.EqualFold(command.name, "debug"),
			strings.EqualFold(command.name, "add-matcher"),
			strings.EqualFold(command.name, "remove-matcher"):
			return nil
		}
	}
	if isLegacyPresentationCommand(line) {
		return nil
	}
	p.writeMaskedLineLocked(target, line)
	return nil
}

func (p *commandOutputProcessor) logSection(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeLogSectionLocked(p.stdout, name)
}

func (p *commandOutputProcessor) expandCurrentSection() {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintln(p.stdout, "^^^ +++")
}

func (p *commandOutputProcessor) writeLogSectionLocked(target io.Writer, name string) {
	name = sanitizeLogSectionText(p.maskTextLocked(name))
	if name != "" {
		_, _ = fmt.Fprintln(target, "--- "+name)
	}
}

func sanitizeLogSectionText(text string) string {
	text = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	return strings.TrimSpace(text)
}

func isLegacyPresentationCommand(line string) bool {
	line = strings.TrimLeftFunc(line, unicode.IsSpace)
	for _, command := range []string{"##[debug]", "##[add-matcher]", "##[remove-matcher]"} {
		if strings.HasPrefix(strings.ToLower(line), command) {
			return true
		}
	}
	return false
}

func validWorkflowCommandStopToken(token string) bool {
	if token == "" || strings.EqualFold(token, "pause-logging") {
		return false
	}
	for _, command := range []string{
		"add-mask", "add-matcher", "add-path", "debug", "echo", "endgroup", "error", "group",
		"internal-set-repo-path", "notice", "remove-matcher", "save-state", "set-env",
		"set-output", "set-repo-path", "stop-commands", "warning",
	} {
		if strings.EqualFold(token, command) {
			return false
		}
	}
	return true
}

func (p *commandOutputProcessor) writeWorkflowCommandMessageLocked(target io.Writer, severity, message string) {
	if message == "" {
		return
	}
	p.writeMaskedLineLocked(target, severity+": "+message)
}

// trustedWarning records a runner-owned warning even after untrusted action
// output has been suppressed, while preserving the job's registered masks.
func (p *commandOutputProcessor) trustedWarning(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendWorkflowCommandLocked(&p.trustedWarnings, workflowWarningAnnotationHeading, parsedWorkflowCommand{message: message})
	p.writeWorkflowCommandMessageLocked(p.stderr, "warning", message)
}

func (p *commandOutputProcessor) writeMaskedLineLocked(target io.Writer, line string) {
	_, _ = fmt.Fprintln(target, p.maskTextLocked(line))
}

func (p *commandOutputProcessor) writeLiteral(target io.Writer, line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.discard {
		p.writeMaskedLineLocked(target, line)
	}
}

func (p *commandOutputProcessor) maskTextLocked(text string) string {
	for _, mask := range p.masks {
		text = strings.ReplaceAll(text, mask, "***")
	}
	return text
}

func (p *commandOutputProcessor) appendWorkflowCommandLocked(buffer *workflowCommandAnnotationBuffer, heading string, command parsedWorkflowCommand) {
	annotation := workflowCommandAnnotation{
		file:     strings.Clone(commandText(command.properties["file"])),
		title:    strings.Clone(commandText(command.properties["title"])),
		location: strings.Clone(workflowCommandLocationLabel(command.properties)),
		message:  strings.Clone(commandText(command.message)),
	}
	p.appendWorkflowCommandAnnotationLocked(buffer, heading, annotation)
}

func (p *commandOutputProcessor) appendWorkflowCommandAnnotationLocked(buffer *workflowCommandAnnotationBuffer, heading string, command workflowCommandAnnotation) {
	if buffer.truncated {
		return
	}
	additional := len(renderWorkflowCommandListItem(command))
	if len(buffer.commands) == 0 {
		additional += len(heading) + len(workflowCommandListHeading) + len(workflowCommandListEnd)
	}
	if buffer.rendered+additional > maxJobAnnotationBytes-len(workflowCommandTruncationNotice) {
		buffer.truncated = true
		return
	}
	buffer.rendered += additional
	buffer.commands = append(buffer.commands, command)
}

func (p *commandOutputProcessor) suppress() {
	p.mu.Lock()
	p.discard = true
	p.mu.Unlock()
}

func (p *commandOutputProcessor) addMask(value string) {
	p.mu.Lock()
	p.addMaskLocked(value)
	p.mu.Unlock()
}

func (p *commandOutputProcessor) maskValues() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.masks...)
}

type scrubbedCommandError struct {
	cause   error
	message string
}

func (e scrubbedCommandError) Error() string { return e.message }
func (e scrubbedCommandError) Unwrap() error { return e.cause }

func (p *commandOutputProcessor) scrubError(err error) error {
	if err == nil {
		return nil
	}
	registered := p.maskValues()
	masks := make([]string, 0, len(registered)*2)
	for _, mask := range registered {
		if mask == "" {
			continue
		}
		masks = append(masks, mask)
		quoted := strconv.Quote(mask)
		escaped := quoted[1 : len(quoted)-1]
		if escaped != mask {
			masks = append(masks, escaped)
		}
	}
	sort.Slice(masks, func(i, j int) bool {
		if len(masks[i]) != len(masks[j]) {
			return len(masks[i]) > len(masks[j])
		}
		return masks[i] < masks[j]
	})
	message := err.Error()
	for _, mask := range masks {
		message = strings.ReplaceAll(message, mask, "***")
	}
	if message == err.Error() {
		return err
	}
	return scrubbedCommandError{cause: err, message: message}
}

func (p *commandOutputProcessor) workflowCommandAnnotations() (warnings string, warningsTruncated bool, errors string, errorsTruncated bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	masks := normalizedMasks(p.masks)
	var combinedWarnings workflowCommandAnnotationBuffer
	for _, command := range p.trustedWarnings.commands {
		p.appendWorkflowCommandAnnotationLocked(&combinedWarnings, workflowWarningAnnotationHeading, maskWorkflowCommandAnnotation(command, masks))
	}
	for _, command := range p.warnings.commands {
		p.appendWorkflowCommandAnnotationLocked(&combinedWarnings, workflowWarningAnnotationHeading, maskWorkflowCommandAnnotation(command, masks))
	}
	combinedWarnings.truncated = combinedWarnings.truncated || p.trustedWarnings.truncated || p.warnings.truncated
	warnings, warningsTruncated = renderWorkflowCommandAnnotation(workflowWarningAnnotationHeading, combinedWarnings.commands, combinedWarnings.truncated)
	var maskedErrors workflowCommandAnnotationBuffer
	for _, command := range p.errors.commands {
		p.appendWorkflowCommandAnnotationLocked(&maskedErrors, workflowErrorAnnotationHeading, maskWorkflowCommandAnnotation(command, masks))
	}
	maskedErrors.truncated = maskedErrors.truncated || p.errors.truncated
	errors, errorsTruncated = renderWorkflowCommandAnnotation(workflowErrorAnnotationHeading, maskedErrors.commands, maskedErrors.truncated)
	return warnings, warningsTruncated, errors, errorsTruncated
}

func (p *commandOutputProcessor) addMaskLocked(value string) {
	if value == "" {
		return
	}
	p.addMaskValueLocked(value)
	for line := range strings.SplitSeq(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if line != "" && line != value {
			p.addMaskValueLocked(line)
		}
	}
	sort.Slice(p.masks, func(i, j int) bool {
		if len(p.masks[i]) != len(p.masks[j]) {
			return len(p.masks[i]) > len(p.masks[j])
		}
		return p.masks[i] < p.masks[j]
	})
}

func (p *commandOutputProcessor) addMaskValueLocked(value string) {
	if slices.Contains(p.masks, value) {
		return
	}
	p.masks = append(p.masks, value)
}
