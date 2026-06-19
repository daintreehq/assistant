/**
 * Reusable visual primitives for the control room. Every stateful primitive
 * shows a symbol AND text in addition to color, so meaning survives no-color and
 * screen-reader rendering. Feature components compose these instead of hand-
 * rolling colored glyphs.
 */
import { Box, Text } from "ink";
import { glyphs, toneColor, toneGlyph, ui, unicodeOk, type Tone } from "./theme.js";

/** ◆ DAINTREE — the brand signature. Functional, not decorative. */
export function BrandMark({ label = "DAINTREE" }: { label?: string }) {
  return (
    <Text>
      <Text color={ui.color.accent}>{glyphs().brand} </Text>
      <Text bold color={ui.color.accent}>
        {label}
      </Text>
    </Text>
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
    <Text color={toneColor(tone)}>
      {sym} {label.toUpperCase()}
    </Text>
  );
}

/** A quiet uppercase section heading (NOW, ATTENTION, AGENTS…). */
export function SectionLabel({ children }: { children: string }) {
  return (
    <Text dimColor bold>
      {children.toUpperCase()}
    </Text>
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
    <Text>
      <Text color={ui.color.info}>{keyName}</Text>
      <Text dimColor> {action}</Text>
    </Text>
  );
}

/**
 * A custom Ink border whose only meaningful edge is the top — the horizontal rule
 * character. Used by the flex {@link Divider}: a top-border-only box yoga-sizes the
 * rule to the live width. Corners/sides are set to the same glyph so a seamless rule
 * results even if Ink draws a corner; the disabled edges (left/right/bottom) keep
 * them out of the render.
 */
const RULE_BORDER = (ch: string) => ({
  topLeft: ch,
  top: ch,
  topRight: ch,
  right: ch,
  bottomRight: ch,
  bottom: ch,
  bottomLeft: ch,
  left: ch,
});

/** A full-width horizontal rule. */
export function Divider({
  width,
  color,
}: {
  /** Fixed rule length. Omit in the repainting region to fill the live width. */
  width?: number;
  color?: string;
}) {
  const ch = unicodeOk() ? "─" : "-";
  // Flex rule: when no width is given, fill the parent's CURRENT laid-out width via
  // yoga instead of a fixed character count. During a resize, React's width prop
  // lags the real terminal by a tick, so a char-counted rule would momentarily run
  // past the edge, wrap, and orphan a row into scrollback (the live region is
  // erased by Ink's logical line count, which can't see the wrap). A top-border-only
  // box draws a clean rule that yoga sizes to the live width — no fixed count, and
  // no ellipsis (which `wrap="truncate"` would leave on a clipped run).
  if (width === undefined) {
    return (
      <Box
        width="100%"
        borderStyle={RULE_BORDER(ch)}
        borderTop
        borderBottom={false}
        borderLeft={false}
        borderRight={false}
        borderColor={color}
        borderDimColor={!color}
      />
    );
  }
  return (
    <Text color={color} dimColor={!color}>
      {ch.repeat(Math.max(1, width))}
    </Text>
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
  return <Text dimColor>{formatDuration(ms)}</Text>;
}

/** A calm, centered empty-state line (never an alarming "0"). */
export function EmptyState({ children }: { children: string }) {
  return (
    <Box>
      <Text dimColor>{children}</Text>
    </Box>
  );
}
