/**
 * Renders a turn's delegated work as a branch tree — the brand signature. Each
 * activity is one branch carrying a real relationship: request → decision →
 * agent → watcher → outcome. Default view shows verb + target + outcome +
 * duration; `expanded` adds the raw args/result for debugging.
 */
import { Box, Text } from "ink";
import type { ActivityItem, ActivityState } from "../types.js";
import {
  glyphs,
  toneColor,
  ui,
  type GlyphSet,
  type Tone,
} from "../theme.js";
import { formatDuration } from "../primitives.js";
import { compactArgs, truncate } from "../../utils/text.js";

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
}: {
  activities: ActivityItem[];
  width: number;
  now?: number;
  expanded?: boolean;
}) {
  if (activities.length === 0) return null;
  const set = glyphs();
  return (
    <Box flexDirection="column">
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
        const detail = a.detail ?? (a.state === "done" ? a.summary : undefined);
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
          <Box key={a.id} flexDirection="column">
            <Box justifyContent="space-between">
              <Box>
                <Text color={ui.color.muted}>{branch} </Text>
                <Text color={toneColor(tone)}>{activityGlyph(a.state, set)} </Text>
                <Text>{a.label}</Text>
                {detail ? (
                  // Always at least one space after the (white) label, and pad
                  // short labels so details line up in a column. Labels longer
                  // than the column just get the single separating space.
                  <Text dimColor>
                    {" ".repeat(Math.max(1, LABEL_WIDTH - a.label.length))}
                    {truncate(detail, detailRoom)}
                  </Text>
                ) : null}
              </Box>
              {elapsed != null ? (
                <Box marginLeft={1}>
                  <Text dimColor>{formatDuration(elapsed)}</Text>
                </Box>
              ) : null}
            </Box>
            {expanded ? (
              <Box flexDirection="column" paddingLeft={3}>
                <Text dimColor>
                  {a.name} args: {compactArgs(a.args, Math.max(20, width - 12))}
                </Text>
                {a.summary ? (
                  <Text dimColor>result: {truncate(a.summary, width - 12)}</Text>
                ) : null}
              </Box>
            ) : null}
          </Box>
        );
      })}
    </Box>
  );
}
