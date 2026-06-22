import { useEffect, useMemo, useRef, useState } from "react";
import { useKeyboard, useRenderer, useTerminalDimensions } from "@opentui/react";
import type { BoxRenderable } from "@opentui/core";
import type { App as DaintreeRuntime } from "../cli/app.js";
import { useDaintreeController } from "./hooks/useDaintreeController.js";
import { useAttentionSignal } from "./hooks/useAttentionSignal.js";
import { useTerminalPreview } from "./hooks/useTerminalPreview.js";
import {
  useScrollbackTranscript,
  type ScrollbackHeader,
} from "./hooks/useScrollbackTranscript.js";
import { useFooterHeight } from "./hooks/useFooterHeight.js";
import { useResizeRedraw } from "./hooks/useResizeRedraw.js";
import { ControlRoom, CONTENT_MAX, LEFT_PAD, type View } from "./ControlRoom.js";
import { Header } from "./components/Header.js";
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

  // The masthead shows the bound project's name. The controller seeds it from the
  // directory leaf and upgrades it to Daintree's authoritative project name once the
  // MCP `actions.getContext` resolves — see useDaintreeController.
  const project = controller.projectName ?? "";

  // Footer sizing + scrollback: mirror ControlRoom's insets so the committed masthead
  // and the live footer share one content measure. `rootRef` is the live footer's
  // outer box; the split-footer region is sized to its measured height.
  const rootRef = useRef<BoxRenderable | null>(null);
  const gutter = Math.max(1, app.config.reservedColumns ?? 1);
  const chromeWidth = Math.max(1, columns - gutter - LEFT_PAD);
  const contentWidth = Math.min(chromeWidth, CONTENT_MAX);

  // Row budget for an active turn's live body. Use the TRUE terminal height
  // (renderer.terminalHeight) — in split-footer `rows` from useTerminalDimensions is
  // the footer's render height, which would feed back on itself. ~40% of the screen
  // keeps the streaming pane generous while leaving rows for committed scrollback +
  // the status/composer chrome. A fixed budget = a fixed footer height = no flash.
  const terminalRows =
    (renderer as unknown as { terminalHeight?: number }).terminalHeight ?? rows;
  const liveMaxRows = Math.max(4, Math.min(16, Math.floor(terminalRows * 0.4)));

  // The masthead committed ONCE to native scrollback (so it scrolls up and away like
  // the rest of the history). It never reflects the live `destructivePending` cue —
  // that escalation surfaces on the ApprovalSheet down in the live footer instead.
  const header: ScrollbackHeader = useMemo(
    () => ({
      node: (
        <box paddingLeft={LEFT_PAD} paddingTop={1}>
          <Header
            columns={chromeWidth}
            project={project}
            tier={app.config.tier}
            logging={app.config.debugLog}
            logFile={app.config.debugLog ? currentDebugLogPath() : undefined}
          />
        </box>
      ),
      fallbackText: `Daintree Assistant — ${project} · tier ${app.config.tier}`,
    }),
    [chromeWidth, project, app.config.tier, app.config.debugLog],
  );

  // Hooks must run every render (before the boot early-return). While booting the
  // header is withheld (null) and the transcript is empty, so nothing commits until
  // the cockpit is actually up; `rootRef` is null during the splash, so the footer
  // stays seeded at full height for it.
  const { liveCells, commitSlot } = useScrollbackTranscript({
    renderer,
    transcript: controller.transcript,
    header: controller.booting ? null : header,
    width: contentWidth,
    expanded,
    // Both a `/clear` and a resize "nuclear redraw" reset the scrollback commit cursor and
    // re-commit the masthead. Summing the two monotonic nonces gives one strictly-rising
    // resetKey that changes on either event (neither counter ever decreases).
    resetKey: controller.clearNonce + controller.redrawNonce,
  });
  useFooterHeight(renderer, rootRef, rows);

  // On a settled terminal resize (and once when the boot splash hands off to the
  // cockpit), do a full "nuclear redraw": wipe the host scrollback and re-commit the
  // masthead + transcript fresh at the new width. This is what clears the duplicate
  // footer rows OpenTUI's split-footer leaves behind on resize and keeps the masthead
  // from scrolling away. Gated off during boot so the splash isn't interrupted.
  useResizeRedraw({
    enabled: !controller.booting,
    columns,
    rows,
    onRedraw: controller.requestRedraw,
  });

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
      transcript={liveCells}
      renderHeader={false}
      footerSlot={commitSlot}
      rootRef={rootRef}
      liveMaxRows={liveMaxRows}
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
