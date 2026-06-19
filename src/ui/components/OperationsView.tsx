/**
 * The operations surface — Daintree's product differentiator. Five sections in
 * strict human-priority order: NOW, ATTENTION, AGENTS, SCHEDULED, RECENT. Space
 * is given to urgency: attention shows every urgent item with its title and
 * recommended actions; empty sections disappear entirely; audit is the last,
 * lowest-priority detail. Watchers and terminals are unified into AGENT rows.
 */
import { Box, Text } from "ink";
import type { DashboardState } from "../types.js";
import type { TerminalPreview } from "../hooks/useTerminalPreview.js";
import type { AuditRecord, QueueEvent, TimerRecord } from "../../schemas.js";
import { SectionLabel, StateBadge, formatDuration } from "../primitives.js";
import {
  epistemicMark,
  glyphs,
  severityTone,
  toneColor,
  ui,
} from "../theme.js";
import { truncate } from "../../utils/text.js";
import {
  buildAgentRows,
  type AgentRow,
} from "../presentation/operations.js";
import type { PanelKey } from "../../cli/commandData.js";

/**
 * A compact epistemic-provenance mark (issue #85): a colored glyph + 3-letter tag
 * telling the user whether a row's state is an observed fact, a model inference, or
 * unverified. Renders nothing when the kind is absent so legacy/non-watcher rows
 * stay unchanged. Carries its own color, so it stays legible nested inside a dim line.
 */
function EpistemicTag({ kind }: { kind?: string }) {
  const mark = epistemicMark(kind);
  if (!mark) return null;
  return (
    <Text color={mark.color}>
      {mark.symbol} {mark.label}{" "}
    </Text>
  );
}

function clock(ms: number): string {
  try {
    return new Date(ms).toLocaleTimeString([], {
      hour: "2-digit",
      minute: "2-digit",
    });
  } catch {
    return "—";
  }
}

function NowSection({
  agents,
  now,
  width,
}: {
  agents: AgentRow[];
  now: number;
  width: number;
}) {
  const active = agents.find((a) => a.classification === "still_working") ?? agents[0];
  return (
    <Box flexDirection="column">
      <SectionLabel>Now</SectionLabel>
      {active ? (
        <Box flexDirection="column">
          <Box justifyContent="space-between">
            <Text>
              <StateBadge tone={active.badge.tone} label={active.badge.label} />
              <Text dimColor> {truncate(active.goal || active.title, width - 20)}</Text>
            </Text>
            <Text dimColor>{formatDuration(Math.max(0, now - active.startedAt))}</Text>
          </Box>
          <Text dimColor>
            {"  "}
            {active.id}
            {active.preview ? ` · ${truncate(active.preview, width - active.id.length - 6)}` : ""}
          </Text>
        </Box>
      ) : (
        <Text dimColor>Standing by</Text>
      )}
    </Box>
  );
}

function AttentionSection({
  events,
  width,
}: {
  events: QueueEvent[];
  width: number;
}) {
  if (events.length === 0) return null;
  const set = glyphs();
  return (
    <Box flexDirection="column">
      <SectionLabel>Needs attention</SectionLabel>
      {events.map((e) => {
        const tone = severityTone(e.severity);
        return (
          <Box key={e.id} flexDirection="column" marginBottom={1}>
            <Text color={toneColor(tone)}>
              {set.attention} {truncate(e.title, width - 4)}
              {e.count > 1 ? <Text dimColor> ×{e.count}</Text> : null}
            </Text>
            {e.summary ? (
              <Text dimColor>
                {"  "}
                <EpistemicTag kind={e.epistemicKind} />
                {truncate(e.summary, width - 4)}
              </Text>
            ) : null}
          </Box>
        );
      })}
    </Box>
  );
}

function AgentsSection({
  agents,
  now,
  width,
  max = 6,
}: {
  agents: AgentRow[];
  now: number;
  width: number;
  max?: number;
}) {
  if (agents.length === 0) return null;
  const shown = agents.slice(0, max);
  return (
    <Box flexDirection="column">
      <SectionLabel>Agents</SectionLabel>
      {shown.map((a) => (
        <Box key={a.watcherId} flexDirection="column">
          <Box justifyContent="space-between">
            <Text>
              <StateBadge tone={a.badge.tone} label={a.badge.label} />
              <Text dimColor> {truncate(a.title, width - 22)}</Text>
            </Text>
            <Text dimColor>{formatDuration(Math.max(0, now - a.startedAt))}</Text>
          </Box>
          <Text dimColor>
            {"  "}
            <EpistemicTag kind={a.epistemicKind} />
            {a.id}
            {a.agentState ? ` · ${a.agentState}` : ""}
            {a.preview ? ` · ${truncate(a.preview, Math.max(8, width - 30))}` : ""}
          </Text>
        </Box>
      ))}
      {agents.length > shown.length ? (
        <Text dimColor>  +{agents.length - shown.length} more</Text>
      ) : null}
    </Box>
  );
}

function ScheduledSection({
  timers,
  width,
  max = 4,
}: {
  timers: TimerRecord[];
  width: number;
  max?: number;
}) {
  if (timers.length === 0) return null;
  return (
    <Box flexDirection="column">
      <SectionLabel>Scheduled</SectionLabel>
      {timers.slice(0, max).map((t) => (
        <Text key={t.id}>
          <Text color={ui.color.accent}>{clock(t.fireAt)}</Text>{" "}
          <Text dimColor>{truncate(t.title, width - 10)}</Text>
        </Text>
      ))}
    </Box>
  );
}

function RecentSection({
  audit,
  width,
  max = 5,
}: {
  audit: AuditRecord[];
  width: number;
  max?: number;
}) {
  if (audit.length === 0) return null;
  const set = glyphs();
  return (
    <Box flexDirection="column">
      <SectionLabel>Recent</SectionLabel>
      {audit.slice(0, max).map((r) => {
        const ok = r.outcome === "ok" || r.outcome === "grant_ok";
        return (
          <Text key={r.id}>
            <Text color={ok ? ui.color.accent : ui.color.danger}>
              {ok ? set.done : set.failed}
            </Text>{" "}
            <Text dimColor>{truncate(r.toolName, width - 14)}</Text>{" "}
            <Text dimColor>{r.durationMs}ms</Text>
          </Text>
        );
      })}
    </Box>
  );
}

export function OperationsView({
  dashboard,
  previews = [],
  width = 72,
  now = Date.now(),
  activePanel = null,
}: {
  dashboard: DashboardState;
  previews?: TerminalPreview[];
  width?: number;
  now?: number;
  /**
   * When a `/panel` command focuses one section, render ONLY that section so the
   * command lands on what it named instead of the whole deck. Ink has no native
   * scroll-to, so "focus" is a filter, not a viewport jump. `null` (or the `help`
   * panel, which ControlRoom renders separately) shows the full five-section deck.
   */
  activePanel?: Exclude<PanelKey, "help"> | null;
}) {
  const agents = buildAgentRows(dashboard.watchers, previews);

  if (activePanel) {
    // Each section returns null when its data is empty, so render an honest
    // "nothing here" line rather than a blank body when a focused panel is empty.
    const panels = {
      watchers: {
        node: <AgentsSection agents={agents} now={now} width={width} />,
        empty: agents.length === 0,
      },
      inbox: {
        node: <AttentionSection events={dashboard.inbox} width={width} />,
        empty: dashboard.inbox.length === 0,
      },
      timers: {
        node: <ScheduledSection timers={dashboard.timers} width={width} />,
        empty: dashboard.timers.length === 0,
      },
      audit: {
        node: <RecentSection audit={dashboard.audit} width={width} />,
        empty: dashboard.audit.length === 0,
      },
    };
    // The `Exclude<PanelKey, "help">` type makes any other key unreachable through
    // normal wiring; the fallback just keeps an out-of-contract caller from crashing.
    const { node, empty } = panels[activePanel] ?? { node: null, empty: true };
    return (
      <Box flexDirection="column" gap={1}>
        {empty ? <Text dimColor>Nothing here yet.</Text> : node}
      </Box>
    );
  }

  return (
    <Box flexDirection="column" gap={1}>
      <NowSection agents={agents} now={now} width={width} />
      <AttentionSection events={dashboard.inbox} width={width} />
      <AgentsSection agents={agents} now={now} width={width} />
      <ScheduledSection timers={dashboard.timers} width={width} />
      <RecentSection audit={dashboard.audit} width={width} />
    </Box>
  );
}
