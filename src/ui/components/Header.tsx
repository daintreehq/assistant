import { Box, Text } from "ink";
import { assistantVersion } from "../../config.js";
import { Divider } from "../primitives.js";
import { glyphs, ui, unicodeOk } from "../theme.js";

/**
 * The control-room masthead — modelled on the Claude Code header: a small brand
 * icon on the left, then a two-space gutter, then the wordmark + version on one
 * line with the bound project's name beneath it. No border, no always-on rule —
 * the masthead is identity, framed only by the whitespace below it. Operational
 * state (tier, the live MCP link) deliberately lives in the StatusLine, not here.
 *
 * The debug-log line is the one exception: when logging is active it gets its own
 * full-width section under the identity block — a rule, then a single line naming
 * the log so it can be tailed — and nothing below it. The section is suppressed
 * entirely when logging is off, so the rule never appears without a reason.
 */

// Foliage greens, lightest at the crown and deepening to the brand green at the
// base so the canopy reads as a lit, layered tree rather than a flat block; the
// trunk is a bark brown. Three foliage tiers + a trunk.
const FOLIAGE_DEEP = ui.color.brand; // brand green, the canopy base
const FOLIAGE_MID = "#6FE3B2";
const FOLIAGE_LIGHT = "#8FEBC4"; // the sunlit crown
const TRUNK = "#A66A3C"; // bark brown

// An abstract Daintree tree: a pointed crown over two widening, base-flared canopy
// tiers and a slim trunk — four rows, so it stands taller than the two-line wordmark
// the way Claude Code's mascot overhangs its text, and pointed enough to read as a
// rainforest emergent rather than a shrub. Built only from full/quadrant blocks (no
// rounded arcs ╭╮╯╰, no geometric ▲ — theme.ts documents that those advance at a
// different width in many fonts and break the column). Each row is padded to equal
// width so the icon stays a clean rectangle, and each carries its own colour.
const LOGO_UNICODE = [
  { text: "  ▟█▙  ", color: FOLIAGE_LIGHT },
  { text: " ▟███▙ ", color: FOLIAGE_MID },
  { text: "▟█████▙", color: FOLIAGE_DEEP },
  { text: "   █   ", color: TRUNK },
];
const LOGO_ASCII = [
  { text: "  /\\   ", color: FOLIAGE_LIGHT },
  { text: " /##\\  ", color: FOLIAGE_MID },
  { text: "/####\\ ", color: FOLIAGE_DEEP },
  { text: "  ||   ", color: TRUNK },
];

export function Header({
  columns,
  project,
  runTitle,
  logging = false,
  logFile,
  version,
}: {
  /** Interior content width of the control room (used to size the bottom rule). */
  columns: number;
  /** Name of the bound project, shown beneath the wordmark. */
  project?: string;
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
  const logoWidth = logo[0].text.length;
  const ruleWidth = Math.max(1, columns);
  return (
    <Box flexDirection="column" marginBottom={2}>
      {/* Identity block: the brand icon on the left, the wordmark + version on one
          line and the project name beneath it. Top-aligned, so the icon's trunk
          row overhangs the (two-row) text — the intended Claude-Code look. */}
      <Box>
        <Box flexDirection="column" flexShrink={0} width={logoWidth}>
          {logo.map((row, i) => (
            <Text key={i} color={row.color}>
              {row.text}
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
      </Box>
      {/* Debug-log section — only when active. A full-width rule sets it off from
          the identity block, then a single line names the log; nothing follows.
          "logging" is pinned (flexShrink 0) so it is never clipped to "loggin";
          only the path truncates when the terminal is narrow. */}
      {logging ? (
        <Box flexDirection="column" marginTop={1}>
          <Divider width={ruleWidth} />
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
        </Box>
      ) : null}
    </Box>
  );
}
