import { Box, Text } from "ink";
import { BrandMark } from "../primitives.js";
import { glyphs, ui, unicodeOk } from "../theme.js";

/**
 * The control-room identity bar, framed in a border so it reads as the masthead
 * the rest of the cockpit hangs beneath (everything below is borderless). Left:
 * the brand signature + the workspace it is operating. Right: the operator tier
 * and a live connection badge — the one place the human confirms, at a glance,
 * that Daintree is actually wired in. A second line names the active run when
 * there is one; a third surfaces the debug log when active.
 */
export function Header({
  project,
  tier,
  connected,
  runTitle,
  logging = false,
  logFile,
}: {
  project: string;
  tier: string;
  connected: boolean;
  /** Subtitle: the in-flight run's intent, when a turn is active. */
  runTitle?: string;
  /** Debug logging is active — surfaced as a badge so it's verifiable at a glance. */
  logging?: boolean;
  /** Path of the active debug log, shown dim under the bar so it can be tailed. */
  logFile?: string;
}) {
  const set = glyphs();
  return (
    <Box flexDirection="column" marginBottom={1}>
      <Box
        flexDirection="column"
        borderStyle={unicodeOk() ? "round" : "classic"}
        borderColor={ui.color.muted}
        paddingX={1}
      >
        <Box justifyContent="space-between">
        {/* Left gives way first (the project name truncates); the right-hand
            tier + connection badge never shrinks. Without these guards a real
            terminal briefly narrower than the laid-out width (a resize/host
            race) detonates the row into a vertical char-by-char stack. */}
        <Box flexShrink={1} minWidth={0}>
          <BrandMark />
          <Text dimColor>{"  "}</Text>
          <Text dimColor wrap="truncate">
            {project}
          </Text>
        </Box>
        <Box flexShrink={0}>
          <Text dimColor>{tier.toUpperCase()}</Text>
          <Text>{"  "}</Text>
          {connected ? (
            <Text color={ui.color.accent}>
              {set.connected} CONNECTED
            </Text>
          ) : (
            <Text color={ui.color.warning}>
              {set.connected} DEGRADED
            </Text>
          )}
        </Box>
      </Box>
      {runTitle ? (
        <Box>
          <Text dimColor>{"  "}</Text>
          <Text dimColor wrap="truncate">
            {runTitle}
          </Text>
        </Box>
      ) : null}
      {/* The debug-log indicator lives on its own line — kept off the identity
          row so its width can't push the non-shrinking tier/connection cluster
          into a second row on a narrow terminal. The badge is the warning glyph;
          the path (when known) truncates so this stays exactly one row. */}
      {logging ? (
        <Box>
          <Text dimColor>{"  "}</Text>
          <Text color={ui.color.warning}>
            {set.active} LOG
          </Text>
          {logFile ? (
            <Text dimColor wrap="truncate">
              {" · "}
              {logFile}
            </Text>
          ) : null}
        </Box>
      ) : null}
      </Box>
    </Box>
  );
}
