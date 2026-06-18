import { Box, Text } from "ink";
import type { App } from "../../cli/app.js";
import { theme } from "../theme.js";

/**
 * A single calm identity line. Rarely-changing context (project, tier) lives
 * here; live state (connection, counts) lives on the status line at the bottom,
 * so the header never needs to redraw and never competes for attention.
 */
export function Header({ app }: { app: App }) {
  const project =
    app.config.projectPath.split("/").pop() || app.config.projectPath;
  return (
    <Box marginBottom={1}>
      <Text bold color={theme.brand}>
        Daintree
      </Text>
      <Text dimColor>
        {" · "}
        {project}
        {" · "}
        {app.config.tier}
      </Text>
    </Box>
  );
}
