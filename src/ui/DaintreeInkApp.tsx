import { useState } from "react";
import { useApp, useInput, useWindowSize } from "ink";
import type { App as DaintreeApp } from "../cli/app.js";
import { useDaintreeController } from "./hooks/useDaintreeController.js";
import { useTerminalPreview } from "./hooks/useTerminalPreview.js";
import { ControlRoom } from "./ControlRoom.js";
import { currentDebugLogPath } from "../debugLog.js";

/**
 * The live shell. It owns the runtime wiring (controller, terminal previews,
 * window size, key handling) and feeds a pure {@link ControlRoom} the resulting
 * state.
 *
 * Scrollback belongs to the host terminal now — the cockpit renders inline (no
 * alternate buffer), so the wheel/trackpad and PgUp scroll history natively,
 * the same way Claude Code's panes do. We don't intercept arrow/page keys for
 * scrolling. Operational detail prints inline via `^O` (or a `/panel` command),
 * and `^X` toggles raw tool detail for the still-streaming turn.
 */
export function DaintreeInkApp({ app }: { app: DaintreeApp }) {
  const { exit } = useApp();
  const { columns, rows } = useWindowSize();
  const [expanded, setExpanded] = useState(false);
  const controller = useDaintreeController(app, exit);
  const previews = useTerminalPreview(app, controller.dashboard.watchers);

  useInput((input, key) => {
    if (key.ctrl && input === "c") {
      exit();
      return;
    }
    // Ctrl chords only — these never collide with composing text.
    if (key.ctrl && input === "o") {
      controller.openOps();
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
      expanded={expanded}
      pending={controller.pendingConfirm}
      logging={app.config.debugLog}
      logFile={app.config.debugLog ? currentDebugLogPath() : undefined}
      composerFocus={!controller.busy && !controller.pendingConfirm}
      onSubmit={controller.sendUserMessage}
      onResolve={controller.resolveConfirm}
    />
  );
}
