import { useState } from "react";
import { Box, Text, useApp, useInput, useWindowSize } from "ink";
import type { App as DaintreeApp } from "../cli/app.js";
import { useDaintreeController } from "./hooks/useDaintreeController.js";
import { Header } from "./components/Header.js";
import { Timeline } from "./components/Timeline.js";
import { OpsSidebar } from "./components/OpsSidebar.js";
import { StatusLine } from "./components/StatusLine.js";
import { AttentionBanner } from "./components/AttentionBanner.js";
import { Composer } from "./components/Composer.js";
import { ConfirmModal } from "./components/ConfirmModal.js";
import { HelpOverlay } from "./components/HelpOverlay.js";
import { theme } from "./theme.js";

/**
 * One single-column layout at every width — a calm conversation-first cockpit,
 * not a fixed-pane dashboard. Top to bottom: identity header → transcript →
 * (conditional) attention banner → status line → borderless composer.
 *
 * Operational detail (watchers, terminals, timers, audit) is detail-on-demand:
 * `^O` opens a full-width overlay over the transcript, `Esc` closes it. Ambient
 * awareness lives in two cheap places instead of a sidebar — discrete events
 * flow through the transcript, continuous counts roll up onto the status line.
 */
export function DaintreeInkApp({ app }: { app: DaintreeApp }) {
  const { exit } = useApp();
  const { columns, rows } = useWindowSize();
  const [showHelp, setShowHelp] = useState(false);
  const [showOps, setShowOps] = useState(false);
  const controller = useDaintreeController(app, exit);

  useInput((input, key) => {
    if (key.ctrl && input === "c") {
      exit();
      return;
    }
    if (key.escape) {
      setShowOps(false);
      setShowHelp(false);
      return;
    }
    if (input === "?") {
      setShowHelp((v) => !v);
      return;
    }
    if (key.ctrl && input === "o") {
      setShowOps((v) => !v);
    }
  });

  const showBanner =
    !controller.pendingConfirm && controller.dashboard.inbox.length > 0;
  const headerHeight = 2; // identity line + its bottom margin
  const chromeHeight = headerHeight + 1 /* status */ + 1 /* composer */ + (showBanner ? 1 : 0);
  // Clamp so the transcript never pushes the composer off a short terminal.
  const bodyHeight = Math.max(3, rows - chromeHeight);

  return (
    <Box flexDirection="column" height={rows} width={columns}>
      <Header app={app} />
      <Box flexGrow={1} flexDirection="column" overflow="hidden">
        {showOps ? (
          <Box
            flexDirection="column"
            height={bodyHeight}
            borderStyle="round"
            borderColor={theme.border}
            paddingX={1}
            overflow="hidden"
          >
            <OpsSidebar
              app={app}
              dashboard={controller.dashboard}
              height={Math.max(3, bodyHeight - 3)}
            />
            <Text dimColor>Esc close · ^O toggle</Text>
          </Box>
        ) : (
          <Timeline items={controller.timeline} height={bodyHeight} />
        )}
      </Box>
      {showBanner ? (
        <AttentionBanner events={controller.dashboard.inbox} />
      ) : null}
      <StatusLine dashboard={controller.dashboard} />
      <Composer
        busy={controller.busy}
        focus={!controller.busy && !controller.pendingConfirm}
        onSubmit={controller.sendUserMessage}
      />
      {controller.pendingConfirm ? (
        <ConfirmModal
          pending={controller.pendingConfirm}
          onResolve={controller.resolveConfirm}
        />
      ) : null}
      {showHelp ? <HelpOverlay /> : null}
    </Box>
  );
}
