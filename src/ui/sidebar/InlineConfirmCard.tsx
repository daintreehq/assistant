import { Box, Text, useInput } from "ink";
import type { PendingConfirm } from "../types.js";
import { compactArgs, fit } from "../../utils/text.js";
import { theme } from "../theme.js";

/**
 * Sidebar-native confirmation. Instead of a floating modal that hides the deck,
 * this is an inline blocking card directly under the header. The composer is
 * frozen while it is visible. Same keys as the wide modal: Y / N / Esc.
 */
export function InlineConfirmCard({
  pending,
  onResolve,
  width,
}: {
  pending: PendingConfirm;
  onResolve: (approved: boolean) => void;
  width: number;
}) {
  useInput((input, key) => {
    if (/^y$/i.test(input)) onResolve(true);
    else if (/^n$/i.test(input) || key.escape) onResolve(false);
  });

  const req = pending.request;
  return (
    <Box
      flexDirection="column"
      marginTop={1}
      borderStyle="round"
      borderColor={theme.warn}
      paddingX={1}
    >
      <Box justifyContent="space-between">
        <Text bold color={theme.warn}>
          Confirm action
        </Text>
        <Text dimColor>risk: {req.risk}</Text>
      </Box>
      <Text>{fit(req.toolName, width, 2)}</Text>
      {req.summary ? <Text dimColor>{fit(req.summary, width, 2)}</Text> : null}
      <Text dimColor>{compactArgs(req.args, Math.max(20, width - 2))}</Text>
      <Text>
        <Text color={theme.ok}>Y approve</Text>
        <Text dimColor> · </Text>
        <Text color={theme.error}>N decline</Text>
      </Text>
    </Box>
  );
}
