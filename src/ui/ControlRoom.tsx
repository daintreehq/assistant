/**
 * The control room as a PURE presentational component: everything it needs is a
 * prop, nothing is fetched or subscribed. This is what makes the surface
 * deterministically renderable — the live shell (DaintreeInkApp) feeds it from
 * the controller, the gallery feeds it from frozen fixtures, and golden-frame
 * tests feed it fixed timestamps.
 *
 * Three intentional layouts (narrow / standard / wide) chosen by width; chrome
 * is budgeted so the transcript never pushes the composer off a short terminal.
 */
import { Box, Text } from "ink";
import type { DashboardState, PendingConfirm, TranscriptCell } from "./types.js";
import type { TerminalPreview } from "./hooks/useTerminalPreview.js";
import { Header } from "./components/Header.js";
import { Transcript } from "./components/Transcript.js";
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

export type LayoutMode = "narrow" | "standard" | "wide";
export type View = "home" | "operations" | "help";

export function layoutFor(columns: number): LayoutMode {
  if (columns >= 116) return "wide";
  if (columns >= 72) return "standard";
  return "narrow";
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
  composerFocus?: boolean;
  onSubmit?: (value: string) => void | Promise<void>;
  onResolve?: (approved: boolean) => void;
}

export function ControlRoom({
  project,
  tier,
  columns,
  rows,
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
  composerFocus = false,
  onSubmit = () => {},
  onResolve = () => {},
}: ControlRoomProps) {
  const layout = layoutFor(columns);
  const agents = buildAgentRows(dashboard.watchers, previews);
  const activeAgent =
    agents.find((a) => a.classification === "still_working") ?? agents[0];

  const showAttention =
    !pending && layout !== "wide" && dashboard.inbox.length > 0 && view === "home";
  const runTitle =
    busy && view === "home" ? activeAgent?.goal ?? undefined : undefined;

  const headerH = runTitle ? 3 : 2;
  const composerH = 3;
  const statusH = 1;
  const attentionH = showAttention ? 1 : 0;
  const opStripH = layout !== "wide" && view === "home" && activeAgent ? 1 : 0;
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
    ? `${agents.length} agent${agents.length === 1 ? "" : "s"} active · MCP`
    : "MCP degraded";

  return (
    <Box flexDirection="column" height={rows} width={columns}>
      <Header
        project={project}
        tier={tier}
        connected={connected}
        runTitle={runTitle}
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
