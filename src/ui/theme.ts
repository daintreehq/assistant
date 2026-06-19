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
    // The Daintree brand green, used by the masthead logo + wordmark. Deliberately
    // distinct from `accent` (which carries the success/focus semantics across the
    // whole cockpit): the masthead is identity, not state, so it owns its own hue.
    brand: "#36CE94",
    info: "#67E8F9",
    warning: "#F6C85F",
    danger: "#FB7185",
    blocked: "#C4B5FD",
    muted: "gray",
    // Transcript user-message surfaces. The human's turn is marked by a left
    // accent bar over a *subtle* fill (only a few percent above terminal black),
    // not a four-sided box — the bar is the "who said what" anchor, the fill just
    // groups the text. Theme-aware so we never paint a bright block on a dark
    // terminal. The bar reads brighter than the fill to draw the eye; it is a
    // cool neutral (not Daintree's accent green, which is reserved for identity).
    userMessageBgDark: "#181D26",
    userMessageBgLight: "#EAEDF1",
    userMessageTextDark: "#E5E7EB",
    userMessageTextLight: "#1F2937",
    userMessageBarDark: "#6B7280",
    userMessageBarLight: "#94A3B8",
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
    // Square sibling of `branch` (U+2514, not the rounded arc U+2570). The arc
    // glyphs ╭╮╯╰ are missing from many terminal monospace fonts and get
    // substituted from a fallback font at a *different* advance width, which
    // shifts the whole row by a cell and breaks badge alignment. `└` ships in
    // every font that has `├`, so the two branch rows stay column-aligned.
    lastBranch: "└─",
    continuation: "│ ",
    bullet: "·",
    connected: "●",
    clock: "◷",
    // Epistemic-provenance marks (issue #85): a solid disc for a measured fact, a
    // hollow ring for a model inference, a query for unverified. The solid/hollow
    // contrast reads at a glance as "certain vs guessed".
    observed: "●",
    inferred: "○",
    unverified: "?",
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
  // ASCII fallbacks for the epistemic marks; must stay shape-parallel with the
  // Unicode set above (DAINTREE_ASCII=1 path).
  observed: "*",
  inferred: "o",
  unverified: "?",
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
/* Terminal theme (light / dark) for transcript surfaces                        */
/* -------------------------------------------------------------------------- */

/**
 * How the host terminal is themed. We resolve this from explicit config/env
 * rather than fragile background probing: a wrong guess here paints a bright
 * block on a dark terminal (a real Claude Code regression we deliberately avoid).
 *   dark  — light text on a dim fill (the default)
 *   light — dark text on a pale fill
 *   ansi  — no fill, borders only, lean on the 16-color palette
 *   none  — no color at all (rely on glyphs + borders)
 */
export type TerminalThemeMode = "dark" | "light" | "ansi" | "none";

/**
 * Resolve the terminal theme. Daintree can inject `DAINTREE_TERMINAL_THEME`
 * when it knows the host panel's appearance; `DAINTREE_THEME` is the manual
 * override. Defaults to dark — the common case for a hosted side panel.
 */
export function terminalThemeMode(): TerminalThemeMode {
  const v = (
    process.env.DAINTREE_THEME ||
    process.env.DAINTREE_TERMINAL_THEME ||
    ""
  ).toLowerCase();
  if (v === "light" || v === "dark" || v === "ansi" || v === "none") return v;
  return "dark";
}

export interface MessageSurface {
  /** Color of the left accent bar (the human's "who said what" anchor). */
  barColor: string;
  textColor?: string;
  /** Subtle fill behind the text; omitted in ansi/none modes (unreliable). */
  backgroundColor?: string;
  dimText: boolean;
}

/**
 * The surface (left bar / text / fill) for a user message, theme-aware. In
 * truecolor themes the bar sits over a faint fill; in ansi/none we drop the fill
 * (16-color backgrounds clash unpredictably) and lean on the bar alone.
 */
export function userMessageSurface(
  mode: TerminalThemeMode = terminalThemeMode(),
): MessageSurface {
  if (mode === "none") {
    return { barColor: ui.color.muted, dimText: true };
  }
  if (mode === "ansi") {
    return { barColor: "gray", dimText: false };
  }
  if (mode === "light") {
    return {
      barColor: ui.color.userMessageBarLight,
      textColor: ui.color.userMessageTextLight,
      backgroundColor: ui.color.userMessageBgLight,
      dimText: false,
    };
  }
  return {
    barColor: ui.color.userMessageBarDark,
    textColor: ui.color.userMessageTextDark,
    backgroundColor: ui.color.userMessageBgDark,
    dimText: false,
  };
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
    case "rate_limited":
      return make("rate-limited", "warning", set.attention, 1);
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

/** A glyph + tone + short label conveying a fact's epistemic provenance (#85). */
export interface EpistemicMark {
  /** Signature glyph: solid disc / hollow ring / query. */
  symbol: string;
  tone: Tone;
  color: string;
  /** Three-letter scan label: "obs" / "inf" / "unv". */
  label: string;
  /** Spelled-out form for tooltips / accessibility. */
  full: string;
}

/**
 * Map an epistemic kind to its render mark. `inferred` and `unverified` are
 * deliberately dimmed (neutral tone) so an inference never visually outweighs the
 * observed facts beside it; an observed fact gets the confident accent color.
 * Returns null for an absent kind so callers can render nothing rather than a
 * placeholder. `ascii` honors the DAINTREE_ASCII fallback like every other glyph.
 */
export function epistemicMark(
  kind: string | undefined,
  ascii: boolean = !unicodeOk(),
): EpistemicMark | null {
  const set = glyphs(ascii);
  switch (kind) {
    case "observed":
      return {
        symbol: set.observed,
        tone: "success",
        color: toneColor("success"),
        label: "obs",
        full: "observed",
      };
    case "inferred":
      return {
        symbol: set.inferred,
        tone: "neutral",
        color: toneColor("neutral"),
        label: "inf",
        full: "inferred",
      };
    case "unverified":
      return {
        symbol: set.unverified,
        tone: "neutral",
        color: toneColor("neutral"),
        label: "unv",
        full: "unverified",
      };
    default:
      return null;
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
