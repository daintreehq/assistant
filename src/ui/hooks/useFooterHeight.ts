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

export function useFooterHeight(
  renderer: CliRenderer,
  rootRef: RefObject<BoxRenderable | null>,
  maxHeight: number,
): void {
  // Keep the live target in a ref so the long-lived frame callback always sees it.
  const maxRef = useRef(maxHeight);
  maxRef.current = maxHeight;
  const last = useRef(-1);

  useEffect(() => {
    // setFrameCallback wants a () => Promise<void>; do the work synchronously and hand
    // back a shared resolved promise so we don't allocate one every single frame.
    const done = Promise.resolve();
    const measure = (): Promise<void> => {
      const node = rootRef.current;
      if (!node) return done;
      const measured = Math.ceil(node.height ?? 0);
      if (!Number.isFinite(measured) || measured <= 0) return done;
      const next = Math.max(1, Math.min(Math.max(1, maxRef.current), measured));
      const prev = last.current;
      if (next === prev) return done;
      last.current = next;
      try {
        renderer.footerHeight = next;
        // Shrinking vacates rows at the footer's top; OpenTUI doesn't always clear them
        // on a split-footer shrink (it does on grow), so old chrome can linger and
        // overlap. Force one full repaint to wipe the vacated rows.
        if (prev > 0 && next < prev) {
          (
            renderer as unknown as { forceFullRepaintRequested: boolean }
          ).forceFullRepaintRequested = true;
        }
      } catch {
        /* sizing is best-effort; never break a render over it */
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
