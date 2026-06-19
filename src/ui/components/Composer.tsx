import { useState } from "react";
import { Box, Text } from "ink";
import { Divider, KeyHint } from "../primitives.js";
import { glyphs, ui } from "../theme.js";
import { MultilineInput } from "./MultilineInput.js";
import { paletteEntries } from "../../commandRegistry.js";

/**
 * Slash commands surfaced as a filterable palette — described by intent. Derived
 * from the shared command registry so the palette can't drift from the commands
 * the handlers actually accept (issue #50).
 */
export const COMMAND_SUGGESTIONS: Array<[string, string]> = paletteEntries();

/** Cap on the recallable prompt history kept for ↑/↓ during a session. */
const HISTORY_LIMIT = 200;

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
  contextHint,
  width = 72,
  cancellable,
  onSubmit,
  onCancel,
}: {
  busy: boolean;
  /** When false the input ignores keystrokes (e.g. during a turn or a modal). */
  focus?: boolean;
  /** Live stage label shown while busy (Inspecting, Delegating, Watching…). */
  stage?: string;
  /** Right-aligned context summary on the second line. */
  contextHint?: string;
  width?: number;
  /** Whether the in-flight turn can be aborted; gates the "Esc cancel" hint.
   *  Defaults to `busy` so callers that don't distinguish turn kinds still show it. */
  cancellable?: boolean;
  onSubmit: (value: string) => boolean | void | Promise<void>;
  /** Abort the in-flight turn — invoked on Escape when the composer is empty and
   *  busy. With text present, Escape clears the buffer instead (no cancel). */
  onCancel?: () => void;
}) {
  const [value, setValue] = useState("");
  // Session prompt history (oldest first) for ↑/↓ recall in the input. Lives
  // here because this is where accepted submits are observed.
  const [history, setHistory] = useState<string[]>([]);
  const suggestions = focus && !busy ? suggestionsFor(value) : [];
  const set = glyphs();

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
    <Box flexDirection="column">
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

      <Divider width={width} />

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
        {busy ? (
          <Box marginLeft={1}>
            <Text color={ui.color.info}>
              {set.active} {stage}
            </Text>
          </Box>
        ) : null}
      </Box>

      {/* Bracket the input top AND bottom so the field reads unmistakably as the
          place text goes — the hints below sit outside the rule. */}
      <Divider width={width} />

      <Box justifyContent="space-between">
        <Box>
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
        </Box>
        {contextHint ? <Text dimColor>{contextHint}</Text> : null}
      </Box>
    </Box>
  );
}
