/**
 * The cockpit as a PURE presentational component: everything it needs is a prop,
 * nothing is fetched or subscribed. The live shell (DaintreeApp) feeds it from the
 * controller, the gallery feeds it from frozen fixtures, and tests feed it fixed
 * timestamps.
 *
 * INLINE MODEL (Claude Code style) on OpenTUI's `split-footer` renderer. This
 * component is ONLY the live footer: it renders the in-flight turn (`transcript` here
 * is the LIVE tail, not the whole history), the status line and the composer. Finished
 * turns and the masthead are committed to the terminal's native scrollback by the
 * shell (DaintreeApp via `useScrollbackTranscript`) and scroll up and away under the
 * host's own scrollbar — so this tree stays short and can never overflow the viewport
 * (the garble that `main-screen` caused once the tree outgrew the terminal height).
 *
 * `renderHeader` is true by default so the gallery/tests still show the masthead
 * inline; the live shell sets it false (the header is in scrollback instead) and feeds
 * `footerSlot` the in-flight scrollback-commit and `rootRef` so it can size the footer
 * to this tree's measured height.
 *
 * Operations and help are momentary, on-demand views rendered in place of the
 * composer (Esc returns), never a pinned panel.
 */
import type { ReactNode, Ref } from "react";
import { TextAttributes, type BoxRenderable } from "@opentui/core";
import type {
  DashboardState,
  PendingConfirm,
  SessionUsage,
  TranscriptCell,
} from "./types.js";
import type { TerminalPreview } from "./hooks/useTerminalPreview.js";
import { Header } from "./components/Header.js";
import { CellView } from "./components/Transcript.js";
import { OperationsView } from "./components/OperationsView.js";
import { StatusLine } from "./components/StatusLine.js";
import { Composer, type ComposerHandle } from "./components/Composer.js";
import { ApprovalSheet } from "./components/ApprovalSheet.js";
import { HelpOverlay } from "./components/HelpOverlay.js";
import type { PanelKey } from "../cli/commandData.js";

export type View = "home" | "operations" | "help";

/**
 * Cap the content width even in a very wide terminal: long lines that run the
 * full width of a maximised window are hard to read, so prose and run cells wrap
 * at a comfortable measure the way other conversational CLIs do.
 */
export const CONTENT_MAX = 100;

/** One-column left inset so content never touches the terminal's left edge. */
export const LEFT_PAD = 1;

export interface ControlRoomProps {
  /** Name of the bound project, shown in the masthead beneath the wordmark. */
  project: string;
  tier: string;
  columns: number;
  /**
   * Columns to hold back from the right edge — the autowrap/scrollbar safety gutter
   * ({@link AppConfig.reservedColumns}). Defaults to 1; the live shell raises it
   * (e.g. to 2 under a Daintree xterm whose overlay scrollbar covers the rightmost
   * cells). Drives both the right padding and the numeric `chromeWidth`.
   */
  reservedColumns?: number;
  /** Accepted for back-compat (gallery/tests); the inline cockpit ignores it. */
  rows?: number;
  connected: boolean;
  transcript: TranscriptCell[];
  dashboard: DashboardState;
  /** Live token/cost/context-pressure rollup, rendered in the status line. */
  sessionUsage?: SessionUsage;
  previews?: TerminalPreview[];
  busy: boolean;
  stage: string;
  /** User follow-ups queued behind the in-flight turn; surfaced in the composer
   *  busy indicator as "· N queued" (#95). Defaults to 0. */
  queueDepth?: number;
  view: View;
  /**
   * Which `/panel` command opened the operations view, so it can focus that one
   * section. `help` is rendered by its own `view` branch. Null shows the full deck.
   */
  activePanel?: PanelKey | null;
  expanded?: boolean;
  pending?: PendingConfirm | null;
  /** Frozen clock for deterministic rendering; defaults to live time. */
  now?: number;
  /** Debug logging is active — shown in the masthead. */
  logging?: boolean;
  /** Path of the active debug log, shown under the header so it can be tailed. */
  logFile?: string;
  composerFocus?: boolean;
  /** Whether the in-flight turn can be aborted (drives the composer's Esc hint). */
  cancellable?: boolean;
  onSubmit?: (value: string) => boolean | void | Promise<void>;
  /** Abort the in-flight turn (Escape on an empty composer while busy). */
  onCancel?: () => void;
  /** Handle the composer registers so a pulled-back message can be restored (#61). */
  composerRef?: Ref<ComposerHandle>;
  onResolve?: (approved: boolean) => void;
  /**
   * Render the masthead inline at the top of this tree. Default true (gallery/tests).
   * The live shell sets it false — there the masthead is committed to native scrollback
   * by {@link useScrollbackTranscript} so it scrolls away with the rest of the history.
   */
  renderHeader?: boolean;
  /**
   * Mounted (but visually portaled out) so the in-flight native-scrollback commit can
   * run as part of the live tree. Supplied by the shell; null in gallery/tests.
   */
  footerSlot?: ReactNode;
  /**
   * Ref to the outermost footer box so the shell can read its measured height and size
   * the split-footer region to exactly this tree (see `useFooterHeight`).
   */
  rootRef?: Ref<BoxRenderable>;
}

export function ControlRoom({
  project,
  tier,
  columns,
  reservedColumns = 1,
  connected,
  transcript,
  dashboard,
  sessionUsage,
  previews = [],
  busy,
  stage,
  queueDepth = 0,
  view,
  activePanel = null,
  expanded = false,
  pending = null,
  now = Date.now(),
  logging = false,
  logFile,
  composerFocus = false,
  cancellable = false,
  onSubmit = () => {},
  onCancel,
  composerRef,
  onResolve = () => {},
  renderHeader = true,
  footerSlot = null,
  rootRef,
}: ControlRoomProps) {
  // A one-column INSET on every side so nothing touches the terminal edges. `gutter`
  // (>=1, see `reservedColumns`) is the right inset — it also keeps glyphs clear of
  // the autowrap (DECAWM) column and any host scrollbar. `LEFT_PAD` is the matching
  // left inset. `chromeWidth` is the usable span after both insets; `contentWidth`
  // caps prose/history at a readable measure so it doesn't stretch across a maximized
  // terminal.
  const gutter = Math.max(1, reservedColumns);
  const chromeWidth = Math.max(1, columns - gutter - LEFT_PAD);
  const contentWidth = Math.min(chromeWidth, CONTENT_MAX);

  const contextHint = connected
    ? `agents ${dashboard.watchers.length} · tmr ${dashboard.timers.length}`
    : "MCP degraded";

  // The tier capsule only goes red when a DESTRUCTIVE (git/system) action is awaiting
  // confirmation — the moment red actually earns attention.
  const destructivePending =
    !!pending &&
    (pending.request.risk === "git" || pending.request.risk === "system");
  // The inbox is controller-filtered to actionable severities, so a non-empty inbox
  // is exactly "actionable attention pending" — the signal that promotes ^O.
  const attentionPending = dashboard.inbox.length > 0;

  // A confirmation is interactive and must surface in every view; while it is
  // pending the on-demand panels yield so the approval (and composer) stay live.
  const showPanel = !pending && view !== "home";

  return (
    // flexShrink={0}: keep the footer's NATURAL height even while the split-footer
    // region is momentarily shorter than the content. Otherwise the live box shrinks
    // to the current `footerHeight`, which then reads back as the measured height and
    // deadlocks `useFooterHeight` (it could never grow the footer back after a shrink).
    <box
      ref={rootRef}
      flexDirection="column"
      flexShrink={0}
      width="100%"
      paddingRight={gutter}
    >
      {/* The masthead. In the live cockpit it is committed to native scrollback (so it
          scrolls away with the history) and `renderHeader` is false; the gallery/tests
          render it inline. The Header owns its own closing rule (below the
          wordmark/project/tier lines, above the logging line). */}
      {renderHeader ? (
        <box paddingLeft={LEFT_PAD} paddingTop={1}>
          <Header
            columns={chromeWidth}
            project={project}
            tier={tier}
            destructivePending={destructivePending}
            logging={logging}
            logFile={logFile}
          />
        </box>
      ) : null}

      {/* The in-flight scrollback commit. It portals its content off into an off-screen
          surface, so it draws nothing here — it only needs to be mounted in the tree. */}
      {footerSlot}

      {/* The conversation + the live region, one column. The native renderer reflows
          the whole tree on resize, so there is no wrap/orphan hazard to design
          around; `contentWidth` is a readable-measure cap for prose/history. */}
      <box flexDirection="column" paddingLeft={LEFT_PAD}>
        {transcript.map((cell) => (
          <CellView
            key={cell.id}
            cell={cell}
            width={contentWidth}
            now={now}
            expanded={expanded}
          />
        ))}

        {pending ? (
          <ApprovalSheet
            pending={pending}
            width={Math.min(80, contentWidth)}
            onResolve={onResolve}
          />
        ) : null}

        {/* One blank line between the conversation and the status chrome below it. */}
        <box marginTop={1} flexDirection="column">
          <StatusLine
            dashboard={dashboard}
            sessionUsage={sessionUsage}
            width={contentWidth}
            now={now}
          />
        </box>

        {/* Breathing room between the conversation and the input. */}
        <box height={1} flexShrink={0} />

        {showPanel && view === "operations" ? (
          <box flexDirection="column">
            <OperationsView
              dashboard={dashboard}
              previews={previews}
              width={contentWidth}
              now={now}
              activePanel={activePanel === "help" ? null : activePanel}
            />
            <text attributes={TextAttributes.DIM}>Esc to return</text>
          </box>
        ) : showPanel && view === "help" ? (
          <box flexDirection="column">
            <HelpOverlay width={Math.min(76, contentWidth)} />
            <text attributes={TextAttributes.DIM}>Esc to return</text>
          </box>
        ) : (
          <Composer
            busy={busy}
            stage={stage}
            queueDepth={queueDepth}
            contextHint={contextHint}
            focus={composerFocus}
            cancellable={cancellable}
            attentionPending={attentionPending}
            onSubmit={onSubmit}
            onCancel={onCancel}
            ref={composerRef}
          />
        )}
      </box>
    </box>
  );
}
