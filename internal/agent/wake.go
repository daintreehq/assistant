package agent

import (
	"encoding/json"
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

// TimerMessageEventIDs returns the inbox ids of the scheduled messages in a burst, so
// a reactor can close exactly the errands its turn just carried out — and nothing else.
func TimerMessageEventIDs(events []domain.QueueEvent) []string {
	var ids []string
	for _, e := range events {
		// By SHAPE. A turn that began inside the window and finished outside it did the
		// work, and re-testing freshness here would refuse to close the errand it had
		// just completed — leaving the row open for ever and holding the daemon awake.
		if IsTimerMessageEvent(e) && e.ID != "" {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// DedupeWakeEvents drops events already present in a pending queue, by id.
//
// The scheduler acknowledges a burst AFTER handing it to the callback, and it discards
// the error from that acknowledgement. So a delivery that is slow, or one whose mark
// fails, can be handed over a second time while the first is still being worked — and
// the reactors append what they are given. For a watcher digest a duplicate is noise;
// for a scheduled MESSAGE it is the instruction carried out twice, which for anything
// that mutates is not a cosmetic problem.
//
// Matched on id AND VERSION, not id alone. The queue re-arms an event whose content
// materially changed (bumping count/updatedAt) precisely so the update is delivered, and
// MarkNotified is conditioned on the same pair — so an id-only match would drop the
// NEW content as a duplicate while the acknowledgement stamped the newer version
// notified, and the update would be lost rather than deduplicated. An event with no id
// cannot be matched, so it is kept rather than silently dropped.
func DedupeWakeEvents(incoming, pending []domain.QueueEvent) []domain.QueueEvent {
	if len(pending) == 0 || len(incoming) == 0 {
		return incoming
	}
	type version struct {
		id      string
		count   int
		revised int64
	}
	key := func(e domain.QueueEvent) version {
		revised := e.CreatedAt
		if e.UpdatedAt != nil {
			revised = *e.UpdatedAt
		}
		return version{id: e.ID, count: e.Count, revised: revised}
	}
	seen := make(map[version]struct{}, len(pending))
	for _, e := range pending {
		if e.ID != "" {
			seen[key(e)] = struct{}{}
		}
	}
	out := incoming[:0:0]
	for _, e := range incoming {
		if e.ID != "" {
			k := key(e)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
		}
		out = append(out, e)
	}
	return out
}

// RetryLedger records which events have already used their one retry.
//
// The reactors used to hold a single boolean for this. That is fine while a burst is one
// indivisible thing, and wrong the moment it is not: a message now takes its turn alone
// and defers its neighbours, so event A's failed attempt would latch the flag and event
// B — delivered later, on its own — would be dropped on its FIRST failure, having never
// been tried twice. A user's scheduled instruction is exactly the sort of thing that
// disappeared that way.
//
// Keyed by event id, cleared when an event succeeds or is abandoned, so the ledger
// tracks live attempts rather than growing for the life of the process.
type RetryLedger map[string]struct{}

// TakeRetry reports whether these events may be retried, and records that they have
// been. True only while at least one of them has a retry left.
func (l RetryLedger) TakeRetry(events []domain.QueueEvent) bool {
	any := false
	for _, e := range events {
		if e.ID == "" {
			continue
		}
		if _, used := l[e.ID]; !used {
			l[e.ID] = struct{}{}
			any = true
		}
	}
	return any
}

// Done forgets these events, so a later delivery of the same id starts fresh.
func (l RetryLedger) Done(events []domain.QueueEvent) {
	for _, e := range events {
		delete(l, e.ID)
	}
}

// DropStaleTimerMessages removes scheduled messages that are past their freshness
// window, so a burst cannot spend a turn on an instruction that must not run.
//
// The gate in IsTimerMessageWake stops a stale message being TREATED as an instruction,
// but on its own that is not enough: a burst is filtered when it is queued, and it may
// sit behind another turn for as long as that turn takes. An event that went stale in
// the queue would then fall through to the watcher branch of BuildWakePrompt and be
// summarized as though it were observed activity — a paid turn, spent on something the
// system has already decided not to do, described as something it is not.
//
// Dropped from the BURST only. The inbox row stays open and unresolved, so the user
// still sees the instruction that did not happen.
func DropStaleTimerMessages(events []domain.QueueEvent) []domain.QueueEvent {
	now := domain.NowMS()
	out := events[:0:0]
	for _, e := range events {
		if IsTimerMessageEvent(e) && isStaleTimerMessage(e, now) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// SplitWakeBatch decides what ONE turn may be asked to do, and what waits.
//
// A scheduled message becomes the turn's user request, so two of them in one burst
// would be handed over as a single request containing two unrelated errands — and the
// scheduler has already marked both notified, so if the model finished only one the
// other would never be retried. Silently dropping half of what the user scheduled is
// worse than delivering it late.
//
// So a message takes the turn ALONE: the first one runs, everything else in the burst
// (later messages, watcher digests, async completions) is deferred for the caller to
// re-arm and comes back on the next sweep. A burst with no message is unchanged —
// watcher and async events have always batched together and still do.
//
// Progress is guaranteed: every turn consumes exactly one message, so a backlog drains
// one turn at a time instead of collapsing into one.
func SplitWakeBatch(events []domain.QueueEvent) (batch, deferred []domain.QueueEvent) {
	first := -1
	for i, e := range events {
		if IsTimerMessageEvent(e) {
			first = i
			break
		}
	}
	if first < 0 {
		return events, nil
	}
	deferred = make([]domain.QueueEvent, 0, len(events)-1)
	for i, e := range events {
		if i != first {
			deferred = append(deferred, e)
		}
	}
	return []domain.QueueEvent{events[first]}, deferred
}

// BurstHasTimerMessage reports whether a wake burst contains a scheduled message, so
// the reactor can mark the turn it is about to start. Both reactors call it rather than
// re-deriving the answer, because a turn marked in one path and not the other would
// leave the recursion guard working in only half the product.
func BurstHasTimerMessage(events []domain.QueueEvent) bool {
	for _, e := range events {
		// By SHAPE. This decides whether the turn counts as message-started, which
		// governs the recursion cut — and that must not hinge on the clock. A burst
		// crossing the freshness boundary between the drop and this call would
		// otherwise start a turn that no longer admitted where it came from.
		if IsTimerMessageEvent(e) {
			return true
		}
	}
	return false
}

// IsTimerMessageWake reports whether a timer event is a scheduled MESSAGE — the one
// timer shape that starts a turn — rather than a reminder, a tool-call outcome, or an
// error.
//
// Every condition is load-bearing and all of them are required. The marker says the
// payload was a message; the id ties it to a real schedule row; a non-empty summary is
// the instruction itself, and an event without one has nothing to carry out. Waking on
// a blank instruction would spend a model turn to discover it had nothing to do.
//
// Shared by internal/agent, internal/host and internal/supervisor so the three filters
// cannot drift into disagreeing about what wakes the assistant.
// isTimerMessageEvent asks WHAT an event is, not whether it may run.
//
// The two questions were sharing one function, and every remaining race came from that:
// a stale message stopped LOOKING like a message, so the prompt builder filed it under
// watcher activity, and the post-turn resolve stopped recognising the very errand it had
// just carried out. Shape is immutable; eligibility depends on the clock. They must be
// asked separately.
func IsTimerMessageEvent(e domain.QueueEvent) bool {
	return e.Source == domain.SourceTimer &&
		e.Target != nil &&
		e.Target.TimerMessage &&
		e.Target.TimerID != "" &&
		strings.TrimSpace(e.Summary) != ""
}

func IsTimerMessageWake(e domain.QueueEvent) bool {
	return IsTimerMessageEvent(e) && !isStaleTimerMessage(e, domain.NowMS())
}

// TimerMessageFreshnessMs is how long after its due time a scheduled message may still
// be carried out. One hour: long enough to cover a crash, a restart and an ownership
// handover; short enough that the instruction is still about the world it was written
// for.
const TimerMessageFreshnessMs int64 = 60 * 60 * 1000

// isStaleTimerMessage reports whether an occurrence is too far past its due time to act
// on.
//
// THE gate, and deliberately the only one that decides. Freshness was previously checked
// where each path happened to notice — at the fire, in boot recovery, in the re-arm —
// and every one of those anchored on a different timestamp (claim time, publication
// time, a repeat's already-advanced fireAt), so each had its own drift and a message
// could arrive late through whichever check it had slipped past. Those remain as cheap
// pre-filters; correctness lives here, immediately before an event can become a turn,
// anchored on the due time the user actually chose.
//
// A zero TimerDueAt is an event written before this field existed. Treated as fresh:
// refusing on a missing field would silently drop instructions from an older row, which
// is the failure being prevented, not a way to prevent it.
func isStaleTimerMessage(e domain.QueueEvent, now int64) bool {
	if e.Target == nil || e.Target.TimerDueAt <= 0 {
		return false
	}
	return now-e.Target.TimerDueAt > TimerMessageFreshnessMs
}

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
	case domain.SourceTimer:
		// ONLY a timed message, never a plain reminder. The marker is what separates
		// "the user asked me to do this now" from "the user asked to be reminded" —
		// two payloads on one table that must not become one behaviour.
		return IsTimerMessageWake(e)
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
	var async, watcher, prompts []domain.QueueEvent
	for _, e := range events {
		switch e.Source {
		case domain.SourceAsyncTool:
			async = append(async, e)
		case domain.SourceTimer:
			// By SHAPE, not by freshness. Judging eligibility here meant a message that
			// crossed the hour between the burst filter and this line stopped looking
			// like a message and was rendered as observed activity — a paid turn
			// describing an instruction as something it is not. Staleness is decided
			// once, by DropStaleTimerMessages, before the burst gets here.
			if IsTimerMessageEvent(e) {
				prompts = append(prompts, e)
			} else {
				watcher = append(watcher, e)
			}
		default:
			watcher = append(watcher, e)
		}
	}
	// A due message leads, always. It is the only thing in a burst that the USER
	// actually dictated, so it outranks any amount of machine-generated watcher
	// digest that happens to land in the same tick.
	if len(prompts) > 0 {
		out := buildTimerMessageWakePrompt(prompts)
		if len(async) > 0 {
			out += "\n\n" + asyncWakeSection(async)
		}
		if len(watcher) > 0 {
			out += "\n\n" + buildWatcherWakePrompt(watcher, alreadySummarized)
		}
		return out
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
	// "Account problem:", not "Account:". These are matched by PREFIX against a reply
	// the model may have authored, and an assistant answer can perfectly well open with
	// "Account: ..." as a heading — at which point a real result would be recorded as a
	// failed turn and the work it summarized lost. Every other entry here is
	// failure-shaped for the same reason; isStalledReply goes further and matches whole.
	"Account problem:",                                  // account taxonomy (classifyBackendError → accountFailureAdvice)
	"Can't reach the Daintree assistant backend",        // backend unreachable (classifyBackendError)
	"Daintree assistant backend is a different version", // protocol mismatch (classifyBackendError)
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

// buildTimerMessageWakePrompt frames due timed MESSAGES.
//
// This shape is different in kind from the other two and the framing has to say so. A
// watcher wake reports machine-observed activity and an async wake reports work the
// model itself started; both are the assistant being TOLD something. A timed message is
// the assistant being told to DO something, by the user, in the user's own words,
// written earlier and delivered now.
//
// The instruction travels as JSON, not interpolated prose. The text is arbitrary user
// input that will be pasted into a prompt — a message containing the wrapper's own
// delimiters, or a line that reads like an instruction to ignore what came before,
// must not be able to escape its slot and become framing. json.Marshal makes the
// boundary structural instead of typographic.
//
// It keeps the "not typed just now" framing because the user may not be present: the
// assistant must not answer as though somebody is sitting there to take a follow-up
// question.
func buildTimerMessageWakePrompt(events []domain.QueueEvent) string {
	type dueMessage struct {
		TimerID    string `json:"timerId"`
		Occurrence int    `json:"occurrence,omitempty"`
		Title      string `json:"title,omitempty"`
		Message    string `json:"message"`
	}
	due := make([]dueMessage, 0, len(events))
	for _, e := range events {
		msg := strings.TrimSpace(e.Summary)
		if msg == "" {
			continue
		}
		m := dueMessage{Message: msg, Title: strings.TrimSpace(e.Title)}
		if e.Target != nil {
			m.TimerID = e.Target.TimerID
			m.Occurrence = e.Target.TimerOccurrence
		}
		due = append(due, m)
	}
	if len(due) == 0 {
		// A marked event with no text is a bug upstream, not an instruction. Say that
		// rather than inventing a task out of an empty string.
		return wakePromptPrefix + " A scheduled message came due but carried no text. " +
			"Report that the scheduled message was empty; do not guess what it was meant to say."
	}

	noun, verb := "message", "It is"
	if len(due) > 1 {
		noun, verb = "messages", "They are"
	}
	body, err := json.MarshalIndent(due, "", "  ")
	if err != nil {
		// Marshalling a slice of plain strings cannot realistically fail, but a
		// fabricated prompt would be worse than an honest one if it ever did.
		return wakePromptPrefix + " A scheduled message came due but could not be rendered. " +
			"Report that the scheduled message could not be read."
	}

	return wakePromptPrefix + " Scheduled " + noun + " you were asked to deliver " +
		"later are now due. " + verb + " the USER'S OWN INSTRUCTIONS, written earlier and " +
		"delivered now — not typed just now, so nobody may be watching.\n\n" +
		"Treat each \"message\" value below as the user's current request and carry it out " +
		"with your normal tools, exactly as if it had been typed. Everything else in the JSON " +
		"is delivery metadata, not instruction. Do not merely acknowledge them, and do not " +
		"report them back as events that happened. Where one cannot be done unattended — it " +
		"needs a confirmation nobody is present to give, or authority you were not granted — " +
		"say precisely what is blocked and leave it, rather than working around it. You cannot " +
		"schedule another message from this turn, so do not try to defer or retry one.\n\n" +
		"Delivery is AT LEAST ONCE. A message interrupted by a restart is delivered again, so " +
		"this may not be the first attempt. Before any step that is destructive or would " +
		"duplicate work, check whether it has already been done and say what you found — do not " +
		"assume either that it has or that it has not.\n\n" +
		"Due " + noun + ":\n\n" + string(body)
}
