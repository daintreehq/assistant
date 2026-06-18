import { useState } from "react";
import { Box, Text, useInput } from "ink";
import TextInput from "ink-text-input";
import { Divider, KeyHint } from "../primitives.js";
import { glyphs, ui } from "../theme.js";

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
 * The composer is always fixed to the bottom. Guidance stays short here; the
 * scrollable intro/help surfaces carry the long explanations. Typing `/` opens a
 * command palette; Tab completes the top match. While busy, the actual stage is
 * shown, not a generic "thinking".
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
  onSubmit: (value: string) => void | Promise<void>;
}) {
  const [value, setValue] = useState("");
  const suggestions = focus && !busy ? suggestionsFor(value) : [];
  const set = glyphs();
  const compactHints = width < 64;

  useInput(
    (_input, key) => {
      // Tab completes the top suggestion.
      if (key.tab && suggestions.length > 0) {
        setValue(suggestions[0][0] + " ");
      }
    },
    { isActive: focus && !busy },
  );

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
        <Text color={ui.color.accent}>{set.active === "◌" ? "›" : ">"} </Text>
        <Box flexGrow={1}>
          <TextInput
            value={value}
            focus={focus}
            showCursor={focus}
            onChange={setValue}
            onSubmit={(text) => {
              const trimmed = text.trim();
              if (!trimmed) return;
              setValue("");
              void onSubmit(trimmed);
            }}
            placeholder="Ask Daintree..."
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

      {/* Bracket the input row top and bottom so it's unmistakable where text goes. */}
      <Divider width={width} />

      <Box justifyContent="space-between">
        <Box>
          <KeyHint keyName="/" action={compactHints ? "cmd" : "commands"} />
          <Text dimColor>{" · "}</Text>
          <KeyHint keyName="^O" action="ops" />
          <Text dimColor>{" · "}</Text>
          <KeyHint keyName="^C" action="exit" />
        </Box>
        {contextHint ? <Text dimColor>{contextHint}</Text> : null}
      </Box>
    </Box>
  );
}
