/**
 * The home surface is a Claude/Codex-style vertical ledger: an intro/work block
 * at the top, then chronological user + Daintree turns separated by horizontal
 * rules. The composer stays fixed below this component; this viewport can scroll
 * by rendered lines so alternate-screen users can still inspect history.
 */
import { Box, Text } from "ink";
import type { DashboardState, TranscriptCell, ActivityItem } from "../types.js";
import type { TerminalPreview } from "../hooks/useTerminalPreview.js";
import type { WorkflowRunRecord } from "../../schemas.js";
import { buildAgentRows } from "../presentation/operations.js";
import {
  glyphs,
  severityTone,
  toneColor,
  ui,
  type Tone,
} from "../theme.js";
import { formatDuration } from "../primitives.js";
import { compactArgs, truncate } from "../../utils/text.js";

interface Segment {
  text: string;
  color?: string;
  dimColor?: boolean;
  bold?: boolean;
  inverse?: boolean;
}

interface LedgerLine {
  key: string;
  segments: Segment[];
}

export interface IntroBlock {
  project: string;
  tier: string;
  connected: boolean;
  dashboard: DashboardState;
  previews?: TerminalPreview[];
  busy: boolean;
  stage: string;
  /** Debug logging is active — surfaced in the header so it's verifiable at a glance. */
  logging?: boolean;
}

const line = (key: string, segments: Segment[] | string): LedgerLine => ({
  key,
  segments: typeof segments === "string" ? [{ text: segments }] : segments,
});

function visibleLen(segments: Segment[]): number {
  return segments.reduce((n, s) => n + s.text.length, 0);
}

function clipSegments(segments: Segment[], width: number): Segment[] {
  let remaining = Math.max(1, width);
  const out: Segment[] = [];
  for (const segment of segments) {
    if (remaining <= 0) break;
    const text =
      segment.text.length > remaining
        ? segment.text.slice(0, remaining)
        : segment.text;
    out.push({ ...segment, text });
    remaining -= text.length;
  }
  return out;
}

function blank(key: string): LedgerLine {
  return line(key, "");
}

function spacer(key: string): LedgerLine {
  return line(key, " ");
}

function rule(key: string, width: number, color?: string): LedgerLine {
  return line(key, [
    {
      text: "─".repeat(Math.max(1, width)),
      color,
      dimColor: !color,
    },
  ]);
}

function wrapText(text: string, width: number): string[] {
  const w = Math.max(4, width);
  const out: string[] = [];
  for (const raw of text.split("\n")) {
    let rest = raw.trimEnd();
    if (!rest) {
      out.push("");
      continue;
    }
    while (rest.length > w) {
      let at = rest.lastIndexOf(" ", w);
      if (at < Math.min(12, Math.floor(w * 0.4))) at = w;
      out.push(rest.slice(0, at));
      rest = rest.slice(at).trimStart();
    }
    out.push(rest);
  }
  return out;
}

function prefixedText(
  key: string,
  prefix: Segment[],
  text: string,
  width: number,
  body: Omit<Segment, "text"> = {},
  continuationPrefix: Segment[] = prefix,
): LedgerLine[] {
  const prefixLen = visibleLen(prefix);
  const bodyWidth = Math.max(4, width - prefixLen);
  return wrapText(text, bodyWidth).map((part, i) =>
    line(`${key}-${i}`, [
      ...(i === 0 ? prefix : continuationPrefix),
      { ...body, text: part },
    ]),
  );
}

function statusTone(state: ActivityItem["state"]): Tone {
  switch (state) {
    case "active":
      return "active";
    case "done":
      return "success";
    case "failed":
      return "danger";
    case "waiting":
      return "warning";
    case "queued":
    default:
      return "neutral";
  }
}

function statusGlyph(state: ActivityItem["state"]): string {
  const set = glyphs();
  switch (state) {
    case "active":
      return set.active;
    case "done":
      return set.done;
    case "failed":
      return set.failed;
    case "waiting":
      return set.waiting;
    case "queued":
    default:
      return set.pending;
  }
}

function relativeDue(ms: number): string {
  if (ms <= 0) return "due now";
  const secs = Math.round(ms / 1000);
  if (secs < 60) return `in ${secs}s`;
  const mins = Math.round(secs / 60);
  if (mins < 90) return `in ${mins}m`;
  const hours = Math.round(mins / 60);
  if (hours < 48) return `in ${hours}h`;
  return `in ${Math.round(hours / 24)}d`;
}

function workflowTitle(run: WorkflowRunRecord): string {
  if (run.issueTitle) {
    const prefix = run.issueNumber != null ? `#${run.issueNumber} ` : "";
    return `${prefix}${run.issueTitle}`;
  }
  if (run.prNumber != null) return `PR #${run.prNumber}`;
  if (run.branch) return run.branch;
  if (run.worktreeId) return run.worktreeId;
  return run.id;
}

function workflowNextAction(run: WorkflowRunRecord): string | undefined {
  if (!run.nextActionJson) return undefined;
  try {
    const parsed = JSON.parse(run.nextActionJson) as { label?: unknown };
    return typeof parsed.label === "string" ? parsed.label : undefined;
  } catch {
    return undefined;
  }
}

function workflowMeta(run: WorkflowRunRecord): string | undefined {
  const parts = [
    run.branch ? `branch ${run.branch}` : "",
    run.prNumber != null ? `PR #${run.prNumber}` : "",
    workflowNextAction(run) ? `next ${workflowNextAction(run)}` : "",
  ].filter(Boolean);
  return parts.length ? parts.join(" · ") : undefined;
}

function buildIntroLines(intro: IntroBlock, width: number, now: number): LedgerLine[] {
  const set = glyphs();
  const agents = buildAgentRows(intro.dashboard.watchers, intro.previews ?? []);
  const active = agents.find((a) => a.classification === "still_working") ?? agents[0];
  const runs = intro.dashboard.workflowRuns ?? [];
  const blockedRun = runs.find((r) => r.status === "blocked");
  const primaryRun =
    blockedRun ?? runs.find((r) => r.status === "active") ?? runs[0];
  const topEvent = intro.dashboard.inbox[0];
  const nextTimer = [...intro.dashboard.timers].sort((a, b) => a.fireAt - b.fireAt)[0];
  const out: LedgerLine[] = [];

  out.push(
    line("intro-brand", [
      { text: `${set.brand} assistant`, color: ui.color.accent, bold: true },
    ]),
  );
  out.push(
    line("intro-project", [
      { text: "project ", dimColor: true },
      { text: truncate(intro.project, Math.max(8, width - 46)) },
      { text: " · ", dimColor: true },
      { text: intro.tier.toUpperCase(), dimColor: true },
      { text: " · ", dimColor: true },
      intro.connected
        ? { text: "MCP CONNECTED", color: ui.color.accent }
        : { text: "MCP DEGRADED", color: ui.color.warning },
      ...(intro.logging
        ? ([
            { text: " · ", dimColor: true },
            { text: `${set.active} LOG`, color: ui.color.warning, bold: true },
          ] as Segment[])
        : []),
    ]),
  );
  out.push(rule("intro-rule-a", width));

  if (topEvent) {
    const tone = severityTone(topEvent.severity);
    out.push(
      line("intro-attention", [
        { text: "ATTENTION ", color: toneColor(tone), bold: true },
        {
          text: truncate(
            `${topEvent.title}${topEvent.summary ? ` · ${topEvent.summary}` : ""}`,
            Math.max(12, width - 10),
          ),
        },
      ]),
    );
  } else if (blockedRun) {
    out.push(
      line("intro-attention", [
        { text: "ATTENTION ", color: ui.color.warning, bold: true },
        {
          text: truncate(`blocked · ${workflowTitle(blockedRun)}`, Math.max(12, width - 10)),
        },
      ]),
    );
  } else {
    out.push(
      line("intro-attention", [
        { text: "ATTENTION ", dimColor: true, bold: true },
        { text: "none", dimColor: true },
      ]),
    );
  }

  const workText = primaryRun
    ? `${primaryRun.status} · ${workflowTitle(primaryRun)}`
    : active
      ? `${active.id} · ${active.goal || active.title}`
      : intro.busy
        ? intro.stage
        : "Standing by";
  out.push(
    line("intro-work", [
      { text: "WORK      ", color: ui.color.accent, bold: true },
      { text: truncate(workText, Math.max(12, width - 10)) },
    ]),
  );

  const meta = primaryRun ? workflowMeta(primaryRun) : active?.preview;
  if (meta) {
    out.push(
      line("intro-preview", [
        { text: "          ", dimColor: true },
        { text: truncate(meta, Math.max(12, width - 10)), dimColor: true },
      ]),
    );
  }

  const activeFacts = [
    runs.length ? `${runs.length} run${runs.length === 1 ? "" : "s"}` : "",
    agents.length ? `${agents.length} agent${agents.length === 1 ? "" : "s"}` : "",
    intro.dashboard.watchers.length
      ? `${intro.dashboard.watchers.length} watcher${intro.dashboard.watchers.length === 1 ? "" : "s"}`
      : "",
    intro.dashboard.timers.length
      ? `${intro.dashboard.timers.length} timer${intro.dashboard.timers.length === 1 ? "" : "s"}`
      : "",
  ].filter(Boolean);
  out.push(
    line("intro-active", [
      { text: "ACTIVE    ", dimColor: true, bold: true },
      { text: activeFacts.length ? activeFacts.join(" · ") : "no background work", dimColor: true },
    ]),
  );

  if (nextTimer) {
    out.push(
      line("intro-timer", [
        { text: "NEXT      ", dimColor: true, bold: true },
        {
          text: `${truncate(nextTimer.title, Math.max(8, width - 20))} ${relativeDue(
            nextTimer.fireAt - now,
          )}`,
          dimColor: true,
        },
      ]),
    );
  }

  out.push(
    line("intro-keys", [
      { text: "KEYS      ", dimColor: true, bold: true },
      { text: "↑/↓ or wheel scroll · End latest · ^O ops · / cmds", dimColor: true },
    ]),
  );
  out.push(rule("intro-rule-b", width, ui.color.accent));
  return out;
}

function buildActivityLines(
  activity: ActivityItem,
  index: number,
  total: number,
  width: number,
  now: number,
  expanded: boolean,
): LedgerLine[] {
  const set = glyphs();
  const branch = index === total - 1 ? set.lastBranch : set.branch;
  const tone = statusTone(activity.state);
  const elapsed =
    activity.endedAt != null
      ? activity.endedAt - activity.startedAt
      : activity.state === "active"
        ? Math.max(0, now - activity.startedAt)
        : undefined;
  const elapsedText = elapsed != null ? ` ${formatDuration(elapsed)}` : "";
  const detail = activity.detail ?? activity.summary ?? "";
  const main = `${activity.label}${detail ? ` ${detail}` : ""}`;
  const out = [
    line(`activity-${activity.id}`, [
      { text: `${branch} `, dimColor: true },
      { text: `${statusGlyph(activity.state)} `, color: toneColor(tone) },
      {
        text: truncate(main, Math.max(8, width - branch.length - elapsedText.length - 4)),
      },
      { text: elapsedText, dimColor: true },
    ]),
  ];
  if (expanded) {
    out.push(
      ...prefixedText(
        `activity-${activity.id}-args`,
        [{ text: "   args   ", dimColor: true }],
        compactArgs(activity.args, Math.max(24, width - 10)),
        width,
        { dimColor: true },
      ),
    );
    if (activity.summary) {
      out.push(
        ...prefixedText(
          `activity-${activity.id}-result`,
          [{ text: "   result ", dimColor: true }],
          activity.summary,
          width,
          { dimColor: true },
        ),
      );
    }
  }
  return out;
}

function buildCellLines(
  cell: TranscriptCell,
  width: number,
  now: number,
  expanded: boolean,
): LedgerLine[] {
  const set = glyphs();
  if (cell.kind === "note") {
    const tone =
      cell.level === "error" ? "danger" : cell.level === "warn" ? "warning" : "active";
    return [
      blank(`${cell.id}-space`),
      ...prefixedText(
        cell.id,
        [
          { text: `${set.continuation}`, dimColor: true },
          { text: `${cell.level === "error" ? set.failed : cell.level === "warn" ? set.attention : set.bullet} `, color: toneColor(tone) },
        ],
        cell.text,
        width,
        {},
        [{ text: `${set.continuation}  `, dimColor: true }],
      ),
    ];
  }

  if (cell.kind === "command") {
    return [
      blank(`${cell.id}-space`),
      rule(`${cell.id}-rule`, width),
      line(`${cell.id}-title`, [{ text: cell.title || "COMMAND", color: ui.color.info, bold: true }]),
      ...prefixedText(
        `${cell.id}-text`,
        [{ text: "  ", dimColor: true }],
        cell.text,
        width,
        { dimColor: true },
      ),
    ];
  }

  const out: LedgerLine[] = [blank(`${cell.id}-space`), rule(`${cell.id}-rule`, width)];
  if (cell.userText) {
    out.push(line(`${cell.id}-user-label`, [{ text: "USER", dimColor: true, bold: true }]));
    out.push(
      ...prefixedText(
        `${cell.id}-user`,
        [{ text: `${set.continuation}`, dimColor: true }],
        cell.userText,
        width,
      ),
    );
    if (cell.assistantText || cell.streaming) {
      out.push(spacer(`${cell.id}-user-gap-1`), spacer(`${cell.id}-user-gap-2`));
    }
  }
  if (cell.assistantText || cell.streaming) {
    out.push(
      line(`${cell.id}-assistant-label`, [
        { text: `${set.brand} DAINTREE`, color: ui.color.accent, bold: true },
      ]),
    );
    const assistantLines = prefixedText(
      `${cell.id}-assistant`,
      [{ text: "  ", dimColor: true }],
      cell.assistantText || "",
      width,
    );
    out.push(...assistantLines);
    if (cell.streaming) {
      out.push(
        line(`${cell.id}-caret`, [
          { text: "  ", dimColor: true },
          { text: "▌", dimColor: true },
        ]),
      );
    }
  }
  cell.activities.forEach((activity, i) => {
    out.push(...buildActivityLines(activity, i, cell.activities.length, width, now, expanded));
  });
  cell.notes.forEach((note) => {
    const tone =
      note.level === "error" ? "danger" : note.level === "warn" ? "warning" : "active";
    out.push(
      ...prefixedText(
        note.id,
        [
          { text: `${set.continuation}`, dimColor: true },
          { text: `${note.level === "error" ? set.failed : note.level === "warn" ? set.attention : set.bullet} `, color: toneColor(tone) },
        ],
        note.text,
        width,
        {},
        [{ text: `${set.continuation}  `, dimColor: true }],
      ),
    );
  });
  return out;
}

function buildLedgerLines({
  cells,
  width,
  now,
  expanded,
  intro,
}: {
  cells: TranscriptCell[];
  width: number;
  now: number;
  expanded: boolean;
  intro?: IntroBlock;
}): LedgerLine[] {
  const lines = intro ? buildIntroLines(intro, width, now) : [];
  if (!intro && cells.length === 0) {
    lines.push(line("empty", [{ text: "Ask Daintree...", dimColor: true }]));
  }
  for (const cell of cells) {
    lines.push(...buildCellLines(cell, width, now, expanded));
  }
  return lines;
}

function renderLine(l: LedgerLine, width: number) {
  const segments = clipSegments(l.segments, width);
  return (
    <Text key={l.key}>
      {segments.map((s, i) => (
        <Text
          key={i}
          color={s.color}
          dimColor={s.dimColor}
          bold={s.bold}
          inverse={s.inverse}
        >
          {s.text}
        </Text>
      ))}
    </Text>
  );
}

export function Transcript({
  cells,
  height,
  width = 72,
  now = Date.now(),
  expanded = false,
  scrollOffset = 0,
  intro,
}: {
  cells: TranscriptCell[];
  height: number;
  width?: number;
  now?: number;
  expanded?: boolean;
  /** Rendered lines above the newest line. 0 means pinned to latest. */
  scrollOffset?: number;
  /** Optional startup/work block rendered at the top of the scrollable stream. */
  intro?: IntroBlock;
  /** Kept for older callsites; ignored because the intro is the empty state. */
  emptyText?: string;
}) {
  const allLines = buildLedgerLines({ cells, width, now, expanded, intro });
  const viewport = Math.max(1, height);
  const maxOffset = Math.max(0, allLines.length - viewport);
  const offset = Math.min(Math.max(0, scrollOffset), maxOffset);
  const start = Math.max(0, allLines.length - viewport - offset);
  const visible = allLines.slice(start, start + viewport);

  return (
    <Box flexDirection="column" height={height} overflow="hidden">
      {visible.map((l) => renderLine(l, width))}
    </Box>
  );
}
