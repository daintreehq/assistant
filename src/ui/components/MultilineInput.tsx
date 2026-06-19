/**
 * A multi-line text area for the composer. `ink-text-input` is single-line, so
 * this is hand-rolled to behave like a real editor pane with the readline/
 * native-text-field conventions a CLI user already knows:
 *
 *   • Enter submits. A newline is inserted by either a modifier+Enter
 *     (Shift/Option/Ctrl+Enter, wherever the terminal reports the modifier —
 *     e.g. via the kitty keyboard protocol the app enables) OR, as a
 *     terminal-independent fallback that always works, a trailing backslash
 *     before Enter (`…\` + Enter), the same convention Claude Code uses.
 *   • Escape clears the buffer (cancel).
 *   • ← → move one char; ↑ ↓ move between lines, keeping the column. At the
 *     top/bottom line they step through the session's prompt history (when a
 *     `history` list is supplied), restoring the in-progress draft on the way
 *     back down — the standard shell recall.
 *   • Home/End and ^A/^E jump to the start/end of the current logical line.
 *   • Ctrl/Alt + ← → and Alt+B / Alt+F move by word (whitespace-delimited).
 *   • Backspace deletes left, Delete / ^D delete right.
 *   • Ctrl+W / Alt+Backspace (Option+Backspace) kill the previous word, Alt+D
 *     the next word; ^K kills to end of line, ^U (or Cmd+Backspace) kills the
 *     whole line; ^Y yanks the last kill.
 *   • Pasted text (which arrives as one chunk, possibly with newlines) is
 *     inserted verbatim, so a multi-line paste lands as multiple lines.
 *
 * Word boundaries are whitespace-delimited (a "word" is a maximal run of
 * non-whitespace) — predictable for prose, paths, and identifiers alike.
 *
 * Rendering is a hanging indent: the prompt (`› `) sits in a fixed left gutter
 * and the text column lives to its right, so every wrapped or explicit line
 * lines up under the first character — the chevron never collides with text.
 *
 * The buffer (`value`) is owned by the parent; the cursor offset, kill ring,
 * and history cursor are local. App-level chords (^C/^O/^X) are never inserted
 * as text, so those handlers still fire.
 */
import { useEffect, useRef, useState } from "react";
import { Box, Text, useInput } from "ink";
import { ui } from "../theme.js";

export interface MultilineInputProps {
  value: string;
  onChange: (value: string) => void;
  /** Enter (without a newline modifier/escape). Receives the current value. */
  onSubmit: (value: string) => void;
  /** Escape. */
  onCancel: () => void;
  focus?: boolean;
  placeholder?: string;
  /** The left gutter shown on the first row (e.g. "› "); also sets indent width. */
  prompt: string;
  /** Tab pressed (used by the composer for slash-command completion). */
  onTab?: () => void;
  /**
   * Previously submitted prompts, oldest first. ↑/↓ at the top/bottom line walk
   * this list (shell-style recall). Omit for inputs without a history.
   */
  history?: string[];
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

/** Start of the logical line containing `offset` (0 if it's the first line). */
function lineStartOf(value: string, offset: number): number {
  return value.lastIndexOf("\n", offset - 1) + 1;
}
/** End of the logical line containing `offset` (the next "\n", or the end). */
function lineEndOf(value: string, offset: number): number {
  const nl = value.indexOf("\n", offset);
  return nl === -1 ? value.length : nl;
}

const isSpace = (ch: string) => /\s/.test(ch);
/** Offset one word to the left: skip trailing space, then the word itself. */
function prevWord(value: string, offset: number): number {
  let i = offset;
  while (i > 0 && isSpace(value[i - 1])) i--;
  while (i > 0 && !isSpace(value[i - 1])) i--;
  return i;
}
/** Offset one word to the right: skip leading space, then the word itself. */
function nextWord(value: string, offset: number): number {
  let i = offset;
  while (i < value.length && isSpace(value[i])) i++;
  while (i < value.length && !isSpace(value[i])) i++;
  return i;
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
  history = [],
}: MultilineInputProps) {
  const [cursor, setCursor] = useState(value.length);
  // The last killed text (^K/^U/^W/Alt+Backspace/Alt+D), for ^Y to yank.
  const killRing = useRef("");
  // History cursor: null = editing the live draft; otherwise an index into
  // `history`. The draft is stashed so ↓ past the newest entry restores it.
  const [histIndex, setHistIndex] = useState<number | null>(null);
  const draft = useRef("");
  // The last value WE pushed (commit/recall). Lets the effect below tell an
  // EXTERNAL replacement (the parent completing a slash command, or clearing on
  // submit/cancel) from our own edits, which it otherwise can't see.
  const lastPushed = useRef(value);

  // When the parent replaces the buffer out from under us, the local cursor is
  // stale: park it at the end (the natural caret spot after a completion or a
  // recalled draft) and, if it was cleared, leave history navigation. Our own
  // edits already set the cursor, so for those we only clamp it.
  useEffect(() => {
    if (value === lastPushed.current) {
      setCursor((c) => Math.min(c, value.length));
      return;
    }
    lastPushed.current = value;
    setCursor(value.length);
    if (value === "") setHistIndex(null);
  }, [value]);
  const cur = Math.min(cursor, value.length);

  function commit(next: string, nextCursor: number) {
    lastPushed.current = next;
    onChange(next);
    setCursor(Math.max(0, Math.min(next.length, nextCursor)));
  }
  function insert(text: string) {
    commit(value.slice(0, cur) + text + value.slice(cur), cur + text.length);
  }
  /** Replace the whole buffer (history recall) and park the cursor at the end. */
  function recall(text: string) {
    lastPushed.current = text;
    onChange(text);
    setCursor(text.length);
  }

  function killRange(from: number, to: number) {
    const [a, b] = from <= to ? [from, to] : [to, from];
    if (a === b) return;
    killRing.current = value.slice(a, b);
    commit(value.slice(0, a) + value.slice(b), a);
  }
  /** Delete the entire logical line the cursor sits on (^U / Cmd+Backspace). */
  function killLine() {
    killRange(lineStartOf(value, cur), lineEndOf(value, cur));
  }

  function moveUp() {
    const lines = value.split("\n");
    const { row, col } = locate(value, cur);
    if (row > 0) {
      setCursor(offsetOf(lines, row - 1, col));
      return;
    }
    // On the top line: walk backward through prompt history.
    if (history.length === 0) {
      setCursor(0);
      return;
    }
    if (histIndex === null) draft.current = value;
    const idx = histIndex === null ? history.length - 1 : Math.max(0, histIndex - 1);
    setHistIndex(idx);
    recall(history[idx]);
  }

  function moveDown() {
    const lines = value.split("\n");
    const { row, col } = locate(value, cur);
    if (row < lines.length - 1) {
      setCursor(offsetOf(lines, row + 1, col));
      return;
    }
    // On the bottom line: walk forward through history, then back to the draft.
    if (histIndex === null) {
      setCursor(value.length);
      return;
    }
    if (histIndex >= history.length - 1) {
      setHistIndex(null);
      recall(draft.current);
      return;
    }
    const idx = histIndex + 1;
    setHistIndex(idx);
    recall(history[idx]);
  }

  useInput(
    (input, key) => {
      // Enter is handled FIRST, before the chord handlers below, so the
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
        // a plain Enter — the common case in embedded terminals.
        if (cur > 0 && value[cur - 1] === "\\") {
          commit(value.slice(0, cur - 1) + "\n" + value.slice(cur), cur);
          return;
        }
        onSubmit(value);
        return;
      }

      // ---- Cursor motion (handled before the ctrl/meta passthrough so the
      //      modified arrows and Alt/^ editing chords aren't swallowed) ----
      if (key.leftArrow) {
        setCursor(key.ctrl || key.meta ? prevWord(value, cur) : Math.max(0, cur - 1));
        return;
      }
      if (key.rightArrow) {
        setCursor(
          key.ctrl || key.meta ? nextWord(value, cur) : Math.min(value.length, cur + 1),
        );
        return;
      }
      if (key.upArrow) {
        moveUp();
        return;
      }
      if (key.downArrow) {
        moveDown();
        return;
      }
      if (key.home) {
        setCursor(lineStartOf(value, cur));
        return;
      }
      if (key.end) {
        setCursor(lineEndOf(value, cur));
        return;
      }
      // Forward delete: the Delete key (distinct from Backspace in Ink).
      if (key.delete) {
        if (cur < value.length) commit(value.slice(0, cur) + value.slice(cur + 1), cur);
        return;
      }
      // Backspace, and its widening variants:
      //   Cmd+Backspace  (super) → delete to start of line, the macOS native
      //                            "delete the line" (^U is the portable twin).
      //   Option+Backspace (meta) → delete the previous word.
      //   Backspace               → delete the previous character.
      // Cmd is only seen when the terminal reports it (kitty protocol); ^U
      // below is the reliable whole-line delete everywhere else.
      if (key.backspace) {
        if (key.super) killLine();
        else if (key.meta) killRange(prevWord(value, cur), cur);
        else if (cur > 0) commit(value.slice(0, cur - 1) + value.slice(cur), cur - 1);
        return;
      }

      // ---- Alt/Meta editing chords (word ops). Other meta chords are ignored
      //      rather than inserted as text. ----
      if (key.meta && !key.ctrl) {
        const ch = input.toLowerCase();
        if (ch === "b") setCursor(prevWord(value, cur));
        else if (ch === "f") setCursor(nextWord(value, cur));
        else if (ch === "d") killRange(cur, nextWord(value, cur));
        return;
      }

      // ---- Ctrl editing chords. ^C/^O/^X (and any others) fall through to the
      //      app-level handlers and are never inserted as text. ----
      if (key.ctrl) {
        switch (input) {
          case "a":
            setCursor(lineStartOf(value, cur));
            return;
          case "e":
            setCursor(lineEndOf(value, cur));
            return;
          case "k": {
            const end = lineEndOf(value, cur);
            // At end-of-line, ^K eats the newline so it joins the next line.
            killRange(cur, end === cur && cur < value.length ? cur + 1 : end);
            return;
          }
          case "u":
            killLine();
            return;
          case "w":
            killRange(prevWord(value, cur), cur);
            return;
          case "d":
            if (cur < value.length) commit(value.slice(0, cur) + value.slice(cur + 1), cur);
            return;
          case "y":
            if (killRing.current) insert(killRing.current);
            return;
          default:
            return; // ^C/^O/^X et al. — leave for the app, do not insert.
        }
      }

      if (key.tab) {
        onTab?.();
        return;
      }
      if (key.escape) {
        onCancel();
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
