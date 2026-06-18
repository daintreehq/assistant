/**
 * The control room as a PURE presentational component: everything it needs is a
 * prop, nothing is fetched or subscribed. This is what makes the surface
 * deterministically renderable — the live shell (DaintreeInkApp) feeds it from
 * the controller, the gallery feeds it from frozen fixtures, and golden-frame
 * tests feed it fixed timestamps.
 *
 * Three intentional layouts chosen by width. The 55–65 column SIDEBAR is the
 * canonical Daintree surface (it usually lives in a host side panel), so it is
 * operations-first with conversation integrated; standard/wide are progressive
 * enhancements that hand more room to the transcript. Chrome is budgeted so the
 * operations sections and composer are never pushed off a short terminal.
 */
import { Box, Text } from "ink";
import type { DashboardState, PendingConfirm, TranscriptCell } from "./types.js";
import type { TerminalPreview } from "./hooks/useTerminalPreview.js";
import { Header } from "./components/Header.js";
import { Transcript } from "./components/Transcript.js";
import { SidebarHome } from "./components/SidebarHome.js";
import { OperationsView } from "./components/OperationsView.js";
import { OpsRail } from "./components/OpsRail.js";
import { StatusLine } from "./components/StatusLine.js";
import { AttentionBanner } from "./components/AttentionBanner.js";
import { Composer } from "./components/Composer.js";
import { ApprovalSheet } from "./components/ApprovalSheet.js";
import { HelpOverlay } from "./components/HelpOverlay.js";
import { StateBadge, formatDuration } from "./primitives.js";
import { buildAgentRows } from "./presentation/operations.js";
import { truncate } from "../utils/text.js";

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
  /** Frozen clock for deterministic rendering; defaults to live time. */
  now?: number;
  /** Debug logging is active — shown as a header badge so it's verifiable. */
  logging?: boolean;
  /** Path of the active debug log, shown under the header so it can be tailed. */
  logFile?: string;
  composerFocus?: boolean;
  onSubmit?: (value: string) => boolean | void | Promise<void>;
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
  now = Date.now(),
  logging = false,
  logFile,
  composerFocus = false,
  onSubmit = () => {},
  onResolve = () => {},
}: ControlRoomProps) {
  // One cell of breathing room around the whole surface, the way other CLIs
  // sit a little inside the terminal edge. The outer padding consumes one
  // column/row on each edge, so the interior lays out at the host size minus
  // two in each axis — every width/height calculation below uses these
  // interior dimensions.
  const columns = Math.max(1, outerColumns - 2);
  const rows = Math.max(1, outerRows - 2);
  const layout = layoutFor(columns);
  const agents = buildAgentRows(dashboard.watchers, previews);
  const activeAgent =
    agents.find((a) => a.classification === "still_working") ?? agents[0];

  // Sidebar home owns its own attention + current-operation rows (SidebarHome);
  // wide shows them in the rail. Only standard layout gets the bottom banner and
  // the one-line operation strip below the header.
  const showAttention =
    !pending && layout === "standard" && dashboard.inbox.length > 0 && view === "home";
  // Sidebar's NOW section already names the active run, so the header subtitle
  // is reserved for standard layout (which has no NOW section).
  const runTitle =
    busy && view === "home" && layout === "standard"
      ? activeAgent?.goal ?? undefined
      : undefined;

  // Header = identity bar (1) + marginBottom (1), plus an optional run subtitle
  // and an optional one-row debug-log line (badge + truncated path); each grows
  // the chrome by one row, so the body budget below must subtract them.
  const headerH = 2 + (runTitle ? 1 : 0) + (logging ? 1 : 0);
  const composerH = 3;
  const statusH = 1;
  const attentionH = showAttention ? 1 : 0;
  const opStripH = layout === "standard" && view === "home" && activeAgent ? 1 : 0;
  const approvalH = pending ? 8 : 0;
  const bodyHeight = Math.max(
    3,
    rows - headerH - composerH - statusH - attentionH - opStripH - approvalH,
  );

  const railWidth =
    layout === "wide"
      ? Math.min(40, Math.max(26, Math.floor(columns * 0.28)))
      : 0;
  const transcriptWidth =
    layout === "wide" ? Math.max(40, columns - railWidth - 1) : columns;

  const contextHint = connected
    ? `agents ${agents.length} · tmr ${dashboard.timers.length}`
    : "MCP degraded";

  return (
    <Box
      flexDirection="column"
      height={outerRows}
      width={outerColumns}
      paddingX={1}
      paddingY={1}
    >
      <Header
        project={project}
        tier={tier}
        connected={connected}
        runTitle={runTitle}
        logging={logging}
        logFile={logFile}
      />

      {opStripH > 0 && activeAgent ? (
        <Box justifyContent="space-between">
          <Text wrap="truncate">
            <StateBadge
              tone={activeAgent.badge.tone}
              label={activeAgent.badge.label}
            />
            <Text dimColor>
              {" "}
              {activeAgent.id} ·{" "}
              {truncate(activeAgent.goal || activeAgent.title, Math.max(8, columns - 30))}
            </Text>
          </Text>
          <Text dimColor>
            {formatDuration(Math.max(0, now - activeAgent.startedAt))}
          </Text>
        </Box>
      ) : null}

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
        ) : layout === "wide" ? (
          <Box height={bodyHeight}>
            <Box width={transcriptWidth} overflow="hidden">
              <Transcript
                cells={transcript}
                height={bodyHeight}
                width={transcriptWidth}
                now={now}
                expanded={expanded}
              />
            </Box>
            <Box width={railWidth} marginLeft={1}>
              <OpsRail
                dashboard={dashboard}
                previews={previews}
                width={railWidth}
                now={now}
              />
            </Box>
          </Box>
        ) : layout === "sidebar" ? (
          <SidebarHome
            dashboard={dashboard}
            previews={previews}
            transcript={transcript}
            width={columns}
            height={bodyHeight}
            now={now}
            expanded={expanded}
          />
        ) : (
          <Transcript
            cells={transcript}
            height={bodyHeight}
            width={columns}
            now={now}
            expanded={expanded}
          />
        )}
      </Box>

      {showAttention ? (
        <AttentionBanner events={dashboard.inbox} width={columns} />
      ) : null}

      {pending ? (
        <ApprovalSheet
          pending={pending}
          width={Math.min(80, columns)}
          onResolve={onResolve}
        />
      ) : null}

      <StatusLine dashboard={dashboard} tier={tier} width={columns} now={now} />

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
