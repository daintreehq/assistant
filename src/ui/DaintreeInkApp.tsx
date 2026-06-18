import { useEffect, useState } from "react";
import { useApp, useInput, useWindowSize } from "ink";
import type { App as DaintreeApp } from "../cli/app.js";
import { useDaintreeController } from "./hooks/useDaintreeController.js";
import { useTerminalPreview } from "./hooks/useTerminalPreview.js";
import { ControlRoom, type View } from "./ControlRoom.js";
import { currentDebugLogPath } from "../debugLog.js";

/**
 * The live shell. It owns the runtime wiring (controller, terminal previews,
 * window size, key handling) and feeds a pure {@link ControlRoom} the resulting
 * state. One centralized UI mode (home / operations / help) keeps key handlers
 * from overlapping, and the composer is focusable only when home owns the
 * screen.
 *
 * Operational detail is a purposeful VIEW (`^O`, or a `/panel` command), never a
 * text dump. Esc returns home; `^X` toggles raw tool detail in the transcript.
 */
export function DaintreeInkApp({ app }: { app: DaintreeApp }) {
  const { exit } = useApp();
  const { columns, rows } = useWindowSize();
  const [view, setView] = useState<View>("home");
  const [expanded, setExpanded] = useState(false);
  const [scrollOffset, setScrollOffset] = useState(0);
  const controller = useDaintreeController(app, exit);
  const previews = useTerminalPreview(app, controller.dashboard.watchers);

  // A `/panel` command opens the matching purposeful view.
  useEffect(() => {
    if (controller.activePanel === "help") setView("help");
    else if (controller.activePanel) setView("operations");
  }, [controller.activePanel]);

  const returnHome = () => {
    setView("home");
    controller.setActivePanel(null);
  };

  const scrollPage = Math.max(6, rows - 8);
  // Per-step line scroll for arrow keys. On the alternate screen the terminal
  // has no native scrollback, so it converts mouse-wheel/trackpad ticks into
  // Up/Down arrow keypresses ("alternate scroll mode"). Binding those arrows to
  // the ledger is what makes the wheel scroll history; a few lines per event
  // keeps a wheel notch feeling responsive without overshooting.
  const scrollLine = 3;

  useInput((input, key) => {
    if (key.ctrl && input === "c") {
      exit();
      return;
    }
    if (view === "home" && key.pageUp) {
      setScrollOffset((n) => n + scrollPage);
      return;
    }
    if (view === "home" && key.pageDown) {
      setScrollOffset((n) => Math.max(0, n - scrollPage));
      return;
    }
    // Up/Down scroll the ledger a few lines at a time. This is also the wheel:
    // alternate-scroll mode delivers wheel ticks here as arrow keys. The
    // composer is single-line (only left/right move its cursor), so up/down are
    // free to own history scrolling.
    if (view === "home" && key.upArrow) {
      setScrollOffset((n) => n + scrollLine);
      return;
    }
    if (view === "home" && key.downArrow) {
      setScrollOffset((n) => Math.max(0, n - scrollLine));
      return;
    }
    if (view === "home" && key.home) {
      setScrollOffset(100_000);
      return;
    }
    if (view === "home" && key.end) {
      setScrollOffset(0);
      return;
    }
    if (key.escape) {
      if (view === "home" && scrollOffset > 0) {
        setScrollOffset(0);
        return;
      }
      if (view !== "home") returnHome();
      return;
    }
    // Ctrl chords only — these never collide with composing text.
    if (key.ctrl && input === "o") {
      if (view === "operations") returnHome();
      else setView("operations");
      return;
    }
    if (key.ctrl && input === "x") {
      setExpanded((e) => !e);
    }
  });

  const project =
    app.config.projectPath.split("/").pop() || app.config.projectPath;

  return (
    <ControlRoom
      project={project}
      tier={app.config.tier}
      columns={columns}
      rows={rows}
      connected={controller.dashboard.mcp.connected}
      transcript={controller.transcript}
      dashboard={controller.dashboard}
      previews={previews}
      busy={controller.busy}
      stage={controller.stage}
      view={view}
      expanded={expanded}
      pending={controller.pendingConfirm}
      scrollOffset={scrollOffset}
      logging={app.config.debugLog}
      logFile={app.config.debugLog ? currentDebugLogPath() : undefined}
      composerFocus={
        view === "home" && !controller.busy && !controller.pendingConfirm
      }
      onSubmit={(text) => {
        setScrollOffset(0);
        return controller.sendUserMessage(text);
      }}
      onResolve={controller.resolveConfirm}
    />
  );
}
