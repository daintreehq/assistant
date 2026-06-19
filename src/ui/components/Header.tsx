import { Box, Text } from "ink";
import { assistantVersion } from "../../config.js";
import { Divider } from "../primitives.js";
import { glyphs, ui, unicodeOk } from "../theme.js";

/**
 * The control-room masthead — modelled on the Claude Code header: a small brand
 * icon on the left, then a two-space gutter, then the wordmark + version on one
 * line with the working folder beneath it. No border, no always-on rule — the
 * masthead is identity, framed only by the whitespace below it. Operational state
 * (tier, the live MCP link) deliberately lives in the StatusLine, not here.
 *
 * The debug-log line is the one exception: when logging is active it gets its own
 * full-width section under the identity block — a rule, then a single line naming
 * the log so it can be tailed — and nothing below it. The section is suppressed
 * entirely when logging is off, so the rule never appears without a reason.
 */

// An abstract Daintree canopy: a tapered crown over a solid foliage row, a centred
// trunk below — three rows so it stands a touch taller than the two-line wordmark,
// the way Claude Code's mascot overhangs its text. Built only from full/half blocks
// (no rounded arcs ╭╮╯╰, no geometric ▲ — theme.ts documents that those advance at a
// different width in many fonts and break the column). Every row is padded to equal
// width so the icon stays a clean rectangle beside the text.
const LOGO_UNICODE = [" ▄█▄ ", "█████", "  █  "];
const LOGO_ASCII = [" /\\ ", "/##\\", " || "];

export function Header({
  columns,
  folder,
  runTitle,
  logging = false,
  logFile,
  version,
}: {
  /** Interior content width of the control room (used to size the bottom rule). */
  columns: number;
  /** Working folder, tilde-abbreviated, shown beneath the wordmark. */
  folder?: string;
  /** Subtitle: the in-flight run's intent, when a turn is active. */
  runTitle?: string;
  /** Debug logging is active — surfaced as a section so it's verifiable at a glance. */
  logging?: boolean;
  /** Path of the active debug log, shown dim so it can be tailed. */
  logFile?: string;
  /** Assistant version; defaults to the resolved package.json version. */
  version?: string;
}) {
  const set = glyphs();
  const logo = unicodeOk() ? LOGO_UNICODE : LOGO_ASCII;
  const ver = version ?? assistantVersion();
  // The icon column gets an explicit width (not just trailing spaces in the
  // strings) so the text gutter is stable even if a glyph renders narrow.
  const logoWidth = logo[0].length;
  const ruleWidth = Math.max(1, columns);
  return (
    <Box flexDirection="column" marginBottom={2}>
      {/* Identity block: the brand icon on the left, the wordmark + version on one
          line and the folder beneath it. Top-aligned, so the icon's trunk row
          overhangs the (two-row) text — which is the intended Claude-Code look. */}
      <Box>
        <Box flexDirection="column" flexShrink={0} width={logoWidth}>
          {logo.map((line, i) => (
            <Text key={i} color={ui.color.brand}>
              {line}
            </Text>
          ))}
        </Box>
        {/* minWidth=0 + truncate so a briefly-narrow terminal (a resize/host
            race) can't detonate the wordmark into a vertical char stack. */}
        <Box flexDirection="column" flexShrink={1} minWidth={0} marginLeft={2}>
          <Text wrap="truncate">
            <Text bold>Daintree Assistant</Text>
            <Text dimColor> v{ver}</Text>
          </Text>
          {folder ? (
            <Text dimColor wrap="truncate">
              {folder}
            </Text>
          ) : null}
          {runTitle ? (
            <Text dimColor wrap="truncate">
              {runTitle}
            </Text>
          ) : null}
        </Box>
      </Box>
      {/* Debug-log section — only when active. A full-width rule sets it off from
          the identity block, then a single line names the log; nothing follows. */}
      {logging ? (
        <Box flexDirection="column" marginTop={1}>
          <Divider width={ruleWidth} />
          <Box>
            <Text color={ui.color.warning}>{set.active} logging</Text>
            {logFile ? (
              <Text dimColor wrap="truncate">
                {` ${set.bullet} `}
                {logFile}
              </Text>
            ) : null}
          </Box>
        </Box>
      ) : null}
    </Box>
  );
}
