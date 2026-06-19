import { useEffect, useState } from "react";
import { useApp, useInput, useWindowSize } from "ink";
import type { App as DaintreeApp } from "../cli/app.js";
import { useDaintreeController } from "./hooks/useDaintreeController.js";
import { useAttentionSignal } from "./hooks/useAttentionSignal.js";
import { useTerminalPreview } from "./hooks/useTerminalPreview.js";
import { ControlRoom, type View } from "./ControlRoom.js";
import { StartupSplash } from "./components/StartupSplash.js";
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
  // How many turns back the transcript is scrolled (0 = latest). Owned here, not in
  // the pure-presentational ControlRoom/Transcript; paged with PageUp/PageDown and
  // snapped to 0 whenever a new message is accepted (see handleSubmit).
  const [scrollOffset, setScrollOffset] = useState(0);
  const controller = useDaintreeController(app, exit);
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

  // Submitting a message always returns to the latest turn so the user sees their
  // new turn land, even if they'd paged back into history. Only snap back when the
  // controller actually accepts the message (an empty buffer returns false).
  const handleSubmit = (value: string) => {
    const accepted = controller.sendUserMessage(value);
    if (accepted) setScrollOffset(0);
    return accepted;
  };

  useInput((input, key) => {
    if (key.ctrl && input === "c") {
      exit();
      return;
    }
    // While the boot splash owns the screen, only Ctrl-C (quit) is live — swallow the
    // view/expand chords so the cockpit can't surface mid-animation in a non-home
    // state (e.g. a stray ^O leaving it on the operations deck once boot finishes).
    if (controller.booting) return;
    // PageUp/PageDown page the transcript one turn at a time — only on the home
    // screen, and not while an approval modal owns the keys. The offset is clamped
    // on the way into ControlRoom, so over-paging past the oldest turn is harmless.
    if (key.pageUp || key.pageDown) {
      if (view !== "home" || controller.pendingConfirm) return;
      if (key.pageUp) {
        setScrollOffset((o) =>
          Math.min(o + 1, Math.max(0, controller.transcript.length - 1)),
        );
      } else {
        setScrollOffset((o) => Math.max(0, o - 1));
      }
      return;
    }
    if (key.escape) {
      // On home the composer owns Escape (clear buffer, or cancel the turn when
      // empty+busy) — handled by MultilineInput, which is focused there. Ink has no
      // stop-propagation, so this handler must act ONLY off-home to avoid double-
      // firing on the same keypress.
      if (view !== "home") returnHome();
      return;
    }
    // Ctrl chords only — these never collide with composing text.
    if (key.ctrl && input === "o") {
      if (view === "operations") returnHome();
      else {
        // ^O opens the full operations deck. Clear any panel left set by a prior
        // `/watchers`-style command so the deck isn't still filtered to one section.
        controller.setActivePanel(null);
        setView("operations");
      }
      return;
    }
    if (key.ctrl && input === "x") {
      setExpanded((e) => !e);
    }
  });

  // The masthead shows the bound project's name. The controller seeds it from the
  // directory leaf and upgrades it to Daintree's authoritative project name (via the
  // MCP `actions.getContext`) once that resolves — see useDaintreeController.
  const project = controller.projectName ?? "";

  // While the session connects/loads in the background, the centered logo-reveal owns
  // the screen; it dissolves into the cockpit once startup has settled and the draw
  // has finished (see useDaintreeController's boot gate).
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
      rows={rows}
      connected={controller.dashboard.mcp.connected}
      transcript={controller.transcript}
      dashboard={controller.dashboard}
      sessionUsage={controller.sessionUsage}
      previews={previews}
      busy={controller.busy}
      stage={controller.stage}
      view={view}
      activePanel={controller.activePanel}
      expanded={expanded}
      transcriptScrollOffset={Math.min(
        scrollOffset,
        Math.max(0, controller.transcript.length - 1),
      )}
      pending={controller.pendingConfirm}
      logging={app.config.debugLog}
      logFile={app.config.debugLog ? currentDebugLogPath() : undefined}
      composerFocus={view === "home" && !controller.pendingConfirm}
      cancellable={controller.canCancel}
      onSubmit={handleSubmit}
      onCancel={controller.pullBackTurn}
      composerRef={controller.composerRef}
      onResolve={controller.resolveConfirm}
    />
  );
}
