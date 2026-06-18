import { useState } from "react";
import { Box, Text } from "ink";
import { Divider, KeyHint } from "../primitives.js";
import { glyphs, ui } from "../theme.js";
import { MultilineInput } from "./MultilineInput.js";

/** Slash commands surfaced as a filterable palette — described by intent. */
export const COMMAND_SUGGESTIONS: Array<[string, string]> = [
  ["/status", "connection and session"],
  ["/inbox", "items requiring attention"],
  ["/watchers", "supervised agents"],
  ["/timers", "scheduled operations"],
  ["/audit", "recent tool calls"],
  ["/tools", "list / search tools"],
  ["/permissions", "supervisor | operator | system"],
  ["/recipes", "loaded · reload · load · clear"],
  ["/compact", "summarize the conversation"],
  ["/doctor", "environment check"],
  ["/reconnect", "retry the Daintree connection"],
  ["/help", "all commands and keys"],
  ["/quit", "exit"],
];

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
  onSubmit,
}: {
  busy: boolean;
  /** When false the input ignores keystrokes (e.g. during a turn or a modal). */
  focus?: boolean;
  /** Live stage label shown while busy (Inspecting, Delegating, Watching…). */
  stage?: string;
  /** Right-aligned context summary on the second line. */
  contextHint?: string;
  width?: number;
  onSubmit: (value: string) => boolean | void | Promise<void>;
}) {
  const [value, setValue] = useState("");
  const suggestions = focus && !busy ? suggestionsFor(value) : [];
  const set = glyphs();

  function submit(text: string) {
    const trimmed = text.trim();
    if (!trimmed) return;
    // The controller returns false synchronously when it rejects the submit (a
    // turn is already in flight). Keep the text so it isn't lost; any other
    // result (void, or a Promise for an accepted turn) means it was taken.
    if (onSubmit(trimmed) === false) return;
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
            onChange={setValue}
            onSubmit={submit}
            onCancel={() => setValue("")}
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
          <KeyHint keyName="\\⏎" action="newline" />
          <Text dimColor>{" · "}</Text>
          <KeyHint keyName="^O" action="inspect ops" />
        </Box>
        {contextHint ? <Text dimColor>{contextHint}</Text> : null}
      </Box>
    </Box>
  );
}
