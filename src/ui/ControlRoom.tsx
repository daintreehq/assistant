/**
 * The cockpit as a PURE presentational component: everything it needs is a prop,
 * nothing is fetched or subscribed. The live shell (DaintreeInkApp) feeds it from
 * the controller, the gallery feeds it from frozen fixtures, and tests feed it
 * fixed timestamps.
 *
 * INLINE MODEL (Claude Code style). The cockpit renders into the terminal's MAIN
 * screen buffer, not the alternate buffer, so the terminal's own scrollback /
 * mouse wheel / selection work natively. Completed turns are committed permanently
 * above the live region with Ink's <Static> (they flow into native scrollback and
 * never repaint); only the in-flight turn, the status line and the composer live
 * in the repainting region pinned at the bottom. The header is printed ONCE at the
 * top and is allowed to scroll away with the history — it is not sticky.
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

/** A <Static> item: the one-time header (no cell) or a committed transcript cell. */
type StaticItem = { key: string; cell?: TranscriptCell };

export function ControlRoom({
  project,
  tier,
  columns,
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
}: ControlRoomProps) {
  // Reserve the terminal's last column. The cockpit renders inline (main buffer,
  // no alternate screen), so the repainting region is erased and redrawn with
  // cursor moves whose count comes from Ink's own layout — NOT the terminal's
  // actual wrapping. If any dynamic line fills the full width its final glyph
  // lands in the autowrap (DECAWM) column, where many terminals wrap it onto a
  // second physical row. That row is invisible to Ink's height accounting, so on
  // the next repaint the cursor moves up one row short, the top row is never
  // erased, and a stale copy is orphaned into scrollback (the status line "ghosts"
  // we saw triplicated). Keeping content one column shy of the edge means nothing
  // ever occupies the autowrap column, so every repaint erases cleanly.
  //
  // The reserve is a HARD ceiling: clamp strictly below the terminal width even on
  // a tiny pane. A naive `Math.max(20, …)` readability floor would defeat it — at
  // columns ≤ 20 it pins contentWidth to 20, back onto (or past) the real edge,
  // reintroducing the very overflow we're avoiding. So `columns - 1` always wins;
  // the floor (1) only guards against a zero/negative width breaking Ink's layout.
  const contentWidth = Math.max(1, Math.min(columns - 1, CONTENT_MAX));

  // Split history (committed -> Static -> native scrollback) from the live tail.
  const liveStart = liveTailStart(transcript);
  const committed = transcript.slice(0, liveStart);
  const live = transcript.slice(liveStart);

  // <Static> renders items once and prints them permanently above the live tree;
  // it only emits items appended since the last pass. `committed` is append-only
  // (it never shrinks or reorders — pull-back only ever drops the live turn), so
  // each completed turn is committed exactly once and the terminal keeps the rest.
  // The header is item 0: printed once, then free to scroll away with the history.
  const staticItems: StaticItem[] = [
    { key: "__header" },
    ...committed.map((c) => ({ key: c.id, cell: c })),
  ];

  const contextHint = connected
    ? `agents ${dashboard.watchers.length} · tmr ${dashboard.timers.length}`
    : "MCP degraded";

  // A confirmation is interactive and must surface in every view; while it is
  // pending the on-demand panels yield so the approval (and composer) stay live.
  const showPanel = !pending && view !== "home";

  return (
    <Box flexDirection="column" width={contentWidth}>
      <Static items={staticItems}>
        {(item) =>
          item.cell ? (
            <CellView
              key={item.key}
              cell={item.cell}
              width={contentWidth}
              now={now}
              expanded={expanded}
            />
          ) : (
            <Header
              key="__header"
              columns={contentWidth}
              project={project}
              logging={logging}
              logFile={logFile}
            />
          )
        }
      </Static>

      {/* The live region (repaints): the in-flight turn and the status line are
          always shown; only the bottom slot swaps the composer for an on-demand
          operations/help panel. Nothing here is pinned across the scrollback — it
          is simply the bottom of the stream. */}
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

        {/* Cells own only their leading gap now, so the live turn no longer carries
            a bottom margin; this marginTop keeps one blank line between the
            conversation and the status chrome below it. */}
        <Box marginTop={1} flexDirection="column">
          <StatusLine
            dashboard={dashboard}
            tier={tier}
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
            width={contentWidth}
            focus={composerFocus}
            cancellable={cancellable}
            onSubmit={onSubmit}
            onCancel={onCancel}
            ref={composerRef}
          />
        )}
      </Box>
    </Box>
  );
}
