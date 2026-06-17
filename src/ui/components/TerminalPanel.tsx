import { Box, Text } from "ink";
import type { TerminalPreview } from "../hooks/useTerminalPreview.js";
import { truncate } from "../../utils/text.js";
import { theme } from "../theme.js";

export function TerminalPanel({
  previews,
  height,
}: {
  previews: TerminalPreview[];
  height: number;
}) {
  const cards = previews.slice(0, 2);
  const linesPerCard = Math.max(2, Math.floor((height - 2) / Math.max(1, cards.length)) - 1);
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.info}>Terminals</Text>
      {cards.length === 0 ? (
        <Text dimColor>no watched terminal preview</Text>
      ) : (
        cards.map((p) => (
          <Box key={p.terminalId} flexDirection="column" marginTop={1}>
            <Text>
              <Text color={theme.brand}>{p.terminalId}</Text>{" "}
              <Text dimColor>
                {p.agentState ?? p.runtimeStatus ?? "unknown"}
              </Text>
            </Text>
            {p.tail
              .split("\n")
              .filter((l) => l.trim().length > 0)
              .slice(-linesPerCard)
              .map((line, index) => (
                <Text key={index} dimColor>
                  {truncate(line, 38)}
                </Text>
              ))}
          </Box>
        ))
      )}
    </Box>
  );
}
