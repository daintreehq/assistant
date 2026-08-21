package supervisor

import (
	"regexp"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// toolNamePattern extracts dotted tool ids from rendered prompt text. It is the twin of
// wakePromptToolNamePattern in internal/agent/wake_test.go, which carries the full
// rationale; the two are duplicated rather than shared because the assembled daemon
// prompt is built HERE (BuildWakePrompt + unattendedWakeNote) while the branch coverage
// lives over there. Sharing one copy would mean a non-_test.go support package (an
// _test.go helper cannot be imported) — more machinery than one regexp is worth. Change
// the two together.
var toolNamePattern = regexp.MustCompile(`\b[a-z][A-Za-z0-9_-]*(?:\.[a-z][A-Za-z0-9_-]*)+\b`)

// noteProhibitions are the COMPLETE prohibition clauses whose tool ids the note tells
// the model not to call, so this prompt gives them no claim on coreToolNames. Masking
// the whole clause rather than the bare name keeps the exemption honest: reword it into
// an instruction to CALL the tool, or weaken it with a trailing "unless …", and the
// surviving occurrence is extracted and checked like any other.
var noteProhibitions = []string{
	"Never call user.askMultipleChoice here;",
}

// TestUnattendedWakeNoteNamesOnlyCoreTools is the daemon half of issue #370's guard.
// Every daemon wake sends BuildWakePrompt's output PLUS unattendedWakeNote (reactWake),
// so the note is part of the autonomous wake prompt and inherits the same rule: a tool
// it tells the model to call must be in coreToolNames, because an unattended wake runs
// with nobody present and may carry no relevant skill to reintroduce a missing tool.
// The agent-side test covers BuildWakePrompt's branches; this covers the assembled text.
func TestUnattendedWakeNoteNamesOnlyCoreTools(t *testing.T) {
	// Dot-free fixture: event fields render verbatim, so a tool-shaped value here would
	// be indistinguishable from a real instruction.
	event := domain.QueueEvent{
		ID:       "inbox-1",
		Source:   domain.SourceTerminalWatcher,
		Severity: domain.SeverityAttention,
		Title:    "supervised finished",
		Summary:  "agent finished cleanly",
		Target:   &domain.EventTarget{TerminalID: "term-1"},
	}
	prompt := agent.BuildWakePrompt([]domain.QueueEvent{event}, nil) + unattendedWakeNote

	if !strings.Contains(prompt, "[unattended]") {
		t.Fatalf("the unattended note did not ride the wake prompt:\n%s", prompt)
	}

	core := make(map[string]struct{})
	for _, name := range agent.CoreToolNames() {
		core[name] = struct{}{}
	}

	seen := make(map[string]struct{})
	scrubbed := prompt
	for _, phrase := range noteProhibitions {
		if strings.Contains(scrubbed, phrase) {
			seen[phrase] = struct{}{}
			scrubbed = strings.ReplaceAll(scrubbed, phrase, " ")
		}
	}
	var missing []string
	for _, name := range toolNamePattern.FindAllString(scrubbed, -1) {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := core[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// Mirrors AssertRegistered's diagnostic: name every offender, not just the first.
		t.Errorf("the daemon wake prompt names tools that are not in coreToolNames: %s\n%s",
			strings.Join(missing, ", "), prompt)
	}

	// A prohibition that gets reworded must not leave a silent mask behind — that would
	// hide the next real drift instead of reporting it.
	for _, phrase := range noteProhibitions {
		if _, ok := seen[phrase]; !ok {
			t.Errorf("prohibition no longer rendered verbatim (re-check the wording, then drop or update it): %q", phrase)
		}
	}
}
