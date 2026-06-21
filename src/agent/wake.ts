/**
 * Autonomous wake-up helpers, shared by the Ink controller and the native host so
 * both surfaces react to background watcher activity identically.
 *
 * When the scheduler surfaces attention events, a terminal-watcher event means a
 * supervised agent finished, is waiting, or failed — exactly when the assistant
 * should look and report. Those events are fed to the model as a READ-ONLY turn
 * (see AgentSession.send's `readOnly` option) so a background trigger can inspect
 * and report but never run a mutating tool unattended.
 */
import type { QueueEvent } from "../schemas.js";

/**
 * Whether a surfaced attention event should autonomously wake the model (run a
 * turn) versus just appear in the inbox. Only a terminal-watcher event carrying a
 * real terminal target qualifies — so model/user-published queue events can't
 * trigger an autonomous turn.
 */
export function isActionableWake(e: QueueEvent): boolean {
  const ev = e as { source?: string; target?: { terminalId?: string } };
  return ev.source === "terminal_watcher" && Boolean(ev.target?.terminalId);
}

/**
 * Build the internal nudge fed to the model when a watcher wakes it. It is sent as
 * a read-only turn; the model's reaction is what surfaces, not this prompt.
 *
 * `opts.alreadySummarized` carries the set of terminal IDs the assistant has
 * already reported on earlier this session. A terminal's lifecycle surfaces
 * several events (e.g. `waiting_for_input` then `terminal_exited`); without this
 * memory the model would re-run `terminal.summarize` on every one and the user
 * would see the same terminal reported two or three times. For a terminal already
 * in this set, the per-event line is downgraded to a one-line acknowledgement so
 * the model treats it as a lifecycle transition, not fresh content to summarize.
 */
export function buildWakePrompt(
  events: QueueEvent[],
  opts?: { alreadySummarized?: ReadonlySet<string> },
): string {
  // Seed from the caller's cross-burst memory, then grow locally so a terminal that
  // appears twice within THIS batch only earns a full summary on its first line.
  const seen = new Set<string>(opts?.alreadySummarized ?? []);
  let anyFollowUp = false;
  let anyNew = false;
  const lines = events.map((e) => {
    const ev = e as {
      title?: string;
      summary?: string;
      target?: { terminalId?: string };
    };
    const terminalId = ev.target?.terminalId;
    const term = terminalId ? ` [terminal ${terminalId}]` : "";
    const base = `- ${ev.title ?? "event"}${ev.summary ? `: ${ev.summary}` : ""}${term}`;
    if (terminalId && seen.has(terminalId)) {
      // Already reported this terminal — a follow-up lifecycle event, not new content.
      anyFollowUp = true;
      return `${base} (already reported — acknowledge in one line, do NOT call terminal.read/terminal.summarize/terminal.extract again)`;
    }
    if (terminalId) seen.add(terminalId);
    anyNew = true; // a fresh terminal, or a non-terminal event worth deciding on
    return base;
  });
  // The positive "read and summarize" instruction only applies when something new is
  // present. When every event is a follow-up it would contradict the per-event "do
  // NOT summarize" markers, so swap in acknowledge-only guidance instead.
  const guidance = anyNew
    ? "Decide what to do. If a watched terminal finished, is waiting for input, or failed, read it and give the user a concise update — use terminal.read to relay what the agent said verbatim, terminal.summarize for a gist, or terminal.extract to pull a specific field. If it isn't worth acting on, say so in one line."
    : "Every event below is a terminal you have already reported this session — these are lifecycle transitions only. Acknowledge each in one short line; do NOT call terminal.read/terminal.summarize/terminal.extract again.";
  return [
    "[automatic wake-up] A background watcher surfaced new activity while you were idle — this was NOT typed by the user.",
    guidance,
    ...(anyNew && anyFollowUp
      ? [
          "Some events below are marked (already reported): you have already summarized that terminal this session. For those, do NOT summarize again — just acknowledge the transition in one short line.",
        ]
      : []),
    "",
    "New events:",
    ...lines,
  ].join("\n");
}

/**
 * Whether a string returned by `AgentSession.send` represents a turn that failed
 * before delivering a real answer. `send` never throws on a model-layer failure —
 * it catches and returns one of these sentinel strings (see `loop.ts` send/run).
 * The wake reactors must treat such a reply as a failure and NOT record the
 * terminals as summarized; otherwise a transient model outage would permanently
 * downgrade those terminals' later lifecycle events to one-line acks and silently
 * swallow the real summary the user never got.
 */
const WAKE_FAILURE_PREFIXES = [
  "Model unavailable:",
  "Model error:",
  "Tool projection failed:",
  "Reached the tool-iteration limit",
  // The circuit breaker stops a turn that hammered one failing tool call with
  // identical args (see loop.ts REPEAT_FAILURE_ABORT). Like the iteration limit, it
  // delivered no summary — a wake must not record its terminals as reported.
  "Stopped: called ",
  // A cancelled turn delivered no summary either — treat it as a non-result so a
  // wake's terminals aren't recorded as reported. (Wake turns aren't user-cancellable
  // today, but this keeps the sentinel set complete and future-proof.)
  "Turn cancelled",
] as const;

export function isWakeFailureReply(reply: string): boolean {
  return WAKE_FAILURE_PREFIXES.some((prefix) => reply.startsWith(prefix));
}
