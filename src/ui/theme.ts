/**
 * Visual theme for the Ink UI. Restrained palette: crisp borders, subdued labels,
 * strong severity cues. The assistant should read like an operations console.
 */
export const theme = {
  brand: "#6EE7B7",
  dim: "gray",
  ok: "green",
  warn: "yellow",
  error: "red",
  info: "cyan",
  blocked: "magenta",
  border: "gray",
} as const;

/** Map a queue severity to a color. */
export function severityColor(severity: string): string {
  switch (severity) {
    case "done":
      return theme.ok;
    case "info":
      return theme.info;
    case "attention":
      return theme.warn;
    case "urgent":
    case "blocked":
      return theme.blocked;
    case "error":
      return theme.error;
    default:
      return theme.dim;
  }
}

/**
 * Signature glyphs. One job each, used consistently across the whole UI:
 *   › active assistant action   ◌ background / working   ! needs a human   ✓ done
 */
export const glyph = {
  active: "›",
  working: "◌",
  attention: "!",
  done: "✓",
  exited: "×",
} as const;

export interface WatcherBadge {
  label: string;
  color: string;
  /** One signature glyph describing the state. */
  symbol: string;
  /**
   * Human-scan priority (lower = more urgent). Drives sidebar ordering:
   * needs input → failed/blocked → escalated → working → pending → done → exited.
   */
  priority: number;
}

/** Map a watcher classification to a short badge for the sidebar. */
export function watcherBadge(classification?: string): WatcherBadge {
  switch (classification) {
    case "waiting_for_input":
    case "permission_prompt":
      return { label: "needs input", color: theme.warn, symbol: glyph.attention, priority: 0 };
    case "command_failed":
    case "tests_failed":
      return { label: "failed", color: theme.error, symbol: glyph.attention, priority: 1 };
    case "merge_conflict":
      return { label: "blocked", color: theme.blocked, symbol: glyph.attention, priority: 1 };
    case "needs_large_model":
      return { label: "escalated", color: theme.warn, symbol: glyph.attention, priority: 2 };
    case "still_working":
      return { label: "working", color: theme.info, symbol: glyph.working, priority: 3 };
    case "tests_passed":
    case "completed_success":
      return { label: "done", color: theme.ok, symbol: glyph.done, priority: 5 };
    case "completed_unknown":
      return { label: "done", color: theme.dim, symbol: glyph.done, priority: 5 };
    case "terminal_exited":
      return { label: "exited", color: theme.dim, symbol: glyph.exited, priority: 6 };
    default:
      return { label: classification ?? "pending", color: theme.dim, symbol: glyph.working, priority: 4 };
  }
}

// Severity ranks for ordering (lower = more urgent). The status line and the
// attention banner share this so they never disagree on the worst event.
const SEVERITY_RANK: Record<string, number> = {
  blocked: 0,
  urgent: 0,
  error: 1,
  attention: 2,
  done: 4,
  info: 5,
  debug: 6,
};

/** The most urgent severity among the events, or null when there are none. */
export function topSeverity(events: ReadonlyArray<{ severity: string }>): string | null {
  let best: string | null = null;
  let bestRank = Infinity;
  for (const e of events) {
    const rank = SEVERITY_RANK[e.severity] ?? 3;
    if (rank < bestRank) {
      bestRank = rank;
      best = e.severity;
    }
  }
  return best;
}

/** Map a queue severity to a signature glyph + color. */
export function severitySymbol(severity: string): { symbol: string; color: string } {
  switch (severity) {
    case "done":
      return { symbol: glyph.done, color: theme.ok };
    case "attention":
      return { symbol: glyph.attention, color: theme.warn };
    case "urgent":
    case "blocked":
      return { symbol: glyph.attention, color: theme.blocked };
    case "error":
      return { symbol: glyph.exited, color: theme.error };
    case "info":
      return { symbol: "·", color: theme.info };
    default:
      return { symbol: "·", color: theme.dim };
  }
}

/** Short tier label for the compact header capsule. */
export function tierShort(tier: string): string {
  switch (tier) {
    case "supervisor":
      return "sup";
    case "system":
      return "sys";
    case "operator":
      return "op";
    default:
      return tier.slice(0, 3);
  }
}
