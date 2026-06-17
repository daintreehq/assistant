/**
 * The sidebar view model. A single, pure, Ink-free place that decides what the
 * narrow operations cockpit shows and how every row is truncated and prioritized.
 *
 * Keeping this out of the components means: one source of truth for truncation
 * and ordering, and exact 55-column output we can snapshot in tests without
 * mounting React.
 */
import type { DashboardState, PendingConfirm, TimelineItem } from "../types.js";
import type { QueueEvent } from "../../schemas.js";
import type { TerminalPreview } from "../hooks/useTerminalPreview.js";
import { glyph, severitySymbol, theme, tierShort, watcherBadge } from "../theme.js";
import { cellBudget, fit } from "../../utils/text.js";

export type Density = "comfortable" | "compact" | "dense";

export interface HeaderStatus {
  live: boolean;
  liveLabel: string;
  project: string;
  mcpOk: boolean;
  tier: string;
  watcherCount: number;
  attentionCount: number;
}

export type NowState =
  | { kind: "confirm"; symbol: string; color: string; title: string; detail: string }
  | { kind: "running"; symbol: string; color: string; title: string; detail: string }
  | { kind: "attention"; symbol: string; color: string; title: string; detail: string }
  | { kind: "idle"; symbol: string; color: string; title: string; detail: string };

export interface AttentionRow {
  id: string;
  symbol: string;
  color: string;
  title: string;
  evidence?: string;
  related?: string;
  actions?: string;
}

export interface WatcherRow {
  id: string;
  symbol: string;
  color: string;
  title: string;
  status: string;
  age: string;
}

export interface TerminalRow {
  id: string;
  state: string;
  line: string;
  isOutput: boolean;
}

export interface TimerRow {
  id: string;
  clock: string;
  title: string;
}

export interface AuditRow {
  id: string;
  symbol: string;
  color: string;
  name: string;
  ms: string;
}

export interface RecentRow {
  id: string;
  who: string;
  text: string;
}

export interface SidebarModel {
  status: HeaderStatus;
  now: NowState;
  attention: AttentionRow[];
  watchers: WatcherRow[];
  terminals: TerminalRow[];
  timers: TimerRow[];
  audit: AuditRow[];
  recent: RecentRow[];
}

export interface BuildSidebarOptions {
  columns: number;
  rows: number;
  now: number;
  project: string;
  tier: string;
  busy: boolean;
  pendingConfirm: PendingConfirm | null;
}

/** Density picked from the available terminal box. */
export function densityFor(columns: number, rows: number): Density {
  if (columns < 44) return "dense";
  if (rows < 22) return "compact";
  return "comfortable";
}

/** "now" / "MM:SS" / "Hh" age string from an elapsed millisecond delta. */
export function formatAge(deltaMs: number): string {
  if (!Number.isFinite(deltaMs) || deltaMs < 0) return "—";
  const s = Math.floor(deltaMs / 1000);
  if (s < 3) return "now";
  if (s < 3600) {
    const m = Math.floor(s / 60);
    const ss = s % 60;
    return `${String(m).padStart(2, "0")}:${String(ss).padStart(2, "0")}`;
  }
  return `${Math.floor(s / 3600)}h`;
}

function clock(ms: number): string {
  try {
    return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  } catch {
    return "—";
  }
}

// eslint-disable-next-line no-control-regex
const ANSI_RE = /\[[0-9;?]*[A-Za-z]/g;

/** Strip ANSI escapes, carriage returns, and spinner residue from a tail line. */
export function cleanTerminalLine(line: string): string {
  return line
    .replace(ANSI_RE, "")
    .replace(/\r/g, "")
    .replace(/[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏◐◓◑◒|/\\-]+\s*$/u, "")
    .trim();
}

function isMeaningfulLine(line: string): boolean {
  if (!line) return false;
  // Pure punctuation / box-drawing noise carries no information.
  return /[A-Za-z0-9]/.test(line);
}

/** The most recent line of real output from a terminal tail. */
export function extractMeaningfulTerminalLine(tail: string): string {
  const lines = tail.split("\n").map(cleanTerminalLine).filter(isMeaningfulLine);
  return lines[lines.length - 1] ?? "";
}

function deriveNow(
  topAttention: QueueEvent | undefined,
  watcherCount: number,
  timeline: TimelineItem[],
  opts: BuildSidebarOptions,
  width: number,
): NowState {
  if (opts.pendingConfirm) {
    const req = opts.pendingConfirm.request;
    return {
      kind: "confirm",
      symbol: "?",
      color: theme.warn,
      title: `confirm · ${req.toolName}`,
      detail: fit(req.summary || "approve to continue", width),
    };
  }

  if (opts.busy) {
    // Prefer a running tool; fall back to a streaming assistant ("thinking").
    for (let i = timeline.length - 1; i >= 0; i--) {
      const item = timeline[i];
      if (item.kind === "tool" && item.ok === undefined) {
        return {
          kind: "running",
          symbol: glyph.active,
          color: theme.info,
          title: `running · ${item.name}`,
          detail: fit("working on it", width),
        };
      }
      if (item.kind === "assistant" && item.streaming) break;
    }
    return {
      kind: "running",
      symbol: glyph.active,
      color: theme.info,
      title: "thinking",
      detail: fit("composing a response", width),
    };
  }

  if (topAttention) {
    const sym = severitySymbol(topAttention.severity);
    return {
      kind: "attention",
      symbol: sym.symbol,
      color: sym.color,
      title: fit(topAttention.title, width),
      detail: fit(topAttention.summary || "needs a look", width),
    };
  }

  const w = watcherCount;
  return {
    kind: "idle",
    symbol: glyph.done,
    color: theme.dim,
    title: "idle",
    detail: w ? `watching ${w} ${w === 1 ? "watcher" : "watchers"}` : "nothing to watch",
  };
}

function shortActions(labels: string[]): string {
  return labels
    .slice(0, 3)
    .map((l) => l.trim().toLowerCase())
    .filter(Boolean)
    .join(" · ");
}

// Severity ranks for sidebar ordering (lower = more urgent). Shared so the "Now"
// card and the attention list agree on which event is the most important one.
const SEVERITY_RANK: Record<string, number> = {
  blocked: 0,
  urgent: 0,
  error: 1,
  attention: 2,
  done: 4,
  info: 5,
  debug: 6,
};

/** Most-urgent-first, then most-recent-first (by update recency, matching the DB). */
function compareEvents(a: QueueEvent, b: QueueEvent): number {
  const pa = SEVERITY_RANK[a.severity] ?? 3;
  const pb = SEVERITY_RANK[b.severity] ?? 3;
  return pa - pb || (b.updatedAt ?? b.createdAt) - (a.updatedAt ?? a.createdAt);
}

function buildAttention(
  sorted: QueueEvent[],
  width: number,
  limit: number,
): AttentionRow[] {
  return sorted.slice(0, limit).map((e) => {
    const sym = severitySymbol(e.severity);
    const related = e.target?.terminalId
      ? fit(`terminal ${e.target.terminalId}`, width, 2)
      : e.target?.worktreeId
        ? fit(`worktree ${e.target.worktreeId}`, width, 2)
        : undefined;
    const actions = e.recommendedActions?.length
      ? fit(shortActions(e.recommendedActions.map((a) => a.label)), width, 2)
      : undefined;
    return {
      id: e.id,
      symbol: sym.symbol,
      color: sym.color,
      title: fit(e.title, width, 2),
      evidence: e.evidence?.[0] ? fit(e.evidence[0], width, 2) : fit(e.summary, width, 2),
      related,
      actions,
    };
  });
}

function buildWatchers(
  dashboard: DashboardState,
  opts: BuildSidebarOptions,
  width: number,
  limit: number,
): WatcherRow[] {
  // status (~12) + age (~6) + symbol/spaces (~3) share the row with the title.
  const reserved = 21;
  return [...dashboard.watchers]
    .map((w) => ({ w, badge: watcherBadge(w.lastClassification) }))
    .sort((a, b) => a.badge.priority - b.badge.priority || b.w.createdAt - a.w.createdAt)
    .slice(0, limit)
    .map(({ w, badge }) => ({
      id: w.id,
      symbol: badge.symbol,
      color: badge.color,
      title: fit(w.title || w.id, width, reserved),
      status: badge.label,
      age: formatAge(opts.now - (w.lastCheckedAt ?? w.createdAt)),
    }));
}

function buildTerminals(
  previews: TerminalPreview[],
  width: number,
  limit: number,
): TerminalRow[] {
  return previews.slice(0, limit).map((p) => {
    const state = p.agentState ?? p.runtimeStatus ?? "unknown";
    const line = extractMeaningfulTerminalLine(p.tail);
    // In dense mode the line shares its row with `id state: "…"`; reserve for
    // that so a long line can't wrap and blow the row budget.
    const reserved = Math.min(width - 4, p.terminalId.length + state.length + 6);
    return {
      id: p.terminalId,
      state,
      line: fit(line, width, reserved),
      isOutput: line.length > 0,
    };
  });
}

function buildTimers(
  dashboard: DashboardState,
  width: number,
  limit: number,
): TimerRow[] {
  return dashboard.timers.slice(0, limit).map((t) => ({
    id: t.id,
    clock: clock(t.fireAt),
    title: fit(t.title, width, 7),
  }));
}

function buildAudit(
  dashboard: DashboardState,
  width: number,
  limit: number,
): AuditRow[] {
  return dashboard.audit.slice(0, limit).map((r) => {
    const color =
      r.outcome === "ok"
        ? theme.ok
        : r.outcome === "error"
          ? theme.error
          : r.outcome === "denied"
            ? theme.warn
            : theme.dim;
    const symbol = r.outcome === "ok" ? glyph.done : r.outcome === "error" ? glyph.exited : "·";
    return {
      id: r.id,
      symbol,
      color,
      name: fit(r.toolName, width, 8),
      ms: `${r.durationMs}ms`,
    };
  });
}

function buildRecent(timeline: TimelineItem[], width: number, limit: number): RecentRow[] {
  const rows: RecentRow[] = [];
  for (let i = timeline.length - 1; i >= 0 && rows.length < limit; i--) {
    const item = timeline[i];
    if (item.kind === "user") {
      rows.unshift({ id: item.id, who: "you", text: fit(item.text, width, 5) });
    } else if (item.kind === "assistant" && item.text.trim()) {
      rows.unshift({ id: item.id, who: "ai", text: fit(item.text.replace(/\s+/g, " "), width, 5) });
    }
  }
  return rows;
}

export function buildSidebarModel(
  dashboard: DashboardState,
  timeline: TimelineItem[],
  previews: TerminalPreview[],
  opts: BuildSidebarOptions,
): SidebarModel {
  const width = cellBudget(opts.columns);
  const connected = dashboard.mcp.connected;
  // Sort the inbox once so the "Now" card and the attention list never disagree
  // on the top event.
  const sortedInbox = [...dashboard.inbox].sort(compareEvents);
  return {
    status: {
      live: connected,
      liveLabel: connected ? "live" : "degraded",
      // The header is a fixed 2 lines; truncate the project so a long repo dir
      // can't wrap line 2 and invalidate the body-height budget. Reserve room for
      // the right-side capsule (mcp ✓  op · 4w · 2!).
      project: fit(opts.project, Math.max(8, opts.columns - 24)),
      mcpOk: connected,
      tier: tierShort(opts.tier),
      watcherCount: dashboard.watchers.length,
      attentionCount: dashboard.inbox.length,
    },
    now: deriveNow(sortedInbox[0], dashboard.watchers.length, timeline, opts, width),
    attention: buildAttention(sortedInbox, width, 3),
    watchers: buildWatchers(dashboard, opts, width, 8),
    terminals: buildTerminals(previews, width, 2),
    timers: buildTimers(dashboard, width, 4),
    audit: buildAudit(dashboard, width, 4),
    recent: buildRecent(timeline, width, 4),
  };
}
