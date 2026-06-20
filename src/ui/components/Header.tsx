import { Box, Text } from "ink";
import { assistantVersion } from "../../config.js";
import { Divider } from "../primitives.js";
import { glyphs, ui } from "../theme.js";

/**
 * The control-room masthead — deliberately plain text now: the wordmark + version
 * on one line, the bound project's name beneath it, then a full-width rule
 * directly below the masthead. Brand identity at startup is handled separately (a
 * centered splash while the session loads); once the cockpit is up the header is
 * just a quiet label, not a logo.
 *
 * The rule doubles as the lip of a status strip: the debug-log line sits under it
 * when logging is active, and it's the natural home for any other at-a-glance startup
 * state we surface later. Operational detail (tier, the live MCP link) still lives in
 * the StatusLine, not here.
 */
export function Header({
  project,
  runTitle,
  logging = false,
  logFile,
  version,
}: {
  /** Deprecated: header width is resolved from the live Ink layout. */
  columns?: number;
  /** Name of the bound project, shown beneath the wordmark. */
  project?: string;
  /** Subtitle: the in-flight run's intent, when a turn is active. */
  runTitle?: string;
  /** Debug logging is active — surfaced under the rule so it's verifiable at a glance. */
  logging?: boolean;
  /** Path of the active debug log, shown dim so it can be tailed. */
  logFile?: string;
  /** Assistant version; defaults to the resolved package.json version. */
  version?: string;
}) {
  const set = glyphs();
  const ver = version ?? assistantVersion();
  return (
    <Box flexDirection="column" width="100%">
      {/* Identity: wordmark + version on one line, project name beneath it.
          minWidth=0 + truncate so a briefly-narrow terminal (a resize/host race)
          can't detonate the wordmark into a vertical char stack. */}
      <Box flexDirection="column" minWidth={0}>
        <Text wrap="truncate">
          <Text bold>Daintree Assistant</Text>
          <Text dimColor> v{ver}</Text>
        </Text>
        {project ? (
          <Text dimColor wrap="truncate">
            {project}
          </Text>
        ) : null}
        {runTitle ? (
          <Text dimColor wrap="truncate">
            {runTitle}
          </Text>
        ) : null}
      </Box>
      {/* The full-width rule sits directly under the masthead. The blank row below
          it belongs to the header so debug logging always starts one row after the
          rule. "logging" is pinned (flexShrink 0) so it is never clipped to
          "loggin", and only the path truncates on a narrow term. */}
      <Box flexDirection="column">
        <Divider />
        <Box height={1} />
        {logging ? (
          <Box>
            <Box flexShrink={0}>
              <Text color={ui.color.warning}>{set.active} logging</Text>
            </Box>
            {logFile ? (
              <Box flexShrink={1} minWidth={0}>
                <Text dimColor wrap="truncate">
                  {` ${set.bullet} `}
                  {logFile}
                </Text>
              </Box>
            ) : null}
          </Box>
        ) : null}
      </Box>
    </Box>
  );
}
