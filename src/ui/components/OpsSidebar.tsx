import { Box, Text } from "ink";
import type { App } from "../../cli/app.js";
import type { DashboardState } from "../types.js";
import { useTerminalPreview } from "../hooks/useTerminalPreview.js";
import { WatcherPanel } from "./WatcherPanel.js";
import { InboxPanel } from "./InboxPanel.js";
import { TimerPanel } from "./TimerPanel.js";
import { AuditPanel } from "./AuditPanel.js";
import { TerminalPanel } from "./TerminalPanel.js";
import { theme } from "../theme.js";

export function OpsSidebar({
  app,
  dashboard,
  height,
}: {
  app: App;
  dashboard: DashboardState;
  height: number;
}) {
  const previews = useTerminalPreview(app, dashboard.watchers);
  // Budget the vertical space across the five sections.
  const section = Math.max(3, Math.floor((height - 1) / 5));
  return (
    <Box flexDirection="column" height={height} overflow="hidden">
      <Text bold color={theme.brand}>
        Operations Deck
      </Text>
      <WatcherPanel watchers={dashboard.watchers} height={section} />
      <TerminalPanel previews={previews} height={section} />
      <InboxPanel events={dashboard.inbox} height={section} />
      <TimerPanel timers={dashboard.timers} height={section} />
      <AuditPanel audit={dashboard.audit} height={section} />
    </Box>
  );
}
