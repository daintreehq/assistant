import { useEffect, useState } from "react";
import { useKeyboard, useRenderer, useTerminalDimensions } from "@opentui/react";
import type { App as DaintreeRuntime } from "../cli/app.js";
import { useDaintreeController } from "./hooks/useDaintreeController.js";
import { useAttentionSignal } from "./hooks/useAttentionSignal.js";
import { useTerminalPreview } from "./hooks/useTerminalPreview.js";
import { ControlRoom, type View } from "./ControlRoom.js";
import { StartupSplash } from "./components/StartupSplash.js";
import { currentDebugLogPath } from "../debugLog.js";

/**
 * The live shell. It owns the runtime wiring (controller, terminal previews,
 * terminal size, key handling) and feeds a pure {@link ControlRoom} the resulting
 * state. One centralized UI mode (home / operations / help) keeps key handlers
 * from overlapping, and the composer is focusable only when home owns the screen.
 *
 * Operational detail is a purposeful VIEW (`^O`, or a `/panel` command), never a
 * text dump. Esc returns home; `^X` toggles raw tool detail in the transcript.
 *
 * OpenTUI port: terminal size comes from `useTerminalDimensions()` (`{width,height}`)
 * and global keys from `useKeyboard((e) => …)` — both replace the Ink hooks. There
 * is no `useApp().exit`; the bootstrap injects an `exit` callback that tears down the
 * renderer + runtime. `useKeyboard` is global (every subscriber sees every key), so
 * this handler only ever acts on the app chords (^C/Esc/^O/^X) and leaves everything
 * else for the focused composer, which gates on `composerFocus`.
 */
export function DaintreeApp({
  app,
  exit,
}: {
  app: DaintreeRuntime;
  /** Tear down the renderer + runtime and end the process (bootstrap-owned). */
  exit: () => void;
}) {
  const { width: columns, height: rows } = useTerminalDimensions();
  const renderer = useRenderer();
  const [view, setView] = useState<View>("home");
  const [expanded, setExpanded] = useState(false);
  // The renderer is passed to the controller so `/clear` can force a clean full
  // repaint after wiping the host scrollback (the controller stays @opentui-free).
  const controller = useDaintreeController(app, exit, renderer);
  const previews = useTerminalPreview(app, controller.dashboard.watchers);

  // Out-of-band cue (BEL + window-title badge) so a fresh attention event reaches
  // the user even when they've switched focus away from the cockpit.
  useAttentionSignal({
    bridge: controller.bridge,
    inboxCount: controller.dashboard.inbox.length,
  });

  // A `/panel` command opens the matching purposeful view.
  useEffect(() => {
    if (controller.activePanel === "help") setView("help");
    else if (controller.activePanel) setView("operations");
  }, [controller.activePanel]);

  const returnHome = () => {
    setView("home");
    controller.setActivePanel(null);
  };

  useKeyboard((e) => {
    const name = e.name ?? "";
    if (e.ctrl && name === "c") {
      exit();
      return;
    }
    // While the boot splash owns the screen, only Ctrl-C (quit) is live — swallow the
    // view/expand chords so the cockpit can't surface mid-animation in a non-home
    // state (e.g. a stray ^O leaving it on the operations deck once boot finishes).
    if (controller.booting) return;
    if (name === "escape") {
      // On home the composer owns Escape (clear buffer, or cancel the turn when
      // empty+busy) — handled by MultilineInput, which is focused there. This handler
      // acts ONLY off-home (where the composer is unfocused) to avoid double-firing.
      if (view !== "home") returnHome();
      return;
    }
    // Ctrl chords only — these never collide with composing text.
    if (e.ctrl && name === "o") {
      if (view === "operations") returnHome();
      else {
        // ^O opens the full operations deck. Clear any panel left set by a prior
        // `/watchers`-style command so the deck isn't still filtered to one section.
        controller.setActivePanel(null);
        setView("operations");
      }
      return;
    }
    if (e.ctrl && name === "x") {
      setExpanded((x) => !x);
    }
  });

  // The masthead shows the bound project's name. The controller seeds it from the
  // directory leaf and upgrades it to Daintree's authoritative project name once
  // the MCP `actions.getContext` resolves — see useDaintreeController.
  const project = controller.projectName ?? "";

  // While the session connects/loads in the background, the horizontally-centered
  // logo-reveal plays inline (natural height, not a full-screen takeover); it
  // dissolves into the cockpit once startup has settled and the draw has finished
  // (see useDaintreeController's boot gate).
  if (controller.booting) {
    return (
      <StartupSplash
        columns={columns}
        rows={rows}
        onComplete={controller.notifyAnimationDone}
      />
    );
  }

  return (
    <ControlRoom
      project={project}
      tier={app.config.tier}
      columns={columns}
      reservedColumns={app.config.reservedColumns}
      rows={rows}
      connected={controller.dashboard.mcp.connected}
      transcript={controller.transcript}
      dashboard={controller.dashboard}
      sessionUsage={controller.sessionUsage}
      previews={previews}
      busy={controller.busy}
      stage={controller.stage}
      queueDepth={controller.queueDepth}
      view={view}
      activePanel={controller.activePanel}
      expanded={expanded}
      pending={controller.pendingConfirm}
      logging={app.config.debugLog}
      logFile={app.config.debugLog ? currentDebugLogPath() : undefined}
      composerFocus={view === "home" && !controller.pendingConfirm}
      cancellable={controller.canCancel}
      onSubmit={controller.sendUserMessage}
      onCancel={controller.pullBackTurn}
      composerRef={controller.composerRef}
      onResolve={controller.resolveConfirm}
    />
  );
}
