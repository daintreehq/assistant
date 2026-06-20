/**
 * The cockpit as a PURE presentational component: everything it needs is a prop,
 * nothing is fetched or subscribed. The live shell (DaintreeInkApp) feeds it from
 * the controller, the gallery feeds it from frozen fixtures, and tests feed it
 * fixed timestamps.
 *
 * INLINE MODEL (Claude Code style). The cockpit renders into the terminal's MAIN
 * screen buffer, not the alternate buffer, so the terminal's own scrollback /
 * mouse wheel / selection work natively. Completed turns are committed permanently
 * with Ink's <Static>: they print ONCE, flow into native scrollback and never
 * repaint. The in-flight turn, the masthead, the status line and the composer live
 * in the repainting region pinned at the bottom. The masthead lives here (not in
 * <Static>) on purpose: its rule must REFLOW on resize like the composer rules,
 * which only the repainting region can do. The accepted trade-off is that a
 * repainting masthead settles just above the input — below the committed
 * scrollback — rather than pinned at the very top, and history that has already
 * scrolled off keeps its commit-time width.
 *
 * Operations and help are momentary, on-demand views rendered in place of the
 * composer (Esc returns), never a pinned panel — a pinned panel is impossible in
 * the main buffer without re-taking the alternate screen and losing scrollback.
 */
import type { Ref } from "react";
import { Box, Static, Text } from "ink";
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
const CONTENT_MAX = 100;

/**
 * Index in `cells` where the live (repainting) tail begins. Everything before it
 * is immutable and committed to <Static>; everything at/after it is re-rendered
 * each pass. Only the trailing turn — and then only while it is still `active` —
 * mutates (the reducer attaches tokens/tools/notes to it). A background note can
 * land *after* the active turn, so the tail is "the active turn and anything after
 * it", which all commits together once the turn finishes.
 */
function liveTailStart(cells: TranscriptCell[]): number {
  for (let i = cells.length - 1; i >= 0; i--) {
    const c = cells[i];
    if (c.kind === "turn") return c.state === "active" ? i : cells.length;
  }
  return cells.length;
}

export interface ControlRoomProps {
  /** Name of the bound project, shown in the masthead beneath the wordmark. */
  project: string;
  tier: string;
  columns: number;
  /**
   * Columns to hold back from the right edge — the autowrap/scrollbar safety gutter
   * ({@link AppConfig.reservedColumns}). Defaults to 1 (DECAWM only) so existing
   * callers/tests are unchanged; the live shell raises it (e.g. to 2 under a Daintree
   * xterm whose overlay scrollbar covers the rightmost cells). Both the right padding
   * of the repainting region AND the numeric `chromeWidth` derive from it, so the
   * masthead rule, the live rules and the truncated chrome all stop at the same
   * column.
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
  /**
   * Bumped by `/clear` to force the `<Static>` region to remount. Ink caches all
   * static output in `fullStaticOutput` and replays it on resize/overflow, so
   * clearing the transcript without changing this key leaves cleared cells able to
   * ghost back. Using it as the `<Static>` `key` unmounts/remounts and purges that
   * cache. Defaults to 0 (gallery/tests that never clear).
   */
  staticKey?: number;
  expanded?: boolean;
  pending?: PendingConfirm | null;
  /** Frozen clock for deterministic rendering; defaults to live time. */
  now?: number;
  /** Debug logging is active — shown in the one-time header banner. */
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
}

/** A committed transcript cell, keyed for <Static>. */
type StaticItem = { key: string; cell: TranscriptCell };

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
  staticKey = 0,
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
}: ControlRoomProps) {
  // Reserve the terminal's last column AND keep the repainting region pinned to the
  // terminal's *live* width. The cockpit renders inline (main buffer, no alternate
  // screen), so the bottom region is erased and redrawn by moving the cursor up by
  // Ink's logical line count — NOT the terminal's physical wrapping. Two distinct
  // failure modes orphan a stale copy into scrollback:
  //
  //  1. Steady state: if a dynamic line fills the full width its final glyph lands
  //     in the autowrap (DECAWM) column, where many terminals wrap it onto a second
  //     physical row invisible to Ink's height accounting; the next repaint then
  //     moves up one row short and orphans the top row. `paddingRight={1}` keeps the
  //     content one column shy of the edge so nothing ever sits in that column.
  //  2. Resize (e.g. Daintree animating the pane width on show/hide): Ink repaints
  //     SYNCHRONOUSLY on every SIGWINCH using the CURRENT React tree, but
  //     `useWindowSize` only updates `columns` a tick LATER. An explicit
  //     `width={columns - 1}` is therefore momentarily WIDER than the just-shrunk
  //     terminal, wraps, and — because Ink only auto-clears when shrinking, not when
  //     it grows back — leaves one orphaned row per show/hide cycle. So the root is
  //     sized `width="100%"`: yoga resolves that against the live terminal width on
  //     every resize relayout, never a lagged prop. `maxWidth` caps it at the
  //     readability ceiling / lagged-prop width. The live full-width children (status
  //     line, composer rules) likewise fill via flex, so they can't overflow mid-resize.
  //
  // `chromeWidth` is the reserved-gutter-shy span allowed to run the whole cockpit
  // for separators/input chrome; `contentWidth` is the readable measure for
  // prose/history cells. Keep those separate so a masthead rule can run end-to-end
  // without making transcript prose stretch across a maximized terminal. The gutter
  // (>=1, see `reservedColumns`) keeps every glyph clear of the terminal autowrap
  // column AND of a host overlay scrollbar painted over the rightmost cells.
  const gutter = Math.max(1, reservedColumns);
  const chromeWidth = Math.max(1, columns - gutter);
  const contentWidth = Math.min(chromeWidth, CONTENT_MAX);

  // Split history (committed -> Static -> native scrollback) from the live tail.
  const liveStart = liveTailStart(transcript);
  const committed = transcript.slice(0, liveStart);
  const live = transcript.slice(liveStart);

  // <Static> renders items once and prints them permanently above the live tree;
  // it only emits items appended since the last pass. `committed` is append-only
  // (it never shrinks or reorders — pull-back only ever drops the live turn), so
  // each completed turn is committed exactly once and the terminal keeps the rest.
  // The header is NOT here: it lives in the repainting region (below) so its rule
  // reflows on resize like the composer rules. The cost — accepted deliberately — is
  // that it sits just above the input instead of pinned at the very top, and once a
  // message scrolls off it keeps its commit-time width.
  const staticItems: StaticItem[] = committed.map((c) => ({ key: c.id, cell: c }));

  const contextHint = connected
    ? `agents ${dashboard.watchers.length} · tmr ${dashboard.timers.length}`
    : "MCP degraded";

  // The tier capsule only goes red when a DESTRUCTIVE (git/system) action is awaiting
  // confirmation — the moment red actually earns attention. The risk-class business
  // logic lives here, not in the pure Header, which just takes a boolean.
  const destructivePending =
    !!pending &&
    (pending.request.risk === "git" || pending.request.risk === "system");
  // The inbox is controller-filtered to actionable severities, so a non-empty inbox
  // is exactly "actionable attention pending" — the signal that promotes ^O in the
  // composer hint row.
  const attentionPending = dashboard.inbox.length > 0;

  // A confirmation is interactive and must surface in every view; while it is
  // pending the on-demand panels yield so the approval (and composer) stay live.
  const showPanel = !pending && view !== "home";

  return (
    <Box flexDirection="column" width="100%" paddingRight={gutter}>
      <Static key={staticKey} items={staticItems}>
        {(item) => (
          <CellView
            key={item.key}
            cell={item.cell}
            width={contentWidth}
            now={now}
            expanded={expanded}
          />
        )}
      </Static>

      {/* The live region (repaints): the in-flight turn and the status line are
          always shown; only the bottom slot swaps the composer for an on-demand
          operations/help panel. Nothing here is pinned across the scrollback — it
          is simply the bottom of the stream.

          INVARIANT (#138): nothing in this subtree may set a Box's layout `width`
          to a number derived from the lagged `columns` prop — yoga would size it to
          a stale value that briefly exceeds a just-shrunk terminal and orphan a
          wrapped row into scrollback. The numeric `contentWidth` handed to the
          children below is a content-budget hint for truncation math ONLY; their
          boxes size by yoga (`width="100%"` / `flexShrink`, capped with `maxWidth`)
          so a live line can never out-run the real width. The `<Static>` cells
          above are exempt — they print once and scroll away. */}
      <Box flexDirection="column">
        {live.map((cell) => (
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

        {/* The masthead lives HERE, in the repainting region, so its rule reflows on
            resize (it's a flex rule that yoga re-measures every frame) — the trade-off
            being it settles just above the input instead of pinned at the top. It sits
            below the in-flight turn so a completing turn commits to <Static> above it
            without jumping past it. */}
        <Box marginTop={1} flexDirection="column">
          <Header
            project={project}
            tier={tier}
            destructivePending={destructivePending}
            logging={logging}
            logFile={logFile}
          />
        </Box>

        {/* Cells own only their leading gap now, so the live turn no longer carries
            a bottom margin; this marginTop keeps one blank line between the
            conversation and the status chrome below it. */}
        <Box marginTop={1} flexDirection="column">
          <StatusLine
            dashboard={dashboard}
            sessionUsage={sessionUsage}
            width={contentWidth}
            now={now}
          />
        </Box>

        {/* Breathing room between the conversation and the input, the way other
            conversational CLIs sit the prompt off the content above it. */}
        <Box height={1} flexShrink={0} />

        {showPanel && view === "operations" ? (
          <Box flexDirection="column">
            <OperationsView
              dashboard={dashboard}
              previews={previews}
              width={contentWidth}
              now={now}
              activePanel={activePanel === "help" ? null : activePanel}
            />
            <Text dimColor>Esc to return</Text>
          </Box>
        ) : showPanel && view === "help" ? (
          <Box flexDirection="column">
            <HelpOverlay width={Math.min(76, contentWidth)} />
            <Text dimColor>Esc to return</Text>
          </Box>
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
      </Box>
    </Box>
  );
}
