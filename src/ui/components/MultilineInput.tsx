/**
 * A multi-line text area for the composer. `ink-text-input` is single-line, so
 * this is hand-rolled to behave like a real editor pane:
 *
 *   • Enter submits. A newline is inserted by either a modifier+Enter
 *     (Shift/Option/Ctrl+Enter, wherever the terminal reports the modifier —
 *     e.g. via the kitty keyboard protocol the app enables) OR, as a
 *     terminal-independent fallback that always works, a trailing backslash
 *     before Enter (`…\` + Enter), the same convention Claude Code uses.
 *   • Escape clears the buffer (cancel).
 *   • ← → move within a line; ↑ ↓ move between lines, keeping the column.
 *   • Backspace/Delete remove the char before the cursor.
 *   • Pasted text (which arrives as one chunk, possibly with newlines) is
 *     inserted verbatim, so a multi-line paste lands as multiple lines.
 *
 * Rendering is a hanging indent: the prompt (`› `) sits in a fixed left gutter
 * and the text column lives to its right, so every wrapped or explicit line
 * lines up under the first character — the chevron never collides with text.
 *
 * The buffer (`value`) is owned by the parent; the cursor offset is local. Ctrl
 * and Meta chords are ignored here so the app-level handlers (^C/^O/^X) still
 * fire.
 */
import { useEffect, useState } from "react";
import { Box, Text, useInput } from "ink";
import { ui } from "../theme.js";

export interface MultilineInputProps {
  value: string;
  onChange: (value: string) => void;
  /** Enter (without Shift). Receives the current value. */
  onSubmit: (value: string) => void;
  /** Escape. */
  onCancel: () => void;
  focus?: boolean;
  placeholder?: string;
  /** The left gutter shown on the first row (e.g. "› "); also sets indent width. */
  prompt: string;
  /** Tab pressed (used by the composer for slash-command completion). */
  onTab?: () => void;
}

/** Locate the cursor's line index and column from a flat offset. */
function locate(value: string, offset: number): { row: number; col: number } {
  const clamped = Math.max(0, Math.min(offset, value.length));
  const before = value.slice(0, clamped);
  const row = before.split("\n").length - 1;
  const col = clamped - (before.lastIndexOf("\n") + 1);
  return { row, col };
}

/** Flat offset of a given line/column (column clamped to the line length). */
function offsetOf(lines: string[], row: number, col: number): number {
  const r = Math.max(0, Math.min(row, lines.length - 1));
  let off = 0;
  for (let i = 0; i < r; i++) off += lines[i].length + 1; // +1 for each "\n"
  return off + Math.min(col, lines[r].length);
}

export function MultilineInput({
  value,
  onChange,
  onSubmit,
  onCancel,
  focus = true,
  placeholder = "",
  prompt,
  onTab,
}: MultilineInputProps) {
  const [cursor, setCursor] = useState(value.length);
  // Keep the cursor valid when the parent mutates the value (e.g. clears on
  // submit, or completes a slash command).
  useEffect(() => {
    setCursor((c) => Math.min(c, value.length));
  }, [value]);
  const cur = Math.min(cursor, value.length);

  function commit(next: string, nextCursor: number) {
    onChange(next);
    setCursor(Math.max(0, Math.min(next.length, nextCursor)));
  }
  function insert(text: string) {
    commit(value.slice(0, cur) + text + value.slice(cur), cur + text.length);
  }

  useInput(
    (input, key) => {
      // Enter is handled FIRST, before the ctrl/meta passthrough below, so the
      // modifier+Enter newline combos are caught rather than swallowed.
      if (key.return) {
        // A modifier+Enter is an explicit newline wherever the terminal reports
        // the modifier (kitty Shift+Enter, Option/Alt+Enter, Ctrl+Enter).
        if (key.shift || key.meta || key.ctrl) {
          insert("\n");
          return;
        }
        // Terminal-independent fallback (same as Claude Code): a backslash right
        // before the cursor turns Enter into a newline instead of a submit. This
        // works even when the terminal can't distinguish Shift/Option+Enter from
        // a plain Enter — which is the common case in embedded terminals.
        if (cur > 0 && value[cur - 1] === "\\") {
          commit(value.slice(0, cur - 1) + "\n" + value.slice(cur), cur);
          return;
        }
        onSubmit(value);
        return;
      }
      // Let app-level chords (^C/^O/^X, Alt+…) through untouched.
      if (key.ctrl || key.meta) return;
      if (key.tab) {
        onTab?.();
        return;
      }
      if (key.escape) {
        onCancel();
        return;
      }
      if (key.leftArrow) {
        setCursor(Math.max(0, cur - 1));
        return;
      }
      if (key.rightArrow) {
        setCursor(Math.min(value.length, cur + 1));
        return;
      }
      if (key.upArrow || key.downArrow) {
        const lines = value.split("\n");
        const { row, col } = locate(value, cur);
        const targetRow = row + (key.upArrow ? -1 : 1);
        if (targetRow < 0) setCursor(0);
        else if (targetRow > lines.length - 1) setCursor(value.length);
        else setCursor(offsetOf(lines, targetRow, col));
        return;
      }
      if (key.backspace || key.delete) {
        if (cur > 0) commit(value.slice(0, cur - 1) + value.slice(cur), cur - 1);
        return;
      }
      // Printable input, including a multi-line paste chunk. Normalise CR/CRLF.
      if (input) insert(input.replace(/\r\n?/g, "\n"));
    },
    { isActive: focus },
  );

  const lines = value.length > 0 ? value.split("\n") : [""];
  const { row: curRow, col: curCol } = locate(value, cur);
  const showCursor = focus;

  // The prompt sits in a fixed-width gutter so wrapped/continuation lines align
  // under the first character rather than under the chevron.
  return (
    <Box>
      <Text color={ui.color.accent}>{prompt}</Text>
      <Box flexGrow={1} flexDirection="column">
        {value.length === 0 ? (
          <Text>
            {showCursor ? (
              placeholder.length > 0 ? (
                <>
                  <Text inverse>{placeholder[0]}</Text>
                  <Text dimColor>{placeholder.slice(1)}</Text>
                </>
              ) : (
                <Text inverse> </Text>
              )
            ) : (
              <Text dimColor>{placeholder}</Text>
            )}
          </Text>
        ) : (
          lines.map((line, i) => {
            if (!showCursor || i !== curRow) {
              return <Text key={i}>{line.length > 0 ? line : " "}</Text>;
            }
            const before = line.slice(0, curCol);
            const at = line[curCol] ?? " ";
            const after = line.slice(curCol + 1);
            return (
              <Text key={i}>
                {before}
                <Text inverse>{at}</Text>
                {after}
              </Text>
            );
          })
        )}
      </Box>
    </Box>
  );
}
