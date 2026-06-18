/**
 * Semantic design system for the Daintree control room.
 *
 * Colors carry MEANING, not decoration. Each tone has exactly one job, so a
 * glance at a hue tells you what kind of thing you're looking at. Feature
 * components should request a semantic `Tone` (via {@link toneColor} / the
 * primitives in primitives.tsx) rather than hard-coding a hex value.
 *
 *   accent  (green)  Daintree's identity + focused controls + success
 *   info    (cyan)   active/working, informational navigation
 *   warning (yellow) action required by a human
 *   danger  (red)    a failed operation
 *   blocked (purple) blocked / externally constrained
 *   muted   (gray)   ids, timestamps, args, secondary metadata
 *
 * Body prose uses the terminal's own foreground — never a forced white.
 */

export const ui = {
  color: {
    accent: "#6EE7B7",
    accentQuiet: "#A7F3D0",
    info: "#67E8F9",
    warning: "#F6C85F",
    danger: "#FB7185",
    blocked: "#C4B5FD",
    muted: "gray",
  },
  /** Unicode signature glyphs. Use {@link glyphs} when ASCII fallback matters. */
  glyph: {
    brand: "◆",
    active: "◌",
    attention: "!",
    done: "✓",
    failed: "×",
    waiting: "◷",
    pending: "◦",
    branch: "├─",
    lastBranch: "╰─",
    continuation: "│ ",
    bullet: "·",
    connected: "●",
    clock: "◷",
  },
  spacing: {
    section: 1,
    gutter: 2,
  },
} as const;

/** Semantic role of a piece of state. Drives both color and badge glyph. */
export type Tone =
  | "neutral"
  | "active"
  | "success"
  | "warning"
  | "danger"
  | "blocked";

/** The one color for a semantic tone. */
export function toneColor(tone: Tone): string {
  switch (tone) {
    case "active":
      return ui.color.info;
    case "success":
      return ui.color.accent;
    case "warning":
      return ui.color.warning;
    case "danger":
      return ui.color.danger;
    case "blocked":
      return ui.color.blocked;
    case "neutral":
    default:
      return ui.color.muted;
  }
}

/* -------------------------------------------------------------------------- */
/* Glyphs + ASCII fallback                                                      */
/* -------------------------------------------------------------------------- */

const UNICODE_GLYPHS = ui.glyph;
const ASCII_GLYPHS = {
  brand: "#",
  active: "*",
  attention: "!",
  done: "+",
  failed: "x",
  waiting: "~",
  pending: "o",
  branch: "|-",
  lastBranch: "`-",
  continuation: "| ",
  bullet: "-",
  connected: "*",
  clock: "@",
} as const;

export type GlyphSet = typeof UNICODE_GLYPHS;

/**
 * True when the terminal can be trusted with our signature Unicode glyphs.
 * Conservative: only falls back to ASCII when explicitly asked (DAINTREE_ASCII=1)
 * or a clearly non-UTF locale is set. Branch/badge alignment then stays stable.
 */
export function unicodeOk(): boolean {
  if (process.env.DAINTREE_ASCII === "1") return false;
  const enc = (
    process.env.LC_ALL ||
    process.env.LC_CTYPE ||
    process.env.LANG ||
    ""
  ).toLowerCase();
  if (enc && !enc.includes("utf")) return false;
  return true;
}

/** The active glyph set, honoring the ASCII fallback. */
export function glyphs(ascii: boolean = !unicodeOk()): GlyphSet {
  return ascii ? (ASCII_GLYPHS as unknown as GlyphSet) : UNICODE_GLYPHS;
}

/** The badge glyph (text symbol) that always accompanies a tone's color. */
export function toneGlyph(tone: Tone, set: GlyphSet = glyphs()): string {
  switch (tone) {
    case "active":
      return set.active;
    case "success":
      return set.done;
    case "warning":
      return set.attention;
    case "danger":
      return set.failed;
    case "blocked":
      return set.attention;
    case "neutral":
    default:
      return set.bullet;
  }
}

/* -------------------------------------------------------------------------- */
/* Back-compat palette (legacy importers)                                      */
/* -------------------------------------------------------------------------- */

/**
 * The original flat palette, kept so any not-yet-migrated importer compiles.
 * New code should prefer {@link ui} + {@link toneColor}.
 */
export const theme = {
  brand: ui.color.accent,
  dim: ui.color.muted,
  ok: "green",
  warn: ui.color.warning,
  error: ui.color.danger,
  info: ui.color.info,
  blocked: ui.color.blocked,
  border: "gray",
} as const;

/** Legacy single glyph map (one job each). Prefer {@link ui.glyph}. */
export const glyph = {
  active: "›",
  working: ui.glyph.active,
  attention: ui.glyph.attention,
  done: ui.glyph.done,
  exited: ui.glyph.failed,
} as const;

/* -------------------------------------------------------------------------- */
/* Severity + classification mapping                                           */
/* -------------------------------------------------------------------------- */

/** Map a queue severity to a semantic tone. */
export function severityTone(severity: string): Tone {
  switch (severity) {
    case "done":
      return "success";
    case "info":
      return "active";
    case "attention":
      return "warning";
    case "urgent":
    case "blocked":
      return "blocked";
    case "error":
      return "danger";
    default:
      return "neutral";
  }
}

/** Map a queue severity to a color (back-compat). */
export function severityColor(severity: string): string {
  return toneColor(severityTone(severity));
}

export interface WatcherBadge {
  label: string;
  tone: Tone;
  color: string;
  /** One signature glyph describing the state. */
  symbol: string;
  /**
   * Human-scan priority (lower = more urgent). Drives ordering:
   * needs input → failed/blocked → escalated → working → pending → done → exited.
   */
  priority: number;
}

/** Map a watcher classification to a short badge. */
export function watcherBadge(classification?: string): WatcherBadge {
  const set = glyphs();
  const make = (
    label: string,
    tone: Tone,
    symbol: string,
    priority: number,
  ): WatcherBadge => ({ label, tone, color: toneColor(tone), symbol, priority });
  switch (classification) {
    case "waiting_for_input":
    case "permission_prompt":
      return make("needs input", "warning", set.attention, 0);
    case "command_failed":
    case "tests_failed":
      return make("failed", "danger", set.failed, 1);
    case "merge_conflict":
      return make("blocked", "blocked", set.attention, 1);
    case "needs_large_model":
      return make("escalated", "warning", set.attention, 2);
    case "still_working":
      return make("working", "active", set.active, 3);
    case "tests_passed":
    case "completed_success":
      return make("done", "success", set.done, 5);
    case "completed_unverified":
      return make("review", "warning", set.attention, 2);
    case "completed_unknown":
      return make("done", "neutral", set.done, 5);
    case "terminal_exited":
      return make("exited", "neutral", set.failed, 6);
    default:
      return make(classification ?? "pending", "neutral", set.pending, 4);
  }
}

// Severity ranks for ordering (lower = more urgent). The status line and the
// attention surfaces share this so they never disagree on the worst event.
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
export function topSeverity(
  events: ReadonlyArray<{ severity: string }>,
): string | null {
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

/** Map a queue severity to a signature glyph + color (back-compat). */
export function severitySymbol(severity: string): {
  symbol: string;
  color: string;
} {
  const tone = severityTone(severity);
  return { symbol: toneGlyph(tone), color: toneColor(tone) };
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
