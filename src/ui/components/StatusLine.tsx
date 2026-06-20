/**
 * A compact status line that speaks ONLY when it has something to say. It carries
 * CURRENT STATE — the active agent and the smallest useful live rollup (context
 * pressure, session cost/model while idle, an attention count, the agent count) —
 * and nothing else. There is no idle "Standing by" label (silence already means
 * idle) and no steady-state "MCP" badge (a healthy link is already confirmed by the
 * startup banner); the connection only surfaces here as `DEGRADED`, by exception.
 * The permission tier lives in the masthead now, not here. When idle with no signal
 * to report the component renders nothing at all.
 *
 *   ◌ WORKING term_8 · tests running 18s · CTX 42% · agents 2
 *   CTX 8% · $0.004 · minimax-m3
 *   (idle, nothing to report → empty)
 */
import { Box, Text } from "ink";
import { Fragment, type ReactNode } from "react";
import type { DashboardState, SessionUsage } from "../types.js";
import { StateBadge, formatDuration } from "../primitives.js";
import { severityTone, toneColor, topSeverity, ui } from "../theme.js";
import { truncate } from "../../utils/text.js";
import { buildAgentRows } from "../presentation/operations.js";
import { LIVE_CHROME_MAX_WIDTH } from "../liveChrome.js";

/** Format a running session cost compactly, keeping small amounts legible. */
function formatCost(cost: number): string {
  if (cost <= 0) return "$0.000";
  if (cost < 0.01) return `$${cost.toFixed(4)}`;
  if (cost < 1) return `$${cost.toFixed(3)}`;
  return `$${cost.toFixed(2)}`;
}

export function StatusLine({
  dashboard,
  sessionUsage,
  width = 80,
  now = Date.now(),
}: {
  dashboard: DashboardState;
  sessionUsage?: SessionUsage;
  width?: number;
  now?: number;
}) {
  const agents = buildAgentRows(dashboard.watchers);
  const active =
    agents.find((a) => a.classification === "still_working") ?? agents[0];
  // The inbox is already filtered to actionable severities (>= attention) at the
  // controller boundary, so its length IS the actionable count — debug/info/done
  // never reach here. Derive the chip color from the worst item explicitly via
  // topSeverity() rather than trusting inbox[0]'s implicit DB ordering, so the
  // "most urgent wins" contract is visible at the callsite, not a hidden invariant.
  const attention = dashboard.inbox.length;
  const connected = dashboard.mcp.connected;
  const topSev = topSeverity(dashboard.inbox) ?? "attention";

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
  const chromeWidth = Math.min(width, LIVE_CHROME_MAX_WIDTH);
  const activeDuration = active
    ? formatDuration(Math.max(0, now - active.startedAt))
    : "";
  // The model id is the longest idle token. It only ever renders while idle (see
  // showCostModel), so it never competes with active-agent text — the width gate is
  // purely a terseness rule: keep narrow panes to the gauge/cost and drop the long
  // model id even when it would technically fit the 56-col chrome.
  const showModel = width >= 62 && !!model;
  // Cost and model are idle-only context: during an active run the left side
  // carries the agent and the right side stays terse.
  const showCostModel = !active;

  const ctxText = pressure !== undefined ? `CTX ${pressure}%` : "";
  const costText = showCostModel && cost !== undefined ? formatCost(cost) : "";

  // Reserve room for the compact rollup so the line never wraps to 2 rows (which
  // would overflow the fixed-height shell and overlap the row above). Each visible
  // segment conservatively costs its text plus a 3-char " · " separator — slightly
  // over-reserving, which only ever drops an OPTIONAL token (model/agents) early.
  const seg = (s: string) => (s ? s.length + 3 : 0);
  const attentionLen = attention > 0 ? 4 + String(attention).length : 0;
  const agentsLen =
    agents.length > 0 ? 3 + "agents ".length + String(agents.length).length : 0;
  // The connection only ever costs width when it is DOWN (the by-exception badge).
  const degradedLen = connected ? 0 : 3 + "DEGRADED".length;
  const modelText =
    showCostModel &&
    showModel &&
    model &&
    seg(ctxText) + seg(costText) + seg(model) + attentionLen + degradedLen <=
      chromeWidth
      ? model
      : "";
  const activeBadgeLen = active
    ? 2 + active.badge.label.toUpperCase().length
    : 0;
  const activeFixedLen = active
    ? activeBadgeLen +
      1 +
      active.id.length +
      (activeDuration ? 1 + activeDuration.length : 0)
    : 0;
  const requiredRollupLen = seg(ctxText) + attentionLen + degradedLen;
  const showAgents =
    agents.length > 0 &&
    (!active || activeFixedLen + requiredRollupLen + agentsLen <= chromeWidth);
  const visibleAgentsLen = showAgents ? agentsLen : 0;
  const rightLen =
    seg(ctxText) +
    seg(costText) +
    seg(modelText) +
    attentionLen +
    visibleAgentsLen +
    degradedLen;
  const activeGoalRoom = active
    ? chromeWidth - rightLen - activeFixedLen - 3
    : 0;
  const activeGoal =
    active && activeGoalRoom >= 6
      ? truncate(active.goal || active.title, activeGoalRoom)
      : "";

  // Build the visible segments, then join them with " · " separators. A segment
  // array (rather than hand-threaded separators) means the FIRST visible token
  // never carries a dangling leading "·", regardless of which tokens are present.
  const parts: ReactNode[] = [];
  if (active) {
    parts.push(
      <Fragment key="active">
        <StateBadge tone={active.badge.tone} label={active.badge.label} />
        <Text dimColor>
          {" "}
          {active.id}
          {activeGoal ? ` · ${activeGoal}` : ""}
          {activeDuration ? ` ${activeDuration}` : ""}
        </Text>
      </Fragment>,
    );
  }
  if (ctxText) {
    parts.push(
      <Text key="ctx" color={pressureColor} dimColor={pressureColor === undefined}>
        {ctxText}
      </Text>,
    );
  }
  if (costText) {
    parts.push(
      <Text key="cost" dimColor>
        {costText}
      </Text>,
    );
  }
  if (modelText) {
    parts.push(
      <Text key="model" dimColor>
        {modelText}
      </Text>,
    );
  }
  if (attention > 0) {
    parts.push(
      <Text key="att" color={toneColor(severityTone(topSev))}>
        !{attention}
      </Text>,
    );
  }
  if (showAgents) {
    parts.push(
      <Text key="agents" dimColor>
        agents {agents.length}
      </Text>,
    );
  }
  if (!connected) {
    parts.push(
      <Text key="deg" color={ui.color.warning}>
        DEGRADED
      </Text>,
    );
  }

  // Idle with nothing to report: render nothing rather than a noisy placeholder.
  if (parts.length === 0) return null;

  return (
    // Keep the repainting status row short. A full-width `space-between` row is
    // visually tidy, but on terminal shrink the OLD wide row reflows before Ink's
    // erase pass and the top physical row can survive as a duplicate status line.
    <Box width="100%" maxWidth={LIVE_CHROME_MAX_WIDTH}>
      <Text wrap="truncate">
        {parts.map((p, i) => (
          <Fragment key={i}>
            {i > 0 ? <Text dimColor> · </Text> : null}
            {p}
          </Fragment>
        ))}
      </Text>
    </Box>
  );
}
