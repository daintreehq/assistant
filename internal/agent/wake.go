package agent

import (
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Autonomous wake-up helpers. A terminal-watcher queue event means a supervised
// agent finished, is waiting, or failed — exactly when the assistant should look
// and report. Those events feed a READ-ONLY turn (Send with ReadOnly) so a
// background trigger can inspect and report but never run a mutating tool
// unattended.

// IsActionableWake reports whether a surfaced attention event should autonomously
// wake the model (run a turn) versus just appear in the inbox. Only a
// terminal-watcher event carrying a real terminalId qualifies — model/user
// queue events can't trigger an autonomous turn.
func IsActionableWake(e domain.QueueEvent) bool {
	return e.Source == domain.SourceTerminalWatcher && e.Target != nil && e.Target.TerminalID != ""
}

// BuildWakePrompt builds the internal nudge fed to the model on a watcher wake. It
// is sent as a read-only turn; the model's reaction is what surfaces, not this
// prompt. alreadySummarized carries terminal ids already reported this session
// (cross-burst memory): a terminal already in the set is downgraded to a one-line
// ack so a lifecycle that surfaces several events (waiting_for_input then
// terminal_exited) is summarized once, not two or three times. Reproduce
// the templates verbatim — the "(already reported …)" text is model-facing.
func BuildWakePrompt(events []domain.QueueEvent, alreadySummarized map[string]struct{}) string {
	// Seed from the caller's cross-burst memory, then grow locally so a terminal
	// appearing twice within THIS batch earns a full summary only on its first line.
	seen := make(map[string]struct{}, len(alreadySummarized))
	for id := range alreadySummarized {
		seen[id] = struct{}{}
	}
	anyFollowUp := false
	anyNew := false
	lines := make([]string, 0, len(events))
	for _, e := range events {
		terminalID := ""
		if e.Target != nil {
			terminalID = e.Target.TerminalID
		}
		title := e.Title
		if title == "" {
			title = "event"
		}
		term := ""
		if terminalID != "" {
			term = " [terminal " + terminalID + "]"
		}
		base := "- " + title
		if e.Summary != "" {
			base += ": " + e.Summary
		}
		base += term
		if terminalID != "" {
			if _, ok := seen[terminalID]; ok {
				anyFollowUp = true
				lines = append(lines, base+" (already reported — acknowledge in one line, do NOT call terminal.read/terminal.summarize/terminal.extract again)")
				continue
			}
			seen[terminalID] = struct{}{}
		}
		anyNew = true
		lines = append(lines, base)
	}

	// The positive "read and summarize" guidance only applies when something new is
	// present; an all-follow-up batch swaps in acknowledge-only guidance instead.
	var guidance string
	if anyNew {
		guidance = "Decide what to do. If a watched terminal finished, is waiting for input, or failed, read it and give the user a concise update — use terminal.read to relay what the agent said verbatim, terminal.summarize for a gist, or terminal.extract to pull a specific field. If it isn't worth acting on, say so in one line."
	} else {
		guidance = "Every event below is a terminal you have already reported this session — these are lifecycle transitions only. Acknowledge each in one short line; do NOT call terminal.read/terminal.summarize/terminal.extract again."
	}

	parts := []string{
		"[automatic wake-up] A background watcher surfaced new activity while you were idle — this was NOT typed by the user.",
		guidance,
	}
	if anyNew && anyFollowUp {
		parts = append(parts, "Some events below are marked (already reported): you have already summarized that terminal this session. For those, do NOT summarize again — just acknowledge the transition in one short line.")
	}
	parts = append(parts, "", "New events:")
	parts = append(parts, lines...)
	return strings.Join(parts, "\n")
}

// wakeFailurePrefixes: a Send reply is a "non-result" (a turn that failed before
// delivering a real answer) iff it has one of these prefixes. Send never throws on
// a model-layer failure — it returns one of these sentinel strings. Wake reactors
// must treat such a reply as a failure and NOT record the terminals as summarized,
// or a transient outage would permanently downgrade later lifecycle events to acks
// and silently swallow the real summary.
var wakeFailurePrefixes = []string{
	"Model unavailable:",
	"Model error:",
	"Tool projection failed:",
	"Reached the tool-iteration limit",
	"Stopped: called ",
	"Turn cancelled",
}

// IsWakeFailureReply reports whether a Send reply is a non-result (prefix match).
func IsWakeFailureReply(reply string) bool {
	for _, p := range wakeFailurePrefixes {
		if strings.HasPrefix(reply, p) {
			return true
		}
	}
	return false
}
