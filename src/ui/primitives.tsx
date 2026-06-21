/**
 * Reusable visual primitives for the control room. Every stateful primitive
 * shows a symbol AND text in addition to color, so meaning survives no-color and
 * screen-reader rendering. Feature components compose these instead of hand-
 * rolling colored glyphs.
 *
 * OpenTUI port: Ink `<Box>`/`<Text>` become the lowercase native intrinsics
 * `<box>`/`<text>`. Text style is `fg=` + an `attributes=` bitfield
 * ({@link TextAttributes}) rather than Ink's `color`/`dimColor`/`bold` props, and
 * an Ink `<Text>` that nested other `<Text>` runs becomes one `<text>` whose
 * children are `<span>` (a native `<text>` may not contain another `<text>`). The
 * {@link Dim}/{@link Muted}/{@link Bold} span helpers keep that mechanical.
 */
import type { ReactNode } from "react";
import { TextAttributes } from "@opentui/core";
import { glyphs, toneColor, toneGlyph, ui, unicodeOk, type Tone } from "./theme.js";

/** A dim inline run (Ink `<Text dimColor>` → `<span attributes=DIM>`). */
export function Dim({ children }: { children: ReactNode }) {
  return <span attributes={TextAttributes.DIM}>{children}</span>;
}

/** A muted (gray) inline run for ids/timestamps/secondary metadata. */
export function Muted({ children }: { children: ReactNode }) {
  return <span fg={ui.color.muted}>{children}</span>;
}

/** A bold inline run (Ink `<Text bold>` → `<span attributes=BOLD>`). */
export function Bold({ children }: { children: ReactNode }) {
  return <span attributes={TextAttributes.BOLD}>{children}</span>;
}

/** ◆ DAINTREE — the brand signature. Functional, not decorative. */
export function BrandMark({ label = "DAINTREE" }: { label?: string }) {
  return (
    <text>
      <span fg={ui.color.accent}>{glyphs().brand} </span>
      <span fg={ui.color.accent} attributes={TextAttributes.BOLD}>
        {label}
      </span>
    </text>
  );
}

/**
 * A state chip: glyph + UPPER label tinted by tone. The glyph and word both
 * carry the meaning, so the chip still reads with color stripped.
 *
 *   ◌ WORKING   ! NEEDS INPUT   ✓ DONE   × FAILED
 */
export function StateBadge({
  tone,
  label,
  glyph,
}: {
  tone: Tone;
  label: string;
  /** Override the tone's default glyph. */
  glyph?: string;
}) {
  const sym = glyph ?? toneGlyph(tone);
  return (
    <text fg={toneColor(tone)}>
      {sym} {label.toUpperCase()}
    </text>
  );
}

/** A quiet uppercase section heading (NOW, ATTENTION, AGENTS…). */
export function SectionLabel({ children }: { children: string }) {
  return (
    <text attributes={TextAttributes.DIM | TextAttributes.BOLD}>
      {children.toUpperCase()}
    </text>
  );
}

/** A keyboard affordance: the key in cyan, its action dim. */
export function KeyHint({
  keyName,
  action,
}: {
  keyName: string;
  action: string;
}) {
  return (
    <text>
      <span fg={ui.color.info}>{keyName}</span>
      <span attributes={TextAttributes.DIM}> {action}</span>
    </text>
  );
}

/**
 * A full-width horizontal rule. With a numeric `width` it is a run of the rule
 * character; without one it flex-fills via a top-border-only box (yoga sizes the
 * rule to the live width). The native renderer reflows it cleanly on resize, so the
 * Ink-era resize-orphan hazard (and the custom border-char workaround) is gone.
 */
export function Divider({
  width,
  color,
}: {
  /** Fixed rule length. Omit in a flex container to fill the live width. */
  width?: number;
  color?: string;
}) {
  const ch = unicodeOk() ? "─" : "-";
  if (width === undefined) {
    return (
      <box
        style={{ flexGrow: 1, width: "100%" }}
        border={["top"]}
        borderColor={color ?? ui.color.muted}
      />
    );
  }
  return (
    <text fg={color} attributes={color ? TextAttributes.NONE : TextAttributes.DIM}>
      {ch.repeat(Math.max(1, width))}
    </text>
  );
}

/** Format a millisecond span the way an operator scans it. */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const secs = Math.round(ms / 1000);
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  const rem = secs % 60;
  return `${String(mins).padStart(2, "0")}:${String(rem).padStart(2, "0")}`;
}

/** A dim elapsed/duration token. */
export function Duration({ ms }: { ms: number }) {
  return <text attributes={TextAttributes.DIM}>{formatDuration(ms)}</text>;
}

/** A calm, centered empty-state line (never an alarming "0"). */
export function EmptyState({ children }: { children: string }) {
  return (
    <box>
      <text attributes={TextAttributes.DIM}>{children}</text>
    </box>
  );
}
