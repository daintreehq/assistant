import { Box, Text } from "ink";
import { BrandMark } from "../primitives.js";
import { glyphs, ui } from "../theme.js";

/**
 * The control-room identity bar. Left: the brand signature + the workspace it is
 * operating. Right: the operator tier and a live connection badge — the one
 * place the human confirms, at a glance, that Daintree is actually wired in.
 * A second line names the active run when there is one.
 */
export function Header({
  project,
  tier,
  connected,
  runTitle,
}: {
  project: string;
  tier: string;
  connected: boolean;
  /** Subtitle: the in-flight run's intent, when a turn is active. */
  runTitle?: string;
}) {
  const set = glyphs();
  return (
    <Box flexDirection="column" marginBottom={1}>
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
          <Text dimColor>{runTitle}</Text>
        </Box>
      ) : null}
    </Box>
  );
}
