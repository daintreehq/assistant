import { useCallback, useEffect, useRef, useState } from "react";
import {
  SPLASH_FRAMES,
  SPLASH_HEIGHT,
  SPLASH_WIDTH,
} from "../splash/frames.js";

/**
 * The boot splash: the Daintree mark drawing itself in, played while the session
 * connects/loads in the background and then dissolved into the cockpit. The frames
 * (scripts/gen-splash.py) reproduce the app's own logo-reveal — trunk, then legs,
 * then canopy arches — as an anti-aliased ASCII coverage ramp; here we just step
 * through them and tint each row with a top-to-base green gradient so the canopy
 * reads lit and the trunk grounded.
 *
 * INLINE SIZING. The cockpit renders into the terminal's MAIN buffer (see
 * ControlRoom), so the splash does NOT fill and vertically-center the screen — a
 * screen-high frame in the main buffer just pushes scrollback around and risks
 * leaving artifacts when it dissolves. Instead it draws at its NATURAL height,
 * after a couple of blank lines for breathing room, and the native renderer cleanly
 * erases those rows when the cockpit takes over. It is still HORIZONTALLY centered
 * across the terminal — within `columns - 1`, so the mark's right edge can never
 * reach the autowrap column and ghost an animation frame (see ControlRoom for that
 * hazard).
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
  /** Available width; only used to skip the mark on a terminal too narrow to hold it. */
  columns: number;
  /** Accepted for call-site compat; the inline splash no longer centers vertically. */
  rows?: number;
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

  // Fire `onComplete` at most once, reading its latest identity through a ref so the
  // timer effect needn't depend on it: a changed callback during the linger still
  // fires the current one, and a callback supplied late still fires. The ref is kept
  // current in an effect (concurrent-safe — no ref writes during render).
  const onCompleteRef = useRef(onComplete);
  useEffect(() => {
    onCompleteRef.current = onComplete;
  });
  const fired = useRef(false);
  const fireOnce = useCallback(() => {
    if (fired.current) return;
    fired.current = true;
    onCompleteRef.current?.();
  }, []);

  // The mark is a fixed SPLASH_WIDTH-wide block. On a terminal too narrow to hold it
  // (with a column of margin so the mark never sits in the autowrap column and ghosts
  // each animation frame) a clipped logo just looks broken, so skip the animation
  // entirely and let boot proceed at once (fireOnce satisfies the draw-done gate). We
  // no longer gate on rows — the splash draws at its natural height, not the screen's.
  const tooSmall = columns <= SPLASH_WIDTH;

  useEffect(() => {
    if (tooSmall) {
      fireOnce();
      return;
    }
    // At the last frame, hold the completed logo for `lingerMs`, THEN signal done —
    // so the splash dissolves a beat after the draw lands, not the same instant.
    if (index >= last) {
      const id = setTimeout(fireOnce, lingerMs);
      return () => clearTimeout(id);
    }
    const id = setTimeout(() => setIndex((i) => Math.min(last, i + 1)), 1000 / fps);
    return () => clearTimeout(id);
  }, [tooSmall, index, last, fps, lingerMs, fireOnce]);

  // Nothing to draw on a too-narrow terminal — boot proceeds immediately.
  if (tooSmall) return null;

  const frameRows = (SPLASH_FRAMES[index] ?? "").split("\n");

  // Natural height, a couple of blank lines down for breathing room, horizontally
  // centered. The centering track is `columns - 1` so the mark stays one column shy
  // of the terminal edge (no autowrap ghosting); `tooSmall` guarantees there's room.
  return (
    // flexDirection="row" is explicit here: Ink's `<Box>` defaulted to a row, but
    // OpenTUI's `<box>` defaults to a column — and `justifyContent="center"` centers
    // along the MAIN axis, so without `row` it would center vertically instead of
    // pushing the mark to the horizontal middle of the `columns - 1` track.
    <box
      flexDirection="row"
      width={Math.max(SPLASH_WIDTH, columns - 1)}
      justifyContent="center"
      marginTop={2}
    >
      <box flexDirection="column" width={SPLASH_WIDTH}>
        {frameRows.map((line, i) => (
          <text key={i} fg={rowColor(i, SPLASH_HEIGHT)}>
            {line}
          </text>
        ))}
      </box>
    </box>
  );
}
