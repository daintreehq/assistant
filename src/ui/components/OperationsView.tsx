/**
 * The operations surface — Daintree's product differentiator. Five sections in
 * strict human-priority order: NOW, ATTENTION, AGENTS, SCHEDULED, RECENT. Space
 * is given to urgency: attention shows every urgent item with its title and
 * recommended actions; empty sections disappear entirely; audit is the last,
 * lowest-priority detail. Watchers and terminals are unified into AGENT rows.
 *
 * OpenTUI port: Ink `<Box>`/`<Text>` become `<box>`/`<text>`; `color=`→`fg=`,
 * `dimColor`→`attributes={TextAttributes.DIM}`, `wrap="truncate"`→`truncate`. The
 * structural wrinkle: a native `<text>` may NOT contain another `<text>`, but the
 * Ink source nested `<StateBadge>`/`<EpistemicTag>` (each its own `<text>`) inline
 * with a dim run. Those inline composites become a one-row `<box flexDirection="row">`
 * whose siblings are the badge `<text>` and the dim `<text>` — same single visible
 * line, just expressed as a flex row instead of an Ink inline text concatenation.
 * Pure inline runs of a single `<text>` use `<span>` children.
 */
import { TextAttributes } from "@opentui/core";
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
 *
 * Emitted as a `<span>` (not a `<text>`) so it can sit inside a parent `<text>`
 * line — a native `<text>` may not contain another `<text>`.
 */
function EpistemicTag({ kind }: { kind?: string }) {
  const mark = epistemicMark(kind);
  if (!mark) return null;
  return (
    <span fg={mark.color}>
      {mark.symbol} {mark.label}{" "}
    </span>
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
    <box flexDirection="column">
      <SectionLabel>Now</SectionLabel>
      {active ? (
        <box flexDirection="column">
          <box flexDirection="row" justifyContent="space-between">
            {/* truncate so the left content yields rather than widening this
                space-between row past the live terminal during a pane resize (#138);
                `width` here is a lagged content-budget hint, not the live width.
                The badge + goal were one Ink inline `<Text>`; with the badge being
                its own `<text>`, they become a flex row of two `<text>` siblings. */}
            <box flexDirection="row">
              <StateBadge tone={active.badge.tone} label={active.badge.label} />
              <text attributes={TextAttributes.DIM} truncate>
                {" "}
                {truncate(active.goal || active.title, width - 20)}
              </text>
            </box>
            <text attributes={TextAttributes.DIM}>
              {formatDuration(Math.max(0, now - active.startedAt))}
            </text>
          </box>
          <text attributes={TextAttributes.DIM} truncate>
            {"  "}
            {active.id}
            {active.preview ? ` · ${truncate(active.preview, width - active.id.length - 6)}` : ""}
          </text>
        </box>
      ) : (
        <text attributes={TextAttributes.DIM}>Standing by</text>
      )}
    </box>
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
    <box flexDirection="column">
      <SectionLabel>Needs attention</SectionLabel>
      {events.map((e) => {
        const tone = severityTone(e.severity);
        return (
          <box key={e.id} flexDirection="column" marginBottom={1}>
            <text fg={toneColor(tone)} truncate>
              {set.attention} {truncate(e.title, width - 4)}
              {e.count > 1 ? (
                <span attributes={TextAttributes.DIM}> ×{e.count}</span>
              ) : null}
            </text>
            {e.summary ? (
              <text attributes={TextAttributes.DIM} truncate>
                {"  "}
                <EpistemicTag kind={e.epistemicKind} />
                {truncate(e.summary, width - 4)}
              </text>
            ) : null}
          </box>
        );
      })}
    </box>
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
    <box flexDirection="column">
      <SectionLabel>Agents</SectionLabel>
      {shown.map((a) => (
        <box key={a.watcherId} flexDirection="column">
          <box flexDirection="row" justifyContent="space-between">
            {/* Badge + title: badge is its own `<text>`, so this is a flex row of
                two `<text>` siblings rather than an Ink inline concatenation. */}
            <box flexDirection="row">
              <StateBadge tone={a.badge.tone} label={a.badge.label} />
              <text attributes={TextAttributes.DIM} truncate>
                {" "}
                {truncate(a.title, width - 22)}
              </text>
            </box>
            <text attributes={TextAttributes.DIM}>
              {formatDuration(Math.max(0, now - a.startedAt))}
            </text>
          </box>
          <text attributes={TextAttributes.DIM} truncate>
            {"  "}
            <EpistemicTag kind={a.epistemicKind} />
            {a.id}
            {a.agentState ? ` · ${a.agentState}` : ""}
            {a.preview ? ` · ${truncate(a.preview, Math.max(8, width - 30))}` : ""}
          </text>
        </box>
      ))}
      {agents.length > shown.length ? (
        <text attributes={TextAttributes.DIM}>  +{agents.length - shown.length} more</text>
      ) : null}
    </box>
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
    <box flexDirection="column">
      <SectionLabel>Scheduled</SectionLabel>
      {timers.slice(0, max).map((t) => (
        <text key={t.id} truncate>
          <span fg={ui.color.accent}>{clock(t.fireAt)}</span>{" "}
          <span attributes={TextAttributes.DIM}>{truncate(t.title, width - 10)}</span>
        </text>
      ))}
    </box>
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
    <box flexDirection="column">
      <SectionLabel>Recent</SectionLabel>
      {audit.slice(0, max).map((r) => {
        const ok = r.outcome === "ok" || r.outcome === "grant_ok";
        return (
          <text key={r.id} truncate>
            <span fg={ok ? ui.color.accent : ui.color.danger}>
              {ok ? set.done : set.failed}
            </span>{" "}
            <span attributes={TextAttributes.DIM}>{truncate(r.toolName, width - 14)}</span>{" "}
            <span attributes={TextAttributes.DIM}>{r.durationMs}ms</span>
          </text>
        );
      })}
    </box>
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
   * command lands on what it named instead of the whole deck. There is no native
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
      <box flexDirection="column" gap={1}>
        {empty ? <text attributes={TextAttributes.DIM}>Nothing here yet.</text> : node}
      </box>
    );
  }

  return (
    <box flexDirection="column" gap={1}>
      <NowSection agents={agents} now={now} width={width} />
      <AttentionSection events={dashboard.inbox} width={width} />
      <AgentsSection agents={agents} now={now} width={width} />
      <ScheduledSection timers={dashboard.timers} width={width} />
      <RecentSection audit={dashboard.audit} width={width} />
    </box>
  );
}
