/**
 * The run-phase vocabulary — one source of truth for naming what a turn is doing
 * right now, shared by the transcript's {@link LiveRunStatus} line and the composer's
 * busy indicator. Replaces the old single hardcoded "Thinking": we always name the
 * MOST PRECISE state we have (the guide's status table), and fall back to "Processing"
 * — never "Thinking" (too vague) and never "Generating" during tool use/approval.
 */
import type { ActivityItem, RunPhase, TurnCell } from "./types.js";

/**
 * Map an active tool's verb to a present-progressive stage, so the composer can say
 * "Inspecting project…" / "Delegating…" during tool execution instead of a generic
 * label. Mirrors the verbs presentTool produces.
 */
function toolStageLabel(activity: ActivityItem | undefined): string {
  const label = activity?.label;
  switch (label) {
    case "Delegated":
      return "Delegating";
    case "Watching":
      return "Watching";
    case "Read":
    case "Listed":
    case "Searched":
    case "Extracted":
      return "Inspecting project";
    case "Scheduled":
      return "Scheduling";
    case undefined:
      return "Processing";
    default:
      // A named-but-unmapped tool: surface its verb as "Running <verb>".
      return `Running ${label.toLowerCase()}`;
  }
}

/** The most-precise human label for a turn's current phase (the composer stage). */
export function runStageLabel(turn: TurnCell): string {
  switch (turn.phase) {
    case "received":
      return "Received";
    case "analyzing":
      return "Analyzing request";
    case "generating":
      return "Generating";
    case "integrating":
      return "Integrating results";
    case "awaiting_approval":
      return "Waiting for approval";
    case "cancelling":
      return "Cancelling";
    case "tool_running":
      return toolStageLabel(
        [...turn.activities].reverse().find((a) => a.state === "active"),
      );
    default:
      // complete / failed / cancelled have no live label; a stray read falls back.
      return "Processing";
  }
}

/**
 * What the live status LINE should show under the DAINTREE marker, or null when the
 * phase is self-evident from other chrome and a separate line would be redundant:
 *   - generating  → the streaming prose + caret already communicate it
 *   - tool_running → the activity tree already shows each tool live
 *   - received     → shown inline on the DAINTREE marker ("· received"), not a line
 *   - terminal     → nothing live
 * The remaining phases (analyzing, integrating, awaiting_approval, cancelling) are the
 * "silent work" gaps the old UI couldn't name — exactly where a spinner + label earns
 * its place.
 */
export function liveStatusLabel(turn: TurnCell): string | null {
  if (turn.state !== "active") return null;
  switch (turn.phase) {
    case "analyzing":
      return "Analyzing request";
    case "generating":
      // Shown right under DAINTREE, above the streaming response, so the run reads
      // "DAINTREE → Generating → [the answer]" rather than a bare wall of prose.
      return "Generating";
    case "integrating":
      return "Integrating results";
    case "awaiting_approval":
      return "Waiting for approval";
    case "cancelling":
      return "Cancelling";
    default:
      // received (shown inline on the marker) and tool_running (the activity tree IS
      // the live indicator) and terminal states get no separate line.
      return null;
  }
}

/** True while the turn is doing live work the composer should flag as busy. */
export function isLiveWork(phase: RunPhase): boolean {
  return (
    phase !== "complete" && phase !== "failed" && phase !== "cancelled"
  );
}
