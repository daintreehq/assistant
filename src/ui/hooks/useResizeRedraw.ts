/**
 * Debounced "nuclear redraw" on terminal resize for the split-footer cockpit.
 *
 * Why this exists: in `split-footer` mode finished content is committed to the host
 * terminal's NATIVE scrollback (immutable, owned by the host) and only the small live
 * footer is repainted. When the terminal is resized, OpenTUI repositions the footer but
 * does NOT reliably clear the rows the footer vacated (it only force-clears on a width
 * SHRINK — see `processResize` in @opentui/core). Every resize that moves the footer
 * down therefore freezes a stale copy of the footer into scrollback, and those
 * duplicates accumulate with each resize while the committed masthead scrolls up and off
 * the top. The native reflow also re-wraps already-committed lines at the new width,
 * shearing the layout.
 *
 * The fix is the same "destructive resize replay" the better inline CLIs use: once the
 * resize SETTLES (debounced — a window drag fires a storm of SIGWINCH events), wipe the
 * host screen + scrollback, reset OpenTUI's split-footer replay record, force a full
 * repaint, and re-commit the masthead + the whole transcript fresh at the new width.
 * That is exactly the machinery `/clear` already drives, so the caller hands us a single
 * `onRedraw` that triggers it (see `useDaintreeController.requestRedraw`).
 *
 * This hook owns ONLY the *when*: it watches the measured terminal dimensions and fires
 * `onRedraw` once, `delayMs` after the last change. It also fires one redraw the first
 * time it becomes `enabled` (the boot splash → cockpit hand-off is itself a large footer
 * resize that leaves splash residue), so the cockpit starts from a clean, masthead-on-top
 * scrollback. It never fires while disabled (during the splash) and never on the initial
 * mount before a real change.
 */
import { useEffect, useRef } from "react";

/**
 * Debounce window after the last resize before the redraw fires. A window drag emits a
 * burst of resize events; we coalesce them and only redraw once the user stops, so the
 * expensive full-transcript replay runs once per resize gesture, not once per frame.
 * 150ms is the high end of the typical inline-CLI range (Claude Code uses ~50ms) — long
 * enough to swallow a drag, short enough to feel immediate once you let go.
 */
export const RESIZE_REDRAW_DELAY_MS = 150;

export function useResizeRedraw(params: {
  /** False while the boot splash owns the screen — no redraw is scheduled then. */
  enabled: boolean;
  /** Current terminal width (columns) from `useTerminalDimensions`. */
  columns: number;
  /** Current terminal height (rows) from `useTerminalDimensions`. */
  rows: number;
  /** Triggers the nuclear redraw (host wipe + split-footer reset + full re-commit). */
  onRedraw: () => void;
  /** Debounce window; defaults to {@link RESIZE_REDRAW_DELAY_MS}. */
  delayMs?: number;
}): void {
  const { enabled, columns, rows, onRedraw, delayMs = RESIZE_REDRAW_DELAY_MS } =
    params;

  // Keep the latest callback in a ref so the debounce timer always invokes the current
  // closure without re-arming the effect (which would reset the debounce on every render).
  const onRedrawRef = useRef(onRedraw);
  onRedrawRef.current = onRedraw;

  // The last size we acted on. `null` means "not yet enabled" — the next enabled run
  // establishes the baseline AND fires the one-time boot-handoff redraw. Cleared back to
  // null whenever we're disabled so a later re-enable re-seeds cleanly.
  const baseline = useRef<{ columns: number; rows: number } | null>(null);

  useEffect(() => {
    if (!enabled) {
      baseline.current = null;
      return;
    }
    // First enabled render (boot just finished): seed the baseline and schedule one
    // redraw to clear any splash residue and commit the masthead on a clean scrollback.
    // Subsequent renders only schedule a redraw when the size actually changed.
    if (
      baseline.current !== null &&
      baseline.current.columns === columns &&
      baseline.current.rows === rows
    ) {
      return;
    }
    baseline.current = { columns, rows };
    // Debounce: this timer is cleared by the cleanup below if another size change (or a
    // re-render) arrives first, so the redraw only fires `delayMs` after the LAST change.
    const id = setTimeout(() => onRedrawRef.current(), delayMs);
    return () => clearTimeout(id);
  }, [enabled, columns, rows, delayMs]);
}
