/**
 * Lean sidebar sections that render rows already shaped + truncated by the view
 * model. Each section is a single calm block: one-word header, status rows, and
 * a quiet empty state. Symbols carry the meaning; color only marks state.
 */
import { Box, Text } from "ink";
import type {
  AuditRow,
  Density,
  RecentRow,
  TerminalRow,
  TimerRow,
  WatcherRow,
} from "./model.js";
import { theme } from "../theme.js";

function Empty({ label }: { label: string }) {
  return <Text dimColor>{label}</Text>;
}

export function WatcherSection({
  rows,
  density = "comfortable",
}: {
  rows: WatcherRow[];
  density?: Density;
}) {
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.dim}>Watchers</Text>
      {rows.length === 0 ? (
        <Empty label="no active watchers" />
      ) : density === "dense" ? (
        // One predictable line per watcher (no right-alignment to fit survival
        // widths): glyph · purpose · status.
        rows.map((r) => (
          <Text key={r.id}>
            <Text color={r.color}>{r.symbol}</Text> {r.title}{" "}
            <Text color={r.color}>{r.status}</Text>
          </Text>
        ))
      ) : (
        rows.map((r) => (
          <Box key={r.id} justifyContent="space-between">
            <Text>
              <Text color={r.color}>{r.symbol}</Text> {r.title}
            </Text>
            <Text>
              <Text color={r.color}>{r.status}</Text> <Text dimColor>{r.age}</Text>
            </Text>
          </Box>
        ))
      )}
    </Box>
  );
}

export function TerminalSection({
  rows,
  density = "comfortable",
}: {
  rows: TerminalRow[];
  density?: Density;
}) {
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.dim}>Terminals</Text>
      {rows.length === 0 ? (
        <Empty label="no watched terminals" />
      ) : (
        rows.map((r) =>
          density === "dense" ? (
            <Text key={r.id}>
              <Text color={theme.brand}>{r.id}</Text> <Text dimColor>{r.state}</Text>
              {r.line ? <Text dimColor>: {r.isOutput ? `"${r.line}"` : r.line}</Text> : null}
            </Text>
          ) : (
            <Box key={r.id} flexDirection="column">
              <Text>
                <Text color={theme.brand}>{r.id}</Text> <Text dimColor>{r.state}</Text>
              </Text>
              {r.line ? <Text dimColor>  {r.isOutput ? `"${r.line}"` : r.line}</Text> : null}
            </Box>
          ),
        )
      )}
    </Box>
  );
}

export function TimerSection({ rows }: { rows: TimerRow[] }) {
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.dim}>Timers</Text>
      {rows.length === 0 ? (
        <Empty label="no timers" />
      ) : (
        rows.map((t) => (
          <Text key={t.id}>
            <Text color={theme.brand}>{t.clock}</Text> <Text dimColor>{t.title}</Text>
          </Text>
        ))
      )}
    </Box>
  );
}

export function AuditStrip({ rows }: { rows: AuditRow[] }) {
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.dim}>Audit</Text>
      {rows.length === 0 ? (
        <Empty label="nothing yet" />
      ) : (
        rows.map((r) => (
          <Box key={r.id} justifyContent="space-between">
            <Text>
              <Text color={r.color}>{r.symbol}</Text> <Text dimColor>{r.name}</Text>
            </Text>
            <Text color={theme.dim}>{r.ms}</Text>
          </Box>
        ))
      )}
    </Box>
  );
}

export function RecentSection({ rows }: { rows: RecentRow[] }) {
  if (rows.length === 0) return null;
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.dim}>Recent</Text>
      {rows.map((r) => (
        <Text key={r.id}>
          <Text color={r.who === "you" ? theme.info : theme.brand}>{r.who.padEnd(3)}</Text>{" "}
          <Text dimColor>{r.text}</Text>
        </Text>
      ))}
    </Box>
  );
}
