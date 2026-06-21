/**
 * Commit finished transcript cells to the terminal's NATIVE scrollback (split-footer
 * mode) — the OpenTUI equivalent of Ink's `<Static>`. A *sealed* cell is rendered
 * once, its rows are written into scrollback ABOVE the live footer, and it then
 * scrolls up and away under the host terminal's own scrollbar. The live footer
 * (`renderer.root`) only ever holds the in-flight turn + status + composer, so the
 * React tree can never grow taller than the viewport — which is exactly what used to
 * overflow and garble under `main-screen`.
 *
 * Mechanism: `createScrollbackSurface()` gives an off-screen Renderable root plus a
 * `commitRows()` that copies measured rows into scrollback. We render the REAL cell
 * component into that root with React's `createPortal` (full fidelity — native
 * `<markdown>`, `<span>` runs, tone colors — zero drift from the live render), let it
 * `settle()` (markdown's tree-sitter highlight is async), then commit and tear the
 * surface down. Commits are ordered by the caller: it mounts ONE {@link ScrollbackCommit}
 * at a time (the head of its queue) and advances on `onCommitted`, so scrollback stays
 * in transcript order.
 *
 * Everything here is heavily guarded: a cosmetic scrollback write must never crash the
 * cockpit, and a finished turn must never be silently lost — if the fidelity path
 * throws we fall back to a plain-text scrollback write so the content still lands.
 */
import { useLayoutEffect, useRef, type ReactNode } from "react";
import {
  TextRenderable,
  type CliRenderer,
  type ScrollbackSurface,
} from "@opentui/core";
import { createPortal } from "@opentui/react";

export interface ScrollbackCommitProps {
  renderer: CliRenderer;
  /** The cell rendered at full fidelity into the off-screen scrollback surface. */
  node: ReactNode;
  /**
   * Plain-text fallback committed if the surface/portal path throws, so a finished
   * turn is never silently lost (degraded styling, but the content still scrolls).
   */
  fallbackText: string;
  /**
   * Fires exactly once, after the block has been committed (or has failed over to the
   * text fallback), so the owner can advance to the next queued cell.
   */
  onCommitted: () => void;
}

/**
 * Render ONE sealed cell into scrollback. Mounting this element portals `node` into a
 * fresh off-screen surface; the post-commit layout effect then settles + commits +
 * destroys the surface and calls `onCommitted`. The owner mounts these one at a time
 * (keyed by cell id) so scrollback stays strictly ordered.
 */
export function ScrollbackCommit({
  renderer,
  node,
  fallbackText,
  onCommitted,
}: ScrollbackCommitProps) {
  // One surface per mounted commit, created lazily so the portal has a container to
  // render into on the very first render (before the layout effect runs). A failed
  // create leaves the ref null and we fall straight to the text path in the effect.
  const surfaceRef = useRef<ScrollbackSurface | null>(null);
  if (!surfaceRef.current) {
    try {
      surfaceRef.current = renderer.createScrollbackSurface({
        startOnNewLine: true,
      });
    } catch {
      surfaceRef.current = null;
    }
  }

  useLayoutEffect(() => {
    // Cancelled if the component unmounts mid-flight — e.g. `/clear` replaces the
    // transcript, or the app tears down, while settle() is still pending. Once
    // cancelled we must NOT commit stale rows or call onCommitted (it would advance a
    // queue that has already been reset), and we destroy the surface in cleanup.
    let cancelled = false;
    let committedDone = false;
    const finish = () => {
      if (committedDone || cancelled) return;
      committedDone = true;
      onCommitted();
    };
    const surface = surfaceRef.current;
    // By the time this layout effect runs React has already committed the portal's
    // children into `surface.root` (children mount before parent effects), so the
    // surface has the real cell tree to lay out.
    void (async () => {
      try {
        if (!surface) throw new Error("scrollback surface unavailable");
        // settle() runs layout + render and waits for async markdown highlighting to
        // converge before we snapshot the rows.
        await surface.settle();
        if (cancelled) return;
        if (surface.height > 0) {
          surface.commitRows(0, surface.height, { trailingNewline: true });
        } else if (fallbackText) {
          // Settled to zero height but we DO have content (a measure miss): don't
          // advance silently — land the plain-text fallback so the turn isn't lost.
          commitFallbackText(renderer, fallbackText);
        }
      } catch {
        // Fidelity path failed — never drop the turn. Commit a plain-text snapshot so
        // the content still lands in scrollback (styling lost, content preserved).
        if (!cancelled) commitFallbackText(renderer, fallbackText);
      } finally {
        try {
          surface?.destroy();
        } catch {
          /* already torn down */
        }
        surfaceRef.current = null;
        finish();
      }
    })();
    // Cleanup cancels the in-flight commit and destroys the surface if the async path
    // hasn't already (idempotent: destroy() after destroy() is caught).
    return () => {
      cancelled = true;
      try {
        surfaceRef.current?.destroy();
      } catch {
        /* already torn down */
      }
      surfaceRef.current = null;
    };
  }, []);

  const surface = surfaceRef.current;
  return surface ? createPortal(node, surface.root, null) : null;
}

/** Last-resort scrollback write: one unstyled text block. Fully guarded; never throws. */
function commitFallbackText(renderer: CliRenderer, text: string): void {
  if (!text) return;
  try {
    renderer.writeToScrollback(({ renderContext, width }) => ({
      // Pin the snapshot width to the terminal width so the text wraps instead of being
      // measured at a single column (a bare TextRenderable has no intrinsic width).
      root: new TextRenderable(renderContext, { content: text, width }),
      width,
      startOnNewLine: true,
      trailingNewline: true,
    }));
  } catch {
    /* nothing more we can safely do */
  }
}
