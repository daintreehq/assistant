/**
 * The default 55–65 column surface. Daintree usually lives in a right-hand
 * sidebar, so the home screen is OPERATIONS-FIRST with conversation integrated:
 * it answers "what is running, what's watched, what's scheduled, what needs me,
 * what did I just ask" before it shows any chat history.
 *
 * Fixed vertical priority: NOW → WATCHING → TIMERS → ATTENTION → RECENT. The
 * full operations surface (recommended actions, raw ids, audit) stays one
 * keystroke away behind `^O`; this is the always-on glance.
 */
import { Box, Text } from "ink";
import type { DashboardState, TranscriptCell } from "../types.js";
import type { TerminalPreview } from "../hooks/useTerminalPreview.js";
import type { TimerRecord, QueueEvent } from "../../schemas.js";
import { SectionLabel, StateBadge, formatDuration } from "../primitives.js";
import { glyphs, severityTone, toneColor } from "../theme.js";
import { truncate } from "../../utils/text.js";
import { buildAgentRows, type AgentRow } from "../presentation/operations.js";
import { Transcript } from "./Transcript.js";

/** Wall-clock HH:MM for today, short "Jun 19" for a future day. */
function timerWhen(fireAt: number, now: number): string {
  try {
    const f = new Date(fireAt);
    const n = new Date(now);
    const sameDay =
      f.getFullYear() === n.getFullYear() &&
      f.getMonth() === n.getMonth() &&
      f.getDate() === n.getDate();
    if (sameDay) {
      return f.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    }
    return f.toLocaleDateString([], { month: "short", day: "2-digit" });
  } catch {
    return "—";
  }
}

/** Exactly one active thing — the single most important answer on the screen. */
function SidebarNow({
  active,
  now,
  width,
  density,
}: {
  active?: AgentRow;
  now: number;
  width: number;
  density: "compact" | "rich";
}) {
  return (
    <Box flexDirection="column" flexShrink={0}>
      <SectionLabel>Now</SectionLabel>
      {active ? (
        <>
          <Box justifyContent="space-between">
            <Text wrap="truncate">
              <StateBadge tone={active.badge.tone} label={active.badge.label} />
              <Text dimColor> {active.id}</Text>
            </Text>
            <Text dimColor>
              {formatDuration(Math.max(0, now - active.startedAt))}
            </Text>
          </Box>
          <Text dimColor wrap="truncate">
            {"  "}
            {truncate(active.goal || active.title, Math.max(8, width - 2))}
          </Text>
          {density === "rich" && active.preview ? (
            <Text dimColor wrap="truncate">
              {"  "}
              {truncate(active.preview, Math.max(8, width - 2))}
            </Text>
          ) : null}
        </>
      ) : (
        <Text dimColor>Standing by</Text>
      )}
    </Box>
  );
}

/** The permanent answer to "which terminals am I supervising, and how are they?" */
function SidebarWatching({
  agents,
  now,
  width,
}: {
  agents: AgentRow[];
  now: number;
  width: number;
}) {
  if (agents.length === 0) return null;
  const shown = agents.slice(0, 6);
  return (
    <Box flexDirection="column" marginTop={1} flexShrink={0}>
      <SectionLabel>Watching</SectionLabel>
      {shown.map((a) => {
        const age = formatDuration(Math.max(0, now - a.startedAt));
        const idCol = a.id.padEnd(8).slice(0, 8);
        return (
          <Box key={a.watcherId} flexDirection="column">
            <Box justifyContent="space-between">
              <Text wrap="truncate">
                <Text color={a.badge.color}>{a.badge.symbol} </Text>
                <Text dimColor>{idCol} </Text>
                <Text dimColor>
                  {truncate(
                    a.goal || a.title || a.badge.label,
                    Math.max(6, width - 20),
                  )}
                </Text>
              </Text>
              <Text dimColor>{age}</Text>
            </Box>
            {a.needsAttention ? (
              <Text color={toneColor(a.badge.tone)} wrap="truncate">
                {"  "}
                {truncate(a.badge.label, 16)}
                <Text dimColor> · focus terminal</Text>
              </Text>
            ) : null}
          </Box>
        );
      })}
      {agents.length > shown.length ? (
        <Text dimColor>{"  "}+{agents.length - shown.length} more</Text>
      ) : null}
    </Box>
  );
}

/** Scheduled intent — ambient, never alarming. */
function SidebarTimers({
  timers,
  now,
  width,
}: {
  timers: TimerRecord[];
  now: number;
  width: number;
}) {
  if (timers.length === 0) return null;
  const set = glyphs();
  const shown = [...timers].sort((a, b) => a.fireAt - b.fireAt).slice(0, 3);
  return (
    <Box flexDirection="column" marginTop={1} flexShrink={0}>
      <SectionLabel>Timers</SectionLabel>
      {shown.map((t) => (
        <Text key={t.id} wrap="truncate">
          <Text dimColor>{set.clock} </Text>
          <Text dimColor>{timerWhen(t.fireAt, now).padEnd(6)} </Text>
          {truncate(t.title, Math.max(6, width - 12))}
        </Text>
      ))}
    </Box>
  );
}

/** The actual urgent title — not just a count (that lives in the status line). */
function SidebarAttention({
  events,
  width,
}: {
  events: QueueEvent[];
  width: number;
}) {
  if (events.length === 0) return null;
  const set = glyphs();
  const top = events[0];
  const more = events.length - 1;
  const tone = severityTone(top.severity);
  return (
    <Box flexDirection="column" marginTop={1} flexShrink={0}>
      <Box justifyContent="space-between">
        <Text color={toneColor(tone)} wrap="truncate">
          {set.attention} {truncate(top.title, Math.max(8, width - 16))}
          {more > 0 ? <Text dimColor> · {more} more</Text> : null}
        </Text>
        <Text dimColor>^O inspect</Text>
      </Box>
      {top.summary ? (
        <Text dimColor wrap="truncate">
          {"  "}
          {truncate(top.summary, Math.max(8, width - 2))}
        </Text>
      ) : null}
    </Box>
  );
}

/** The conversation strip — the last turn or two, never the whole product. */
function RecentTranscript({
  cells,
  height,
  width,
  now,
  expanded,
}: {
  cells: TranscriptCell[];
  height: number;
  width: number;
  now: number;
  expanded?: boolean;
}) {
  return (
    <Box flexDirection="column" marginTop={1}>
      <SectionLabel>Recent</SectionLabel>
      <Transcript
        cells={cells}
        height={Math.max(2, height)}
        width={width}
        now={now}
        expanded={expanded}
        emptyText="No recent conversation."
      />
    </Box>
  );
}

export function SidebarHome({
  dashboard,
  previews = [],
  transcript,
  width,
  height,
  now,
  expanded,
}: {
  dashboard: DashboardState;
  previews?: TerminalPreview[];
  transcript: TranscriptCell[];
  width: number;
  height: number;
  now: number;
  expanded?: boolean;
}) {
  const agents = buildAgentRows(dashboard.watchers, previews);
  const active =
    agents.find((a) => a.classification === "still_working") ?? agents[0];
  const density = width < 55 ? "compact" : "rich";

  // Budget vertical space so the operations sections are never starved by a long
  // conversation, and the recent strip absorbs whatever is left.
  const nowH = active ? (density === "rich" ? 4 : 3) : 2;
  const shown = agents.slice(0, 6);
  const watchH =
    agents.length > 0
      ? 1 + shown.length + shown.filter((a) => a.needsAttention).length
      : 0;
  const timerH = dashboard.timers.length > 0 ? 1 + Math.min(3, dashboard.timers.length) : 0;
  const attnH = dashboard.inbox.length > 0 ? 2 : 0;
  const chrome = nowH + watchH + timerH + attnH + 2;
  const recentH = Math.max(3, height - chrome);

  return (
    <Box flexDirection="column" height={height} overflow="hidden">
      <SidebarNow active={active} now={now} width={width} density={density} />
      <SidebarWatching agents={agents} now={now} width={width} />
      <SidebarTimers timers={dashboard.timers} now={now} width={width} />
      <SidebarAttention events={dashboard.inbox} width={width} />
      <RecentTranscript
        cells={transcript}
        height={recentH}
        width={width}
        now={now}
        expanded={expanded}
      />
    </Box>
  );
}
