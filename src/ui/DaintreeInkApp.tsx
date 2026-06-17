import { useState } from "react";
import { Box, useApp, useInput, useWindowSize } from "ink";
import type { App as DaintreeApp } from "../cli/app.js";
import { useDaintreeController } from "./hooks/useDaintreeController.js";
import { Header } from "./components/Header.js";
import { Timeline } from "./components/Timeline.js";
import { OpsSidebar } from "./components/OpsSidebar.js";
import { Composer } from "./components/Composer.js";
import { ConfirmModal } from "./components/ConfirmModal.js";
import { HelpOverlay } from "./components/HelpOverlay.js";

export function DaintreeInkApp({ app }: { app: DaintreeApp }) {
  const { exit } = useApp();
  const { columns, rows } = useWindowSize();
  const [showHelp, setShowHelp] = useState(false);
  const [showOps, setShowOps] = useState(true);
  const controller = useDaintreeController(app, exit);

  useInput((input, key) => {
    if (key.ctrl && input === "c") {
      exit();
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

  const compact = columns < 110;
  const headerHeight = 3;
  const composerHeight = 3;
  // Clamp so the body never exceeds the remaining rows on a short terminal
  // (which would push the composer off-screen / overflow the root).
  const bodyHeight = Math.max(3, rows - headerHeight - composerHeight);

  return (
    <Box flexDirection="column" height={rows} width={columns}>
      <Header app={app} dashboard={controller.dashboard} />
      <Box height={bodyHeight} flexDirection="row">
        <Box flexGrow={1} flexDirection="column" paddingRight={1}>
          <Timeline items={controller.timeline} height={bodyHeight} />
        </Box>
        {showOps && !compact ? (
          <Box width={44} borderStyle="round" paddingX={1}>
            <OpsSidebar
              app={app}
              dashboard={controller.dashboard}
              height={bodyHeight - 2}
            />
          </Box>
        ) : null}
      </Box>
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
