/**
 * Renders a turn's delegated work as a branch tree — the brand signature. Each
 * activity is one branch carrying a real relationship: request → decision →
 * agent → watcher → outcome. Default view shows verb + target + outcome +
 * duration; `expanded` adds the raw args/result for debugging.
 *
 * OpenTUI port: Ink `<Box>`/`<Text>` → the lowercase native `<box>`/`<text>`.
 * The branch / glyph / label / detail runs stay as sibling `<text>` leaves inside
 * a flex `<box>` (they each carry a distinct `fg`/`attributes` and the detail
 * truncates independently, so they can't collapse into one `<text>`+`<span>`).
 * Ink `color=` → `fg=`, `dimColor` → `attributes={TextAttributes.DIM}`,
 * `wrap="truncate"` → the `truncate` prop. Glyphs, columns and budgets are byte
 * identical to the Ink source.
 */
import { TextAttributes } from "@opentui/core";
import type { ActivityItem, ActivityState } from "../types.js";
import {
  glyphs,
  toneColor,
  ui,
  unicodeOk,
  type GlyphSet,
  type Tone,
} from "../theme.js";
import { formatDuration } from "../primitives.js";
import { compactArgs, truncate } from "../../utils/text.js";
import { ThinkingDot } from "./ThinkingDot.js";

function activityTone(state: ActivityState): Tone {
  switch (state) {
    case "done":
      return "success";
    case "failed":
      return "danger";
    case "active":
      return "active";
    case "waiting":
      return "warning";
    case "queued":
    default:
      return "neutral";
  }
}

function activityGlyph(state: ActivityState, set: GlyphSet): string {
  switch (state) {
    case "done":
      return set.done;
    case "failed":
      return set.failed;
    case "active":
      return set.active;
    case "waiting":
      return set.waiting;
    case "queued":
    default:
      return set.pending;
  }
}

const LABEL_WIDTH = 11;
// Fixed left prefix: branch glyph + space + badge glyph + space ("├─ ✓ ").
const PREFIX_COLS = 5;
// Reserve for the right-aligned duration ("9999ms" ≈ 6) plus a one-column gap,
// so a long detail is truncated *before* it collides with the timing — the
// `space-between` row otherwise leaves zero gap when the content fills the width.
const DURATION_COLS = 8;

export function ActivityTree({
  activities,
  width,
  now = Date.now(),
  expanded = false,
  live = false,
}: {
  activities: ActivityItem[];
  width: number;
  now?: number;
  expanded?: boolean;
  /**
   * True only in the LIVE active turn — gates the animated spinner on active rows.
   * Must stay false for a committed/scrollback render: ThinkingDot drives a timer and
   * would freeze/smear into native scrollback (see its INVARIANT). Active activities
   * only exist on a live turn anyway, so a committed turn never animates.
   */
  live?: boolean;
}) {
  if (activities.length === 0) return null;
  const set = glyphs();
  const ascii = !unicodeOk();
  return (
    <box flexDirection="column">
      {activities.map((a, i) => {
        const last = i === activities.length - 1;
        const branch = last ? set.lastBranch : set.branch;
        const tone = activityTone(a.state);
        const elapsed =
          a.endedAt != null
            ? a.endedAt - a.startedAt
            : a.state === "active"
              ? Math.max(0, now - a.startedAt)
              : undefined;
        // Default detail: the target, or the result summary once done. On FAILURE,
        // surface the failure summary even when a target detail exists — the outcome
        // must never be hidden behind the original "Reading foo.ts" target.
        let detail = a.detail ?? (a.state === "done" ? a.summary : undefined);
        if (a.state === "failed" && a.summary) {
          detail = detail ? `${detail} · ${a.summary}` : a.summary;
        }
        // Detail shares the row with the label and the right-aligned duration.
        // The label column is its real width (padded up to LABEL_WIDTH for short
        // verbs), plus one separating space — budget the remainder so a long
        // detail truncates instead of shoving into the timing.
        const labelCols = Math.max(a.label.length + 1, LABEL_WIDTH);
        const detailRoom = Math.max(
          8,
          width - PREFIX_COLS - labelCols - DURATION_COLS,
        );
        return (
          <box key={a.id} flexDirection="column">
            {/* OpenTUI `<box>` defaults to a column (Ink's `<Box>` defaulted to a
                row), so the horizontal rows are explicitly `flexDirection="row"`. */}
            <box flexDirection="row" justifyContent="space-between">
              <box flexDirection="row">
                <text fg={ui.color.muted}>{branch} </text>
                {live && a.state === "active" ? (
                  // Animated spinner for a running tool (live turn only). One column +
                  // a trailing space, matching the static glyph cell's width.
                  <box flexDirection="row">
                    <ThinkingDot ascii={ascii} />
                    <text> </text>
                  </box>
                ) : (
                  <text fg={toneColor(tone)}>
                    {activityGlyph(a.state, set)}{" "}
                  </text>
                )}
                <text>{a.label}</text>
                {detail ? (
                  // Always at least one space after the label, and pad short
                  // labels so details line up in a column. Labels longer than the
                  // column just get the single separating space.
                  <text attributes={TextAttributes.DIM} truncate>
                    {" ".repeat(Math.max(1, LABEL_WIDTH - a.label.length))}
                    {truncate(detail, detailRoom)}
                  </text>
                ) : null}
              </box>
              {elapsed != null ? (
                <box marginLeft={1}>
                  <text attributes={TextAttributes.DIM}>
                    {formatDuration(elapsed)}
                  </text>
                </box>
              ) : null}
            </box>
            {expanded ? (
              // truncate the raw args/result so an expanded row (^X) can't out-run a
              // just-shrunk terminal during a pane resize and orphan a wrapped copy
              // into scrollback (#138); `width` is a lagged content-budget hint.
              <box flexDirection="column" paddingLeft={3}>
                <text attributes={TextAttributes.DIM} truncate>
                  {a.name} args: {compactArgs(a.args, Math.max(20, width - 12))}
                </text>
                {a.summary ? (
                  <text attributes={TextAttributes.DIM} truncate>
                    result: {truncate(a.summary, width - 12)}
                  </text>
                ) : null}
              </box>
            ) : null}
          </box>
        );
      })}
    </box>
  );
}
