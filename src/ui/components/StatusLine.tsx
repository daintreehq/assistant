/**
 * A single status line that prefers CURRENT STATE over inventory counts. Left:
 * what Daintree is doing right now (the active agent, or "Standing by"). Right: a
 * compact rollup — context pressure, session cost, active model, attention count,
 * agent count, and the live MCP badge. The attention chip and a high context
 * gauge are the only saturated tokens; everything else stays dim.
 *
 *   ◌ WORKING term_8 · tests running 18s   CTX 42% · $0.012 · !1 · agents 2 · MCP
 *   Standing by                            CTX 8% · $0.004 · minimax-m3 · MCP
 */
import { Box, Text } from "ink";
import type { DashboardState, SessionUsage } from "../types.js";
import { StateBadge, formatDuration } from "../primitives.js";
import { severityTone, toneColor, tierShort, ui } from "../theme.js";
import { truncate } from "../../utils/text.js";
import { buildAgentRows } from "../presentation/operations.js";

/** Format a running session cost compactly, keeping small amounts legible. */
function formatCost(cost: number): string {
  if (cost <= 0) return "$0.000";
  if (cost < 0.01) return `$${cost.toFixed(4)}`;
  if (cost < 1) return `$${cost.toFixed(3)}`;
  return `$${cost.toFixed(2)}`;
}

export function StatusLine({
  dashboard,
  tier,
  sessionUsage,
  width = 80,
  now = Date.now(),
}: {
  dashboard: DashboardState;
  tier?: string;
  sessionUsage?: SessionUsage;
  width?: number;
  now?: number;
}) {
  const agents = buildAgentRows(dashboard.watchers);
  const active =
    agents.find((a) => a.classification === "still_working") ?? agents[0];
  const attention = dashboard.inbox.length;
  const connected = dashboard.mcp.connected;
  const topSev = dashboard.inbox[0]?.severity ?? "attention";

  // Context pressure: latest estimate over the auto-compact threshold. Only shown
  // once a usage event has arrived (threshold > 0). Dim by default; amber as it
  // nears the threshold, red once compaction is imminent, so a glance reads load.
  const pressure =
    sessionUsage && sessionUsage.contextThreshold > 0
      ? Math.round(
          (sessionUsage.contextTokens / sessionUsage.contextThreshold) * 100,
        )
      : undefined;
  const pressureColor =
    pressure === undefined
      ? undefined
      : pressure >= 90
        ? ui.color.danger
        : pressure >= 75
          ? ui.color.warning
          : undefined;
  const cost = sessionUsage?.costUsd;
  const model = sessionUsage?.lastModel;
  // The model id is the longest right-hand token, so only show it when the line is
  // wide enough to carry it without crowding the active-agent text on the left.
  const showModel = width >= 62 && !!model;

  const ctxText = pressure !== undefined ? `CTX ${pressure}%` : "";
  const costText = cost !== undefined ? formatCost(cost) : "";
  const modelText = showModel && model ? model : "";
  // Keep the permission tier visible at all times — including during active runs,
  // when the left side carries agent context instead of the idle "Standing by"
  // label. The `system` tier (git/system powers unlocked) gets a saturated danger
  // color so the elevated risk reads at a glance; lower tiers stay dim.
  const tierText = tier ? tierShort(tier) : "";

  // Reserve room for the right-hand rollup so the line never wraps to 2 rows
  // (which would overflow the fixed-height shell and overlap the row above). Each
  // visible segment costs its text plus a 3-char " · " separator.
  const seg = (s: string) => (s ? s.length + 3 : 0);
  const rightLen =
    seg(ctxText) +
    seg(costText) +
    seg(modelText) +
    seg(tierText) +
    (attention > 0 ? 6 : 0) +
    (agents.length > 0 ? 10 : 0) +
    10;
  const leftRoom = Math.max(12, width - rightLen);

  return (
    // Fill the parent's LIVE width (`width="100%"`) rather than an explicit number:
    // yoga resolves it against the real terminal on every resize relayout, so the
    // space-between line can't momentarily exceed a just-shrunk terminal and orphan a
    // wrapped row into scrollback during a pane show/hide. The `width` prop still
    // drives the right-hand reservation math above; the left side is `wrap="truncate"`,
    // so even if that estimate lags a resize the line never actually overflows.
    <Box width="100%" justifyContent="space-between">
      <Box>
        {active ? (
          <Text wrap="truncate">
            <StateBadge tone={active.badge.tone} label={active.badge.label} />
            <Text dimColor>
              {" "}
              {active.id} ·{" "}
              {truncate(active.goal || active.title, Math.max(6, leftRoom - active.id.length - 14))}{" "}
              {formatDuration(Math.max(0, now - active.startedAt))}
            </Text>
          </Text>
        ) : (
          <Text dimColor wrap="truncate">
            Standing by{tier ? ` · ${tier.toUpperCase()}` : ""}
          </Text>
        )}
      </Box>
      <Box>
        {ctxText ? (
          <Text color={pressureColor} dimColor={pressureColor === undefined}>
            {ctxText}
            <Text dimColor> · </Text>
          </Text>
        ) : null}
        {costText ? <Text dimColor>{costText} · </Text> : null}
        {modelText ? <Text dimColor>{modelText} · </Text> : null}
        {attention > 0 ? (
          <Text color={toneColor(severityTone(topSev))}>
            !{attention}
            <Text dimColor> · </Text>
          </Text>
        ) : null}
        {agents.length > 0 ? (
          <Text dimColor>agents {agents.length} · </Text>
        ) : null}
        {tierText ? (
          <Text
            color={tier === "system" ? ui.color.danger : undefined}
            dimColor={tier !== "system"}
          >
            {tierText}
            <Text dimColor> · </Text>
          </Text>
        ) : null}
        {connected ? (
          <Text color={ui.color.accent}>MCP</Text>
        ) : (
          <Text color={ui.color.warning}>DEGRADED</Text>
        )}
      </Box>
    </Box>
  );
}
