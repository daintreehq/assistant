/**
 * Split the controller's run-oriented transcript into two regions:
 *
 *   - **committed** — the masthead plus every *sealed* cell (a finished turn, a
 *     standalone note, a command result). These are pushed ONCE into the terminal's
 *     native scrollback via {@link ScrollbackCommit} and then owned entirely by the
 *     host terminal — they scroll up and away under its scrollbar and never re-render.
 *   - **live** — whatever is still mutating: the in-flight turn (+ any sealed cell
 *     still draining to scrollback this frame). This is all that ControlRoom renders
 *     into the split-footer footer, so the React tree can never outgrow the viewport.
 *
 * This restores the Ink `<Static>` model the controller was already written for (it
 * keeps sealed cells immutable on purpose — see useDaintreeController) but which the
 * OpenTUI `main-screen` port had silently dropped by rendering the whole tree live.
 *
 * Ordering: scrollback is append-only, so we commit STRICTLY in transcript order —
 * the header first, then one cell at a time from the front. We mount a single
 * {@link ScrollbackCommit} (the head of the queue) and only advance once it reports
 * `onCommitted`, so two commits can never race and interleave their rows.
 */
import { useEffect, useRef, useState, type ReactNode } from "react";
import type { CliRenderer } from "@opentui/core";
import type { TranscriptCell } from "../types.js";
import { CellView } from "../components/Transcript.js";
import { ScrollbackCommit } from "../scrollback.js";

/** A cell can be committed once it will never change again. */
function isSealed(cell: TranscriptCell): boolean {
  // Turns seal when they leave the active state; standalone notes/command results are
  // immutable the moment they arrive.
  return cell.kind === "turn" ? cell.state !== "active" : true;
}

/** Plain-text used only if the fidelity (surface) commit throws — never lose content. */
function cellFallbackText(cell: TranscriptCell): string {
  if (cell.kind === "note") return cell.text;
  if (cell.kind === "command")
    return [cell.title, cell.text].filter(Boolean).join("\n");
  const parts: string[] = [];
  if (cell.userText) parts.push(cell.userText);
  if (cell.assistantText) parts.push(cell.assistantText);
  for (const n of cell.notes) parts.push(n.text);
  return parts.join("\n\n");
}

export interface ScrollbackHeader {
  /** The masthead rendered at full fidelity into scrollback (scrolls away on top). */
  node: ReactNode;
  /** Plain-text fallback if the surface commit throws. */
  fallbackText: string;
}

export interface ScrollbackTranscript {
  /** Cells still live in the footer (active turn + any sealed cell mid-commit). */
  liveCells: TranscriptCell[];
  /** The single in-flight scrollback commit to mount in the tree (or null). */
  commitSlot: ReactNode;
}

export function useScrollbackTranscript(params: {
  renderer: CliRenderer;
  transcript: TranscriptCell[];
  /** Masthead to seed scrollback with; null skips it (gallery/tests). */
  header: ScrollbackHeader | null;
  width: number;
  now?: number;
  expanded?: boolean;
  /**
   * Bumped whenever the transcript is REPLACED rather than appended (the controller's
   * `/clear` nonce). Length alone can't detect a clear — `/clear` drops a fresh
   * confirmation card, so the new length can equal the old committed count and a
   * length check would treat the new card as already committed (lost) and skip
   * re-committing the masthead. A monotonic key makes the reset deterministic.
   */
  resetKey?: number;
}): ScrollbackTranscript {
  const { renderer, transcript, header, width, now, expanded, resetKey } =
    params;
  // Whether the masthead has been committed yet. Starts false even when no header is
  // configured (gallery/tests) — the cell gate below only WAITS on it when a header is
  // actually required, so it stays harmless there and the header can still commit the
  // moment one first appears (it's null during the boot splash, set once booted).
  const [headerDone, setHeaderDone] = useState(false);
  // How many cells (from the front) have been committed to scrollback.
  const [committed, setCommitted] = useState(0);
  const headerRequired = header != null;
  const prevResetKey = useRef(resetKey);

  // `/clear` REPLACES the transcript (and the controller wipes the host scrollback).
  // Reset the commit cursor and re-arm the masthead so the fresh scrollback gets the
  // header back on top and the new cells commit from scratch. Driven by the explicit
  // `resetKey` (not length) so a clear whose new length matches the old committed
  // count is still detected. The `committed > length` clamp below is a belt-and-braces
  // guard for any other shrink (it never re-arms the header on its own).
  useEffect(() => {
    if (resetKey !== prevResetKey.current) {
      prevResetKey.current = resetKey;
      setCommitted(0);
      if (headerRequired) setHeaderDone(false);
    }
  }, [resetKey, headerRequired]);

  useEffect(() => {
    if (committed > transcript.length) setCommitted(transcript.length);
  }, [transcript.length, committed]);

  let commitSlot: ReactNode = null;
  if (headerRequired && !headerDone) {
    commitSlot = (
      <ScrollbackCommit
        key="__header__"
        renderer={renderer}
        node={header.node}
        fallbackText={header.fallbackText}
        onCommitted={() => setHeaderDone(true)}
      />
    );
  } else if ((!headerRequired || headerDone) && committed < transcript.length) {
    const cell = transcript[committed];
    if (cell && isSealed(cell)) {
      commitSlot = (
        <ScrollbackCommit
          key={cell.id}
          renderer={renderer}
          node={
            <CellView cell={cell} width={width} now={now} expanded={expanded} />
          }
          fallbackText={cellFallbackText(cell)}
          onCommitted={() => setCommitted((c) => c + 1)}
        />
      );
    }
  }

  // Everything not yet committed stays live in the footer — the active turn plus any
  // sealed cell still draining to scrollback (it leaves the footer the frame its
  // commit lands). Slicing keeps committed history out of the live tree entirely.
  const liveCells = committed > 0 ? transcript.slice(committed) : transcript;

  return { liveCells, commitSlot };
}
