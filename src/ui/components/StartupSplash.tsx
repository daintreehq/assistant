import { Box, Text } from "ink";
import { useEffect, useState } from "react";
import {
  SPLASH_FRAMES,
  SPLASH_HEIGHT,
  SPLASH_WIDTH,
} from "../splash/frames.js";

/**
 * The boot splash: the Daintree mark drawing itself in, centered on an otherwise
 * empty screen, played while the session connects/loads in the background and then
 * dissolved into the cockpit. The frames (scripts/gen-splash.py) reproduce the app's
 * own logo-reveal — trunk, then legs, then canopy arches — as an anti-aliased ASCII
 * coverage ramp; here we just step through them and tint each row with a top-to-base
 * green gradient so the canopy reads lit and the trunk grounded.
 *
 * It self-advances and holds on the final frame, calling `onComplete` once when the
 * draw finishes. It does NOT decide when to leave — the controller owns that (it
 * waits for startup to settle AND a minimum on-screen time), so a slow connect can't
 * cut the animation short and a fast one can't make it flash.
 */

// Canopy crown (lighter) at the top row, brand green at the base — interpolated per
// row to imply depth down the mark.
const TOP = [0x8f, 0xeb, 0xc4];
const BASE = [0x36, 0xce, 0x94];

function rowColor(row: number, rows: number): string {
  const t = rows <= 1 ? 0 : row / (rows - 1);
  const ch = TOP.map((a, i) => Math.round(a + (BASE[i] - a) * t));
  return "#" + ch.map((c) => c.toString(16).padStart(2, "0")).join("");
}

export function StartupSplash({
  columns,
  rows,
  fps = 28,
  lingerMs = 420,
  onComplete,
}: {
  /** Available width to center the mark within. */
  columns: number;
  /** Available height to center the mark within. */
  rows: number;
  /** Playback rate; 28fps over 20 frames is a ~0.7s draw. */
  fps?: number;
  /** How long to hold the finished logo before signalling completion, so it doesn't
   *  vanish the instant the draw lands. ~0.7s draw + ~0.42s hold ≈ 1.1s total. */
  lingerMs?: number;
  /** Fired once the draw has finished AND the linger has elapsed. */
  onComplete?: () => void;
}) {
  const [index, setIndex] = useState(0);
  const last = SPLASH_FRAMES.length - 1;

  useEffect(() => {
    // At the last frame, hold the completed logo for `lingerMs`, THEN signal done —
    // so the splash dissolves a beat after the draw lands, not the same instant.
    if (index >= last) {
      const id = setTimeout(() => onComplete?.(), lingerMs);
      return () => clearTimeout(id);
    }
    const id = setTimeout(() => setIndex((i) => Math.min(last, i + 1)), 1000 / fps);
    return () => clearTimeout(id);
    // onComplete is intentionally omitted: it fires exactly once after the linger,
    // and re-subscribing on a new callback identity would risk a double-fire.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [index, last, fps, lingerMs]);

  const frameRows = (SPLASH_FRAMES[index] ?? "").split("\n");

  return (
    <Box
      width={columns}
      height={rows}
      alignItems="center"
      justifyContent="center"
    >
      <Box flexDirection="column" width={SPLASH_WIDTH} height={SPLASH_HEIGHT}>
        {frameRows.map((line, i) => (
          <Text key={i} color={rowColor(i, SPLASH_HEIGHT)}>
            {line}
          </Text>
        ))}
      </Box>
    </Box>
  );
}
