/**
 * The control room as a PURE presentational component: everything it needs is a
 * prop, nothing is fetched or subscribed. This is what makes the surface
 * deterministically renderable — the live shell (DaintreeInkApp) feeds it from
 * the controller, the gallery feeds it from frozen fixtures, and golden-frame
 * tests feed it fixed timestamps.
 *
 * One intentional home layout: a vertical ledger with a fixed composer. Wider
 * terminals get longer lines, not side rails. Operational detail is still
 * available as a full-screen view, but the default surface stays Claude/Codex
 * shaped: startup block at the top, history below, prompt at the bottom.
 */
import { Box } from "ink";
import type { DashboardState, PendingConfirm, TranscriptCell } from "./types.js";
import type { TerminalPreview } from "./hooks/useTerminalPreview.js";
import { Transcript } from "./components/Transcript.js";
import { OperationsView } from "./components/OperationsView.js";
import { StatusLine } from "./components/StatusLine.js";
import { Composer } from "./components/Composer.js";
import { ApprovalSheet } from "./components/ApprovalSheet.js";
import { HelpOverlay } from "./components/HelpOverlay.js";
import { buildAgentRows } from "./presentation/operations.js";

export type LayoutMode = "sidebar" | "standard" | "wide";
export type View = "home" | "operations" | "help";

export function layoutFor(columns: number): LayoutMode {
  if (columns >= 116) return "wide";
  if (columns >= 72) return "standard";
  return "sidebar";
}

/**
 * Width bands inside sidebar mode. The design is optimized for "comfortable"
 * (55–65); below that is survival (fewer labels/previews), above it is the same
 * layout with slightly richer text.
 */
export type SidebarDensity = "compact" | "comfortable" | "roomy";
export function sidebarDensity(columns: number): SidebarDensity {
  if (columns < 55) return "compact";
  if (columns <= 65) return "comfortable";
  return "roomy";
}

export interface ControlRoomProps {
  project: string;
  tier: string;
  columns: number;
  rows: number;
  connected: boolean;
  transcript: TranscriptCell[];
  dashboard: DashboardState;
  previews?: TerminalPreview[];
  busy: boolean;
  stage: string;
  view: View;
  expanded?: boolean;
  pending?: PendingConfirm | null;
  /** Rendered-line offset into history. 0 means pinned to latest. */
  scrollOffset?: number;
  /** Frozen clock for deterministic rendering; defaults to live time. */
  now?: number;
  /** Debug logging is active — shown as a header badge so it's verifiable. */
  logging?: boolean;
  /** Path of the active debug log, shown under the header so it can be tailed. */
  logFile?: string;
  composerFocus?: boolean;
  onSubmit?: (value: string) => void | Promise<void>;
  onResolve?: (approved: boolean) => void;
}

export function ControlRoom({
  project,
  tier,
  columns: outerColumns,
  rows: outerRows,
  connected,
  transcript,
  dashboard,
  previews = [],
  busy,
  stage,
  view,
  expanded = false,
  pending = null,
  scrollOffset = 0,
  now = Date.now(),
  logging = false,
  logFile,
  composerFocus = false,
  onSubmit = () => {},
  onResolve = () => {},
}: ControlRoomProps) {
  // One cell of breathing room around the whole surface, the way other CLIs sit
  // a little inside the terminal edge. The interior dimensions below are the
  // actual ledger/composer budget.
  const columns = Math.max(1, outerColumns - 2);
  const rows = Math.max(1, outerRows - 2);
  const agents = buildAgentRows(dashboard.watchers, previews);
  const runs = dashboard.workflowRuns ?? [];

  // Composer is 4 rows: top rule, input, bottom rule, hint footer.
  const composerH = 4;
  const statusH = 1;
  const approvalH = pending ? 8 : 0;
  const bodyHeight = Math.max(3, rows - composerH - statusH - approvalH);

  const contextHint = connected
    ? columns < 64
      ? `${runs.length}r · ${agents.length}a · ${dashboard.timers.length}t`
      : `${runs.length} run${runs.length === 1 ? "" : "s"} · ${agents.length} agent${agents.length === 1 ? "" : "s"} · ${dashboard.timers.length} timer${dashboard.timers.length === 1 ? "" : "s"}`
    : "MCP degraded";

  return (
    <Box
      flexDirection="column"
      height={outerRows}
      width={outerColumns}
      paddingX={1}
      paddingY={1}
    >
      <Box height={bodyHeight} flexDirection="column" overflow="hidden">
        {view === "help" ? (
          <Box height={bodyHeight}>
            <HelpOverlay width={Math.min(76, columns)} />
          </Box>
        ) : view === "operations" ? (
          <Box height={bodyHeight} overflow="hidden">
            <OperationsView
              dashboard={dashboard}
              previews={previews}
              width={columns}
              now={now}
            />
          </Box>
        ) : (
          <Transcript
            cells={transcript}
            height={bodyHeight}
            width={columns}
            now={now}
            expanded={expanded}
            scrollOffset={scrollOffset}
            intro={{
              project,
              tier,
              connected,
              dashboard,
              previews,
              busy,
              stage,
              logging,
              logFile,
            }}
          />
        )}
      </Box>

      {pending ? (
        <ApprovalSheet
          pending={pending}
          width={Math.min(80, columns)}
          onResolve={onResolve}
        />
      ) : null}

      <StatusLine
        dashboard={dashboard}
        tier={tier}
        width={columns}
        now={now}
        scrollOffset={scrollOffset}
      />

      <Composer
        busy={busy}
        stage={stage}
        contextHint={contextHint}
        width={columns}
        focus={composerFocus}
        onSubmit={onSubmit}
      />
    </Box>
  );
}
