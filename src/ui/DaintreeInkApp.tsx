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
import { SidebarShell } from "./sidebar/SidebarShell.js";

export type LayoutMode = "sidebar" | "balanced" | "wide";

/**
 * Pick a layout mode from the terminal width. Below 72 cols (the Daintree
 * sidebar default) we drop the two-pane chat layout entirely for a single-column
 * operations cockpit — the deck is the product surface, never hidden.
 */
export function layoutMode(columns: number): LayoutMode {
  if (columns < 72) return "sidebar";
  if (columns < 110) return "balanced";
  return "wide";
}

export function DaintreeInkApp({ app }: { app: DaintreeApp }) {
  const { exit } = useApp();
  const { columns, rows } = useWindowSize();
  const [showHelp, setShowHelp] = useState(false);
  const [showOps, setShowOps] = useState(true);
  const controller = useDaintreeController(app, exit);
  const mode = layoutMode(columns);

  useInput((input, key) => {
    if (key.ctrl && input === "c") {
      exit();
      return;
    }
    if (input === "?") {
      if (mode === "sidebar") {
        controller.setActivePanel(controller.activePanel === "help" ? null : "help");
      } else {
        setShowHelp((v) => !v);
      }
      return;
    }
    if (key.ctrl && input === "o") {
      setShowOps((v) => !v);
    }
  });

  // Narrow widths become the single-column cockpit instead of hiding the deck.
  if (mode === "sidebar") {
    return (
      <SidebarShell app={app} controller={controller} columns={columns} rows={rows} />
    );
  }

  const headerHeight = 3;
  const composerHeight = 3;
  // Clamp so the body never exceeds the remaining rows on a short terminal
  // (which would push the composer off-screen / overflow the root).
  const bodyHeight = Math.max(3, rows - headerHeight - composerHeight);
  // Wide keeps the roomy fixed deck; balanced gets a slimmer width-aware deck
  // that ^O can toggle away to give the chat the full pane.
  const deckWidth = mode === "wide" ? 44 : Math.max(32, Math.min(42, columns - 44));

  return (
    <Box flexDirection="column" height={rows} width={columns}>
      <Header app={app} dashboard={controller.dashboard} />
      <Box height={bodyHeight} flexDirection="row">
        <Box flexGrow={1} flexDirection="column" paddingRight={1}>
          <Timeline items={controller.timeline} height={bodyHeight} />
        </Box>
        {showOps ? (
          <Box width={deckWidth} borderStyle="round" paddingX={1}>
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
