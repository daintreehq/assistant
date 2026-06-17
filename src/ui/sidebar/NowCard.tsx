import { Box, Text } from "ink";
import type { NowState } from "./model.js";
import { theme } from "../theme.js";

/** The "what is Daintree doing right now" card. One line of state, one of detail. */
export function NowCard({ now, compact }: { now: NowState; compact?: boolean }) {
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.dim}>Now</Text>
      <Text color={now.color}>
        {now.symbol} {now.title}
      </Text>
      {!compact && now.detail ? <Text dimColor>  {now.detail}</Text> : null}
    </Box>
  );
}
