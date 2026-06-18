import { useState } from "react";
import { Box, Text } from "ink";
import TextInput from "ink-text-input";
import Spinner from "ink-spinner";
import { theme } from "../theme.js";

export function Composer({
  busy,
  focus = true,
  onSubmit,
}: {
  busy: boolean;
  /** When false the input ignores keystrokes (e.g. during a turn or a modal). */
  focus?: boolean;
  onSubmit: (value: string) => void | Promise<void>;
}) {
  const [value, setValue] = useState("");
  return (
    <Box alignItems="center">
      <Text color={theme.brand}>daintree ❯ </Text>
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
          placeholder="Ask, supervise, spawn, watch, summarize…  (/help)"
        />
      </Box>
      <Box marginLeft={1}>
        {busy ? (
          <Text color={theme.info}>
            <Spinner type="dots" /> thinking
          </Text>
        ) : (
          <Text dimColor>? help · ^O ops · ^C exit</Text>
        )}
      </Box>
    </Box>
  );
}
