import { Box, Text } from "ink";
import type { AttentionRow, Density } from "./model.js";
import { theme } from "../theme.js";

/**
 * The richest section on the home surface: what needs the human now. Shows a
 * severity glyph, a short title, one evidence line, optional relationship, and
 * recommended actions. Collapses to a single dense line in survival mode.
 */
export function AttentionSection({
  rows,
  density = "comfortable",
}: {
  rows: AttentionRow[];
  density?: Density;
}) {
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.dim}>Needs attention</Text>
      {rows.length === 0 ? (
        <Text color={theme.ok}>{"✓ nothing needs you"}</Text>
      ) : density === "dense" ? (
        rows.map((r) => (
          <Text key={r.id} color={r.color}>
            {r.symbol} {r.title}
            {r.actions ? <Text dimColor> · {r.actions.split(" · ")[0]}</Text> : null}
          </Text>
        ))
      ) : (
        rows.map((r) => (
          <Box key={r.id} flexDirection="column">
            <Text color={r.color}>
              {r.symbol} {r.title}
            </Text>
            {r.evidence ? <Text dimColor>  {r.evidence}</Text> : null}
            {density !== "compact" && r.related ? (
              <Text dimColor>  {r.related}</Text>
            ) : null}
            {density !== "compact" && r.actions ? (
              <Text color={theme.info}>  {r.actions}</Text>
            ) : null}
          </Box>
        ))
      )}
    </Box>
  );
}
