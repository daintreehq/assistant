/**
 * The home surface is a Claude/Codex-style vertical ledger: an intro/work block
 * at the top, then chronological user + Daintree turns separated by horizontal
 * rules. The composer stays fixed below this component.
 *
 * Scrollback is the host TERMINAL's, not ours: finalized turns/notes/commands
 * are emitted through Ink's <Static>, which commits each block to the normal
 * buffer exactly once and never repaints it — so wheel/trackpad scrolling works
 * natively (this is how Claude Code behaves). Only the trailing live block (the
 * in-flight turn) stays in the repainting frame. Static is append-only: a cell
 * may move from "live" to "committed" but committed blocks never reorder or
 * drop, which is why the live tail is everything from the active turn onward.
 */
import { Box, Static, Text } from "ink";
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
  /** Path of the active debug log, shown under the header so it can be tailed. */
  logFile?: string;
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
  if (intro.logging && intro.logFile) {
    out.push(
      line("intro-logging", [
        { text: "logging to ", dimColor: true },
        { text: truncate(intro.logFile, Math.max(12, width - 12)), color: ui.color.warning },
      ]),
    );
  }
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
      { text: "scroll the terminal for history · ^O ops · / cmds · ^C exit", dimColor: true },
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

/** The index of the active (still-mutating) turn, or -1. Everything from here
 *  on is the live tail; everything before it is committed to <Static>. */
function liveTailIndex(cells: TranscriptCell[]): number {
  for (let i = cells.length - 1; i >= 0; i--) {
    const c = cells[i];
    if (c.kind === "turn") return c.state === "active" ? i : -1;
  }
  return -1;
}

/** A committed block fed to <Static>: the intro banner, then each finalized cell.
 *  Built lazily inside the Static child so buildCellLines runs once per block. */
type StaticBlock =
  | { key: string; kind: "intro"; intro: IntroBlock }
  | { key: string; kind: "cell"; cell: TranscriptCell };

function renderLedger(lines: LedgerLine[], width: number) {
  return lines.map((l) => renderLine(l, width));
}

export function Transcript({
  cells,
  width = 72,
  now = Date.now(),
  expanded = false,
  liveHeight,
  intro,
}: {
  cells: TranscriptCell[];
  width?: number;
  now?: number;
  expanded?: boolean;
  /**
   * Max rendered lines for the inline live region. Ink wipes the terminal
   * (scrollback included) and re-dumps all static history whenever the
   * repainting frame overflows the viewport, so the caller bounds the live tail
   * to the rows left below the footer. Omitted → unbounded (tests/non-TTY).
   */
  liveHeight?: number;
  /** Optional startup/work block committed once at the top of the stream. */
  intro?: IntroBlock;
}) {
  const tail = liveTailIndex(cells);
  const committed = tail >= 0 ? cells.slice(0, tail) : cells;
  const liveCells = tail >= 0 ? cells.slice(tail) : [];

  // Static is append-only and renders each item exactly once; the intro is the
  // first immutable block so it lands at the very top of the terminal scrollback.
  const blocks: StaticBlock[] = [];
  if (intro) blocks.push({ key: "intro", kind: "intro", intro });
  for (const cell of committed) blocks.push({ key: cell.id, kind: "cell", cell });

  // Show only the TAIL of the live block so the repainting frame stays within
  // the viewport. The clipped-off top isn't lost — the moment this turn settles
  // it leaves the live tail and commits to <Static> in full (buildCellLines,
  // unclipped), landing the whole turn in the terminal's scrollback.
  const allLiveLines = liveCells.flatMap((cell) =>
    buildCellLines(cell, width, now, expanded),
  );
  const liveLines =
    liveHeight != null && allLiveLines.length > liveHeight
      ? allLiveLines.slice(allLiveLines.length - liveHeight)
      : allLiveLines;
  const showEmpty = !intro && committed.length === 0 && liveCells.length === 0;

  return (
    <Box flexDirection="column">
      <Static items={blocks}>
        {(block) => (
          <Box key={block.key} flexDirection="column">
            {renderLedger(
              block.kind === "intro"
                ? buildIntroLines(block.intro, width, now)
                : buildCellLines(block.cell, width, now, expanded),
              width,
            )}
          </Box>
        )}
      </Static>
      {liveLines.length > 0 ? (
        <Box flexDirection="column">{renderLedger(liveLines, width)}</Box>
      ) : null}
      {showEmpty ? <Text dimColor>Ask Daintree...</Text> : null}
    </Box>
  );
}
