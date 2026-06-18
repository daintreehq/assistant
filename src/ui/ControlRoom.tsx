/**
 * The control room as a PURE presentational component: everything it needs is a
 * prop, nothing is fetched or subscribed. This is what makes the surface
 * deterministically renderable — the live shell (DaintreeInkApp) feeds it from
 * the controller, the gallery feeds it from frozen fixtures, and golden-frame
 * tests feed it fixed timestamps.
 *
 * One intentional layout: a vertical ledger with a fixed composer. Wider
 * terminals get longer lines, not side rails. The surface stays Claude/Codex
 * shaped: startup block at the top, history below, prompt at the bottom.
 * Operational detail (`^O`, a `/panel` command) prints inline into the same
 * stream rather than taking over a full screen — there's no alternate buffer to
 * take over, the host terminal owns scrollback.
 */
import { Box } from "ink";
import type { DashboardState, PendingConfirm, TranscriptCell } from "./types.js";
import type { TerminalPreview } from "./hooks/useTerminalPreview.js";
import { Transcript } from "./components/Transcript.js";
import { StatusLine } from "./components/StatusLine.js";
import { Composer } from "./components/Composer.js";
import { ApprovalSheet } from "./components/ApprovalSheet.js";
import { buildAgentRows } from "./presentation/operations.js";

export type LayoutMode = "sidebar" | "standard" | "wide";

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
  expanded?: boolean;
  pending?: PendingConfirm | null;
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
  expanded = false,
  pending = null,
  now = Date.now(),
  logging = false,
  logFile,
  composerFocus = false,
  onSubmit = () => {},
  onResolve = () => {},
}: ControlRoomProps) {
  // One cell of breathing room around the whole surface, the way other CLIs sit
  // a little inside the terminal edge. The interior width is the ledger budget.
  const columns = Math.max(1, outerColumns - 2);
  const agents = buildAgentRows(dashboard.watchers, previews);
  const runs = dashboard.workflowRuns ?? [];

  // Finished turns commit to the terminal's own scrollback via <Static>, but the
  // LIVE region (in-flight turn + footer) repaints in place. Ink falls back to a
  // full clearTerminal — which wipes the user's scrollback (ESC[3J) and re-dumps
  // all static history every frame — the moment that repainting frame exceeds
  // the viewport (ink shouldClearTerminalForFrame). So the live tail must stay
  // shorter than the rows left after the footer; the clipped-off top isn't lost,
  // the whole turn re-renders in full once it commits to <Static>.
  //
  // We deliberately OVER-reserve chrome: under-reserving risks the scrollback
  // wipe, over-reserving just clips a few more live lines (harmless — they
  // commit in full on settle). The composer's slash palette (+6 rows) can't
  // co-occur with a live tail (it needs focus, which requires !busy, but a live
  // tail means a turn is in flight), so 4 covers it. The approval sheet can be
  // taller than its collapsed 8 rows once args are expanded AND it shows while a
  // tool-call turn streams, so we reserve generously for it.
  const composerH = 4; // top rule + input + bottom rule + hint footer
  const statusH = 1;
  const approvalH = pending ? 12 : 0; // collapsed sheet ~8 + headroom for expanded args
  const liveHeight = Math.max(3, outerRows - composerH - statusH - approvalH - 2);

  const contextHint = connected
    ? columns < 64
      ? `${runs.length}r · ${agents.length}a · ${dashboard.timers.length}t`
      : `${runs.length} run${runs.length === 1 ? "" : "s"} · ${agents.length} agent${agents.length === 1 ? "" : "s"} · ${dashboard.timers.length} timer${dashboard.timers.length === 1 ? "" : "s"}`
    : "MCP degraded";

  return (
    <Box flexDirection="column" paddingX={1}>
      <Transcript
        cells={transcript}
        width={columns}
        now={now}
        expanded={expanded}
        liveHeight={liveHeight}
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
