import { Box, Text, useInput } from "ink";
import type { PendingConfirm } from "../types.js";
import { compactArgs } from "../../utils/text.js";
import { theme } from "../theme.js";

export function ConfirmModal({
  pending,
  onResolve,
}: {
  pending: PendingConfirm;
  onResolve: (approved: boolean) => void;
}) {
  useInput((input, key) => {
    if (/^y$/i.test(input)) onResolve(true);
    else if (/^n$/i.test(input) || key.escape) onResolve(false);
  });

  const req = pending.request;
  return (
    <Box
      position="absolute"
      width="80%"
      marginLeft={4}
      marginTop={2}
      borderStyle="double"
      borderColor={theme.warn}
      paddingX={2}
      paddingY={1}
      flexDirection="column"
    >
      <Text bold color={theme.warn}>
        Confirm action
      </Text>
      <Text>
        {req.toolName} <Text dimColor>({req.risk})</Text>
      </Text>
      <Text dimColor>{req.summary}</Text>
      <Text dimColor>args: {compactArgs(req.args, 180)}</Text>
      <Box marginTop={1}>
        <Text color={theme.ok}>Y approve</Text>
        <Text dimColor> · </Text>
        <Text color={theme.error}>N / Esc decline</Text>
      </Box>
    </Box>
  );
}
