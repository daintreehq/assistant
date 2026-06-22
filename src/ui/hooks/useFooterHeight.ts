/**
 * Size the split-footer footer to the live tree's measured height, every frame.
 *
 * In `split-footer` mode `footerHeight` is the number of rows OpenTUI RESERVES at the
 * bottom for `renderer.root`; it does NOT auto-track the rendered content. Too large
 * and there's dead space under the composer; too small and the footer clips/overlaps.
 *
 * We measure via OpenTUI's per-frame callback rather than a React layout effect: the
 * native renderer lays the tree out on its OWN frame loop, so a layout effect reads a
 * STALE height (the previous tree's) when a view changes — e.g. returning from the tall
 * operations view sized the footer to the short composer one frame too early, clipping
 * it. A frame callback re-reads the laid-out height every rendered frame, so any stale
 * read self-corrects on the next frame and `footerHeight` converges. The value is
 * memoised so we only hit the setter on a real change; on a SHRINK we also force one
 * full repaint, since OpenTUI doesn't always clear the rows a shrunk footer vacates.
 * Fully guarded — a sizing hiccup must never crash the cockpit.
 */
import { useEffect, useRef, type RefObject } from "react";
import type { BoxRenderable, CliRenderer } from "@opentui/core";

/**
 * Frames to keep the render loop alive after each React commit so the per-frame
 * measurement can CONVERGE. This is the load-bearing fix for the live cockpit: the
 * frame callback reads the tree's height ONE FRAME LATE (native layout lags the React
 * commit), and the renderer paints on-demand — so after a turn streams in, exactly one
 * frame renders (the callback reads the OLD, small height), then the renderer goes idle
 * and the footer NEVER grows to fit the new content. The result was the bug where a
 * running turn showed only its top few rows: no streamed response, no activity tree, and
 * the composer clipped off the bottom. Kicking a few extra frames per commit lets the
 * callback re-read the settled layout and grow the footer, then it stops (idle).
 */
const SETTLE_FRAMES = 3;

export function useFooterHeight(
  renderer: CliRenderer,
  rootRef: RefObject<BoxRenderable | null>,
  maxHeight: number,
): void {
  // Keep the live target in a ref so the long-lived frame callback always sees it.
  const maxRef = useRef(maxHeight);
  maxRef.current = maxHeight;
  const last = useRef(-1);
  // Frames remaining to keep the loop alive (reset on every React commit, below).
  const settle = useRef(0);

  // Runs after EVERY React commit (no deps): the tree may have changed (a streamed
  // token, a new tool row, a view switch), so re-arm the settle budget and kick a frame
  // — without this the on-demand renderer would stop after one frame and the lagged
  // measurement could never catch up to the new content.
  useEffect(() => {
    settle.current = SETTLE_FRAMES;
    try {
      renderer.requestRender();
    } catch {
      /* renderer torn down — nothing to schedule */
    }
  });

  useEffect(() => {
    // setFrameCallback wants a () => Promise<void>; do the work synchronously and hand
    // back a shared resolved promise so we don't allocate one every single frame.
    const done = Promise.resolve();
    const measure = (): Promise<void> => {
      const node = rootRef.current;
      if (!node) return done;
      const measured = Math.ceil(node.height ?? 0);
      if (Number.isFinite(measured) && measured > 0) {
        // Cap at the FULL TERMINAL height, NOT the caller's `maxHeight`: in split-footer
        // mode that value comes from `useTerminalDimensions()`, which returns the RENDER
        // height — i.e. the current footerHeight. Capping the footer at its own height is
        // the deadlock: once it shrinks to fit the idle composer it can never grow back
        // for a turn (the bug where a running turn showed only its top few rows). The
        // renderer knows the real terminal height; OpenTUI clamps footerHeight to it
        // anyway, so an over-tall measure is safe.
        const terminalRows =
          (renderer as unknown as { terminalHeight?: number }).terminalHeight ||
          maxRef.current;
        const next = Math.max(1, Math.min(Math.max(1, terminalRows), measured));
        const prev = last.current;
        if (next !== prev) {
          last.current = next;
          try {
            // Setting footerHeight makes OpenTUI scroll the viewport and force a FULL
            // split-footer repaint for the resize. That repaint is REQUIRED — it clears
            // the rows the resize vacates and repaints over the scrolled region; the old
            // code tried to SUPPRESS it on a grow to stop the streaming flash, but that
            // just left the scrolled region unpainted (the black-background/garbage
            // glitch). The real flash fix is upstream: the live turn renders in a
            // FIXED-height pane (see TurnCellView/liveMaxRows), so this height now only
            // changes at turn boundaries / view switches — rare, single, clean repaints
            // that should NOT be suppressed.
            renderer.footerHeight = next;
          } catch {
            /* sizing is best-effort; never break a render over it */
          }
        }
      }
      // While settling, keep requesting frames so a height that's still converging (the
      // one-frame layout lag) gets re-measured. Counts DOWN to zero so an idle cockpit
      // isn't pinned in a permanent render loop.
      if (settle.current > 0) {
        settle.current -= 1;
        try {
          renderer.requestRender();
        } catch {
          /* renderer torn down */
        }
      }
      return done;
    };
    renderer.setFrameCallback(measure);
    return () => {
      try {
        renderer.removeFrameCallback(measure);
      } catch {
        /* renderer already torn down */
      }
    };
  }, [renderer, rootRef]);
}
