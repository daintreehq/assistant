import { Box, Text } from "ink";
import { assistantVersion } from "../../config.js";
import { Divider } from "../primitives.js";
import { glyphs, ui, unicodeOk } from "../theme.js";

/**
 * The control-room masthead — a brand identity bar framed in a border so it reads
 * as the header the rest of the cockpit hangs beneath (everything below is
 * borderless). Modelled on the Claude Code masthead: a small Daintree canopy logo
 * on the left, the product name and version stacked beside it. Operational state
 * (tier, the live MCP link) deliberately lives in the StatusLine, not here — the
 * masthead is identity, not status. A full-width rule closes the bottom edge.
 */

// The Daintree canopy + trunk, two rows tall so the wordmark and version stack
// cleanly beside it without stealing more of the body than the masthead needs.
// Built from block / box-drawing glyphs only (no rounded arcs ╭╮╯╰ — theme.ts
// documents that those substitute at a different advance width in many fonts and
// break alignment). Each row is padded to equal width so the column stays
// rectangular beside the text.
const LOGO_UNICODE = ["▟█▙", " █ "];
const LOGO_ASCII = ["/^\\", " | "];

export function Header({
  columns,
  runTitle,
  logging = false,
  logFile,
  version,
}: {
  /** Interior content width of the control room (used to size the bottom rule). */
  columns: number;
  /** Subtitle: the in-flight run's intent, when a turn is active. */
  runTitle?: string;
  /** Debug logging is active — surfaced as a badge so it's verifiable at a glance. */
  logging?: boolean;
  /** Path of the active debug log, shown dim so it can be tailed. */
  logFile?: string;
  /** Assistant version; defaults to the resolved package.json version. */
  version?: string;
}) {
  const set = glyphs();
  const logo = unicodeOk() ? LOGO_UNICODE : LOGO_ASCII;
  const ver = version ?? assistantVersion();
  // Pin the box to the interior width so the rule below is flush by construction:
  // a stretch/shrink box has no width we can predict, which leaves the rule a cell
  // proud of the border. With a fixed width, interior content = columns − border
  // (2) − paddingX (2), so the rule is exactly columns − 4.
  const ruleWidth = Math.max(1, columns - 4);
  return (
    <Box flexDirection="column" marginBottom={1}>
      <Box
        flexDirection="column"
        width={columns}
        borderStyle={unicodeOk() ? "round" : "classic"}
        borderColor={ui.color.muted}
        paddingX={1}
      >
        {/* Identity block: the brand canopy on the left, the wordmark + version
            stacked to its right. Two rows tall; a run subtitle, when present, adds
            a third text row (so headerH in ControlRoom accounts for runTitle). */}
        <Box>
          <Box flexDirection="column" flexShrink={0}>
            {logo.map((line, i) => (
              <Text key={i} color={ui.color.brand}>
                {line}
              </Text>
            ))}
          </Box>
          {/* minWidth=0 + truncate so a briefly-narrow terminal (a resize/host
              race) can't detonate the wordmark into a vertical char stack. */}
          <Box flexDirection="column" flexShrink={1} minWidth={0} marginLeft={2}>
            <Text bold wrap="truncate">
              Daintree assistant
            </Text>
            <Text dimColor wrap="truncate">
              v{ver}
            </Text>
            {runTitle ? (
              <Text dimColor wrap="truncate">
                {runTitle}
              </Text>
            ) : null}
          </Box>
        </Box>
        {/* The debug-log indicator on its own line — kept off the identity block
            so its width can't reflow it, and held to exactly one row (the path
            truncates). The badge is the active glyph. */}
        {logging ? (
          <Box>
            <Text color={ui.color.warning}>{set.active} LOG</Text>
            {logFile ? (
              <Text dimColor wrap="truncate">
                {" · "}
                {logFile}
              </Text>
            ) : null}
          </Box>
        ) : null}
        {/* A full-width rule connects the left and right edges, closing the empty
            space below the identity block. */}
        <Divider width={ruleWidth} />
      </Box>
    </Box>
  );
}
