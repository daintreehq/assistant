import { useImperativeHandle, useState, type Ref } from "react";
import { Box, Text } from "ink";
import { Divider, KeyHint } from "../primitives.js";
import { glyphs, ui, unicodeOk } from "../theme.js";
import { MultilineInput } from "./MultilineInput.js";
import { ThinkingDot } from "./ThinkingDot.js";
import { paletteEntries } from "../../commandRegistry.js";
import { LIVE_CHROME_MAX_WIDTH } from "../liveChrome.js";

/**
 * Slash commands surfaced as a filterable palette — described by intent. Derived
 * from the shared command registry so the palette can't drift from the commands
 * the handlers actually accept (issue #50).
 */
export const COMMAND_SUGGESTIONS: Array<[string, string]> = paletteEntries();

/** Cap on the recallable prompt history kept for ↑/↓ during a session. */
const HISTORY_LIMIT = 200;

/**
 * Imperative handle the controller uses to push text back into the composer when
 * a just-sent message is pulled back with Escape (issue #61). The buffer stays
 * owned by the composer (lifting it would re-render the whole tree on every
 * keystroke), so the one out-of-band write is exposed as a method instead.
 */
export interface ComposerHandle {
  /** Replace the composer buffer (cursor parks at end via MultilineInput). */
  restore(text: string): void;
}

function suggestionsFor(value: string): Array<[string, string]> {
  if (!value.startsWith("/")) return [];
  const q = value.slice(1).toLowerCase();
  return COMMAND_SUGGESTIONS.filter(
    ([cmd, desc]) =>
      cmd.slice(1).startsWith(q) || desc.toLowerCase().includes(q),
  ).slice(0, 5);
}

/**
 * The composer. Two visual layers: the input line (a single `›` — Daintree is
 * already named in the header) and a context line whose right side reflects the
 * session, not a fixed shortcut string. Typing `/` opens a command palette;
 * Tab completes the top match. While busy, the actual stage is shown, not a
 * generic "thinking".
 */
export function Composer({
  busy,
  focus = true,
  stage = "Thinking",
  queueDepth = 0,
  contextHint,
  cancellable,
  onSubmit,
  onCancel,
  ref,
}: {
  busy: boolean;
  /** When false the input ignores keystrokes (e.g. during a turn or a modal). */
  focus?: boolean;
  /** Live stage label shown while busy (Inspecting, Delegating, Watching…). */
  stage?: string;
  /** User follow-ups queued behind the in-flight turn. When >0 the busy
   *  indicator appends "· N queued" so silently-queued input is visible (#95). */
  queueDepth?: number;
  /** Right-aligned context summary on the second line. */
  contextHint?: string;
  /** Whether the in-flight turn can be aborted; gates the "Esc cancel" hint.
   *  Defaults to `busy` so callers that don't distinguish turn kinds still show it. */
  cancellable?: boolean;
  onSubmit: (value: string) => boolean | void | Promise<void>;
  /** Abort the in-flight turn — invoked on Escape when the composer is empty and
   *  busy. With text present, Escape clears the buffer instead (no cancel). */
  onCancel?: () => void;
  /** Imperative handle (React 19 ref-as-prop) exposing {@link ComposerHandle} so
   *  the controller can restore a pulled-back message into the buffer. */
  ref?: Ref<ComposerHandle>;
}) {
  const [value, setValue] = useState("");
  // The one out-of-band write into the otherwise composer-owned buffer: restoring
  // a message the user pulled back with Escape before any assistant output (#61).
  useImperativeHandle(ref, () => ({ restore: (text: string) => setValue(text) }), []);
  // Session prompt history (oldest first) for ↑/↓ recall in the input. Lives
  // here because this is where accepted submits are observed.
  const [history, setHistory] = useState<string[]>([]);
  const suggestions = focus && !busy ? suggestionsFor(value) : [];
  // Resolve the ASCII fallback once so the prompt glyph and the busy spinner agree
  // on the same character set (no mixed Unicode/ASCII within one composer line).
  const ascii = !unicodeOk();
  const set = glyphs(ascii);

  function submit(text: string) {
    const trimmed = text.trim();
    if (!trimmed) return;
    // The controller returns false synchronously when it rejects the submit (a
    // turn is already in flight). Keep the text so it isn't lost; any other
    // result (void, or a Promise for an accepted turn) means it was taken.
    if (onSubmit(trimmed) === false) return;
    // Record the accepted prompt for recall, collapsing immediate repeats and
    // bounding the buffer so a long session (or pasted prompts) can't grow it
    // without limit.
    setHistory((h) =>
      h[h.length - 1] === trimmed ? h : [...h, trimmed].slice(-HISTORY_LIMIT),
    );
    setValue("");
  }

  return (
    <Box flexDirection="column" width="100%" maxWidth={LIVE_CHROME_MAX_WIDTH}>
      {suggestions.length > 0 ? (
        <Box flexDirection="column" marginBottom={1} paddingLeft={2}>
          {suggestions.map(([cmd, desc]) => (
            <Text key={cmd}>
              <Text color={ui.color.info}>{cmd.padEnd(14)}</Text>
              <Text dimColor>{desc}</Text>
            </Text>
          ))}
        </Box>
      ) : null}

      <Divider />

      <Box>
        <Box flexGrow={1}>
          <MultilineInput
            value={value}
            focus={focus}
            history={history}
            onChange={setValue}
            onSubmit={submit}
            onCancel={() => {
              // Escape: while busy with an empty composer, abort the in-flight turn;
              // otherwise just clear the buffer (the long-standing cancel-edit gesture).
              // Treat a whitespace-only buffer as empty so a stray space doesn't swallow
              // the cancel gesture.
              if (busy && value.trim() === "") onCancel?.();
              else setValue("");
            }}
            onTab={() => {
              if (suggestions.length > 0) setValue(suggestions[0][0] + " ");
            }}
            prompt={set.active === "◌" ? "› " : "> "}
            placeholder="Ask Daintree to supervise, delegate, or inspect…"
          />
        </Box>
      </Box>

      {busy ? (
        // Keep the busy indicator on its own short row. Sharing the prompt row
        // forces either the placeholder or the stage to truncate inside the
        // shrink-safe chrome budget.
        <Text color={ui.color.info} wrap="truncate">
          <ThinkingDot ascii={ascii} /> {stage}
          {queueDepth > 0 ? ` · ${queueDepth} queued` : ""}
        </Text>
      ) : null}

      {/* Bracket the input top AND bottom so the field reads unmistakably as the
          place text goes — the hints below sit outside the rule. */}
      <Divider />

      <Box flexDirection="column">
        <Text wrap="truncate">
          <KeyHint keyName="/" action="commands" />
          <Text dimColor>{" · "}</Text>
          <KeyHint keyName="↑" action="history" />
          <Text dimColor>{" · "}</Text>
          <KeyHint keyName="^O" action="inspect ops" />
          {/* Surfaced only while a cancellable turn runs, so the gesture is
              discoverable exactly when it applies (Escape on the empty composer).
              Falls back to `busy` when the caller doesn't distinguish turn kinds. */}
          {(cancellable ?? busy) ? (
            <>
              <Text dimColor>{" · "}</Text>
              <KeyHint keyName="Esc" action="cancel" />
            </>
          ) : null}
        </Text>
        {/* Truncate so a long context summary (many agents/timers) can't widen this
            row past the live terminal and orphan a wrapped row into scrollback
            during a pane resize (#138). */}
        {contextHint ? (
          <Text dimColor wrap="truncate">
            {contextHint}
          </Text>
        ) : null}
      </Box>
    </Box>
  );
}
