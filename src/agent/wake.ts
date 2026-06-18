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
 */
export function buildWakePrompt(events: QueueEvent[]): string {
  const lines = events.map((e) => {
    const ev = e as {
      title?: string;
      summary?: string;
      target?: { terminalId?: string };
    };
    const term = ev.target?.terminalId ? ` [terminal ${ev.target.terminalId}]` : "";
    return `- ${ev.title ?? "event"}${ev.summary ? `: ${ev.summary}` : ""}${term}`;
  });
  return [
    "[automatic wake-up] A background watcher surfaced new activity while you were idle — this was NOT typed by the user.",
    "Decide what to do. If a watched terminal finished, is waiting for input, or failed, read it with terminal.summarize or terminal.extract and give the user a concise update. If it isn't worth acting on, say so in one line.",
    "",
    "New events:",
    ...lines,
  ].join("\n");
}
