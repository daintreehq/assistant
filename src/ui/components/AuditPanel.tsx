import { Box, Text } from "ink";
import type { AuditRecord } from "../../schemas.js";
import { theme } from "../theme.js";
import { truncate } from "../../utils/text.js";

function outcomeColor(outcome: AuditRecord["outcome"]): string {
  switch (outcome) {
    case "ok":
    case "grant_ok":
      return theme.ok;
    case "denied":
      return theme.warn;
    case "error":
      return theme.error;
    default:
      return theme.dim;
  }
}

export function AuditPanel({
  audit,
  height,
}: {
  audit: AuditRecord[];
  height: number;
}) {
  const visible = audit.slice(0, Math.max(0, height - 1));
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.info}>Audit</Text>
      {visible.length === 0 ? (
        <Text dimColor>none</Text>
      ) : (
        visible.map((r) => (
          <Text key={r.id}>
            <Text color={outcomeColor(r.outcome)}>•</Text>{" "}
            <Text dimColor>{truncate(r.toolName, 22)}</Text>{" "}
            <Text color={theme.dim}>{r.durationMs}ms</Text>
          </Text>
        ))
      )}
    </Box>
  );
}
