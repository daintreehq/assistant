package agent

import (
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
)

// Autonomous wake-up helpers. A terminal-watcher queue event means a supervised
// agent finished, is waiting, or failed — exactly when the assistant should look
// and react. Those events feed a normal, FULL-CAPABILITY turn: the wake reactor can
// inspect and report, AND take action (relay between agents with terminal.sendCommand,
// spawn, resolve the inbox item) — the per-call confirmation/tier gate, not a turn-wide
// read-only narrowing, decides what may mutate. This is what makes autonomous
// multi-agent orchestration (agent A finishes → wake → relay to agent B) possible.

// wakePromptPrefix is the literal opener of every BuildWakePrompt output — the
// model-facing "this was NOT typed by the user" framing. A wake turn is identified by the
// SendOptions.IsWake CHANNEL signal, not by matching this prefix, so the const is only the
// prompt opener (named once here rather than inlined in the parts slice below).
const wakePromptPrefix = "[automatic wake-up]"

// IsActionableWake reports whether a surfaced attention event should autonomously
// wake the model (run a turn) versus just appear in the inbox. Two sources
// qualify: a terminal-watcher event carrying a real terminalId, and an
// async-tool completion (the model started that work — its completion is
// exactly when it should look and continue). Model/user queue events can't
// trigger an autonomous turn.
func IsActionableWake(e domain.QueueEvent) bool {
	switch e.Source {
	case domain.SourceTerminalWatcher:
		return e.Target != nil && e.Target.TerminalID != ""
	case domain.SourceAsyncTool:
		return true
	default:
		return false
	}
}

// BuildWakePrompt builds the internal nudge fed to the model on an autonomous
// wake. The model's reaction is what surfaces, not this prompt. It partitions
// the burst by source: async-tool completions get their own framing + guidance
// (continue the task, read output only if needed, never re-run the operation),
// everything else keeps the watcher framing. A watcher-only burst renders
// byte-identically to the pre-async output — the templates are model-facing
// contract text. alreadySummarized carries terminal ids already reported this
// session (cross-burst memory) for the watcher branch's ack downgrade.
func BuildWakePrompt(events []domain.QueueEvent, alreadySummarized map[string]struct{}) string {
	var async, watcher []domain.QueueEvent
	for _, e := range events {
		if e.Source == domain.SourceAsyncTool {
			async = append(async, e)
		} else {
			watcher = append(watcher, e)
		}
	}
	if len(async) == 0 {
		return buildWatcherWakePrompt(watcher, alreadySummarized)
	}
	if len(watcher) == 0 {
		return buildAsyncWakePrompt(async)
	}
	// Mixed burst: the watcher prompt leads (its guidance covers inbox hygiene),
	// the async completions follow as their own clearly-framed section.
	return buildWatcherWakePrompt(watcher, alreadySummarized) + "\n\n" + asyncWakeSection(async)
}

// buildWatcherWakePrompt is the original watcher wake prompt, verbatim.
func buildWatcherWakePrompt(events []domain.QueueEvent, alreadySummarized map[string]struct{}) string {
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
		base := wakeEventLine(e, "event")
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
		guidance = "Decide what to do. If a watched terminal finished, is waiting for input, or failed, give the user a concise update — DEFAULT to terminal.summarize for a clean gist of what the agent said (its raw scrollback is garbled TUI noise and bloats your context); use terminal.extract to pull a specific field, and terminal.read only when the user needs the exact literal text. Relay the gist; do NOT paste raw terminal output. If it isn't worth acting on, say so in one line."
	} else {
		guidance = "Every event below is a terminal you have already reported this session — these are lifecycle transitions only. Acknowledge each in one short line; do NOT call terminal.read/terminal.summarize/terminal.extract again."
	}

	parts := []string{
		wakePromptPrefix + " A background watcher surfaced new activity while you were idle — this was NOT typed by the user.",
		guidance,
	}
	if anyNew && anyFollowUp {
		parts = append(parts, "Some events below are marked (already reported): you have already summarized that terminal this session. For those, do NOT summarize again — just acknowledge the transition in one short line.")
	}
	// Inbox hygiene — applies to BOTH the summarize and the acknowledge branch. A
	// watch that is OVER no longer needs the user's attention once you have reported
	// it, but its supervisor-promoted "attention" item lingers (and keeps the badge
	// lit) until something resolves it — so clear it now with queue.resolve. A finished
	// watch's watcher has already stopped ITSELF, so there is nothing to cancel.
	parts = append(parts, "When you have reported (or acknowledged) a watch that is OVER — an agent that finished/completed, or a terminal that exited — clear its inbox item with queue.resolve {\"id\":\"<the inbox id on that event's line>\"} so it stops counting as needing attention; its watcher already stopped itself, so there is nothing left to cancel. Do NOT resolve an item that still needs the user to act (an agent waiting on a question) — leave that one open.")
	parts = append(parts, "", "New events:")
	parts = append(parts, lines...)
	return strings.Join(parts, "\n")
}

// buildAsyncWakePrompt frames an async-only burst: operations the MODEL started
// (terminal.run.async / terminal.await.async) have completed, so the guidance is
// "continue the task", not "report a watched terminal".
func buildAsyncWakePrompt(events []domain.QueueEvent) string {
	return wakePromptPrefix + " Asynchronous operation(s) you started earlier have finished — this was NOT typed by the user.\n" +
		asyncWakeGuidance + "\n\n" + asyncWakeLines(events)
}

// asyncWakeSection renders the async completions as a section appended to a
// mixed watcher+async burst.
func asyncWakeSection(events []domain.QueueEvent) string {
	return "Also: asynchronous operation(s) you started earlier have finished.\n" +
		asyncWakeGuidance + "\n\n" + asyncWakeLines(events)
}

// asyncWakeGuidance is the model-facing async completion playbook. The outcome
// facts are already ON each event line (per-terminal status + exit codes), so
// the default action is to continue the task — output reads are for when the
// CONTENT is needed, and re-running the operation is never right.
const asyncWakeGuidance = "Each line below is one completion, with per-terminal outcomes inline. Pick up the task each operation was part of and continue it: report the outcome to the user concisely, and when you need the actual output, read it with terminal.summarize (default, clean gist) or terminal.extract (a specific field) — do NOT paste raw terminal output. If an outcome says a terminal is asking a question, answer it with terminal.sendCommand. Do NOT re-run the async operation, do NOT start a new wait on the same terminals, and do NOT call async.list to double-check — these events are authoritative. When you have handled a completion, resolve its inbox item with queue.resolve {\"id\":\"<the inbox id on that line>\"} so it stops counting as needing attention."

// wakeEventLine renders ONE event as the shared "- Title: Summary
// [terminal id] (inbox id)" line — the single formatter behind BOTH the watcher
// and the async wake branches, so the model always reads one consistent event
// format and guidance like "the inbox id on that line" can never drift out of
// sync with only one of the two renderings. The inbox id is surfaced so the
// reactor can resolve THIS exact item without a queue.digest hunt.
func wakeEventLine(e domain.QueueEvent, fallbackTitle string) string {
	title := e.Title
	if title == "" {
		title = fallbackTitle
	}
	base := "- " + title
	if e.Summary != "" {
		base += ": " + e.Summary
	}
	if e.Target != nil && e.Target.TerminalID != "" {
		base += " [terminal " + e.Target.TerminalID + "]"
	}
	if e.ID != "" {
		base += " (inbox " + e.ID + ")"
	}
	return base
}

// asyncWakeLines renders one shared-format line per completion event.
func asyncWakeLines(events []domain.QueueEvent) string {
	lines := make([]string, 0, len(events)+1)
	lines = append(lines, "Completed operations:")
	for _, e := range events {
		lines = append(lines, wakeEventLine(e, "async operation"))
	}
	return strings.Join(lines, "\n")
}

// wakeFailurePrefixes: a Send reply is a "non-result" (a turn that failed before
// delivering a real answer) iff it has one of these prefixes. Send never throws on
// a model-layer failure — it returns one of these sentinel strings. Wake reactors
// must treat such a reply as a failure and NOT record the terminals as summarized,
// or a transient outage would permanently downgrade later lifecycle events to acks
// and silently swallow the real summary.
var wakeFailurePrefixes = []string{
	"Model unavailable:",
	"Model rate-limited:",
	"Model error:",
	"Can't reach the Daintree assistant backend", // backend unreachable (classifyBackendError)
	"Tool projection failed:",
	"Stopped: called ",
	"Turn cancelled",
}

// IsWakeFailureReply reports whether a Send reply is a non-result.
//
// The stalled-turn sentinel is matched WHOLE (isStalledReply), not by prefix. A turn
// that hit its round budget and reported its plan is a real answer; only the fallback
// the engine emits when the closing round produced no prose at all is a non-result, and
// "Stopped after N rounds…" is far too plausible an opening for the former to be
// classified on its first two words.
func IsWakeFailureReply(reply string) bool {
	if isStalledReply(reply) {
		return true
	}
	for _, p := range wakeFailurePrefixes {
		if strings.HasPrefix(reply, p) {
			return true
		}
	}
	return false
}
