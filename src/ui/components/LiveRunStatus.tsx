/**
 * The live run-status line under the DAINTREE marker: an animated spinner + the
 * precise phase label + a live elapsed clock, e.g. `⠋ Analyzing request · 0.4s`.
 *
 * It renders ONLY for the "silent work" phases the old UI couldn't name — the gaps
 * after submit before the first token, and after a tool finishes before the next
 * model response (`analyzing`, `integrating`, `awaiting_approval`, `cancelling`).
 * While prose streams or tools run, it shows nothing: the moving response/caret and
 * the activity tree already communicate that work. See {@link liveStatusLabel}.
 */
import { TextAttributes } from "@opentui/core";
import type { TurnCell } from "../types.js";
import { unicodeOk } from "../theme.js";
import { formatDuration } from "../primitives.js";
import { liveStatusLabel } from "../runStatus.js";
import { ThinkingDot } from "./ThinkingDot.js";

export function LiveRunStatus({
  turn,
  now = Date.now(),
}: {
  turn: TurnCell;
  /** Frozen clock for deterministic rendering; defaults to live time. */
  now?: number;
}) {
  const label = liveStatusLabel(turn);
  if (!label) return null;
  const ascii = !unicodeOk();
  const elapsed = Math.max(0, now - turn.phaseStartedAt);
  return (
    // ThinkingDot is its own animated `<text>`, and a native `<text>` may not nest
    // another, so the spinner, label and elapsed sit side-by-side in a row.
    <box flexDirection="row">
      <ThinkingDot ascii={ascii} />
      <text attributes={TextAttributes.DIM}>
        {" "}
        {label}
        {elapsed >= 300 ? ` · ${formatDuration(elapsed)}` : ""}
      </text>
    </box>
  );
}
