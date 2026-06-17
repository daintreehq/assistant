import { Box, Text } from "ink";
import type { App } from "../../cli/app.js";
import type { DashboardState } from "../types.js";
import { theme } from "../theme.js";

export function Header({
  app,
  dashboard,
}: {
  app: App;
  dashboard: DashboardState;
}) {
  const connected = dashboard.mcp.connected;
  const project =
    app.config.projectPath.split("/").pop() || app.config.projectPath;
  return (
    <Box flexDirection="column" marginBottom={1}>
      <Box justifyContent="space-between">
        <Text bold color={theme.brand}>
          Daintree Assistant
        </Text>
        <Text dimColor>local operations officer</Text>
      </Box>
      <Box gap={2}>
        <Text dimColor>
          project <Text color="white">{project}</Text>
        </Text>
        <Text dimColor>
          mcp{" "}
          <Text color={connected ? theme.ok : theme.warn}>
            {connected ? "connected" : "degraded"}
          </Text>
        </Text>
        <Text dimColor>
          tier <Text color="white">{app.config.tier}</Text>
        </Text>
        <Text dimColor>
          watchers <Text color="white">{dashboard.watchers.length}</Text>
        </Text>
        <Text dimColor>
          inbox <Text color="white">{dashboard.inbox.length}</Text>
        </Text>
      </Box>
    </Box>
  );
}
