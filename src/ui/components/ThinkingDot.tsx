import { Text } from "ink";
import { useEffect, useState } from "react";

/**
 * The animated "the assistant is busy" glyph. Replaces the static `◌` that the
 * Composer used to print beside the live stage label: a single-cell spinner that
 * cycles while a turn is in flight so the cockpit reads as *working*, not hung.
 *
 * INVARIANT — lives ONLY in the repainting live region. This component drives a
 * `setInterval` that mutates state ~12×/s; rendering it inside a committed
 * `<Static>` cell would freeze it on its first frame AND smear that frame into
 * native scrollback on every repaint (see ControlRoom). The Composer is the live
 * region, so that is its only caller — do not import it into a Static cell.
 *
 * Both frame sets are exactly ONE column wide so the `{glyph} {stage}` layout
 * never reflows between the Unicode and ASCII paths. The braille ramp matches the
 * project's existing hollow-ring `◌` aesthetic (cli-spinners "dots"); the ASCII
 * fallback is the classic `-\|/` quarter-turn for DAINTREE_ASCII / non-UTF locales.
 */

// Module-level constants: stable array identities so the effect's `frames.length`
// dep can't change spuriously per render, and no per-render allocation.
export const BRAILLE_FRAMES = [
  "⠋",
  "⠙",
  "⠹",
  "⠸",
  "⠼",
  "⠴",
  "⠦",
  "⠧",
  "⠇",
  "⠏",
] as const;
export const ASCII_FRAMES = ["-", "\\", "|", "/"] as const;

/** Frame cadence — fast enough to read as motion, slow enough to stay calm and
 *  cheap on a low-refresh inline cockpit (cli-spinners "dots" native rate). */
const INTERVAL_MS = 80;

export function ThinkingDot({ ascii = false }: { ascii?: boolean }) {
  const frames = ascii ? ASCII_FRAMES : BRAILLE_FRAMES;
  const [idx, setIdx] = useState(0);
  useEffect(() => {
    const id = setInterval(
      () => setIdx((i) => (i + 1) % frames.length),
      INTERVAL_MS,
    );
    return () => clearInterval(id);
    // `frames.length` (not the array ref) is the only thing the timer reads that
    // can change — flipping `ascii` resets the cycle cleanly; a stable length
    // never restarts the interval mid-turn.
  }, [frames.length]);
  return <Text>{frames[idx]}</Text>;
}
