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
 * state we surface later. The permission tier — session-level capability that rarely
 * changes — sits in the identity block as a plain-English line so "system" stops
 * reading as a cryptic token; the live MCP link stays in the StatusLine (it only
 * ever surfaces there, by exception, as DEGRADED).
 *
 * The tier line stays QUIET at rest (dim, no color) for every tier including
 * `system`: a steady red `system` capsule is alarm fatigue — it screams danger
 * when nothing is happening. Red is reserved for the moment it actually matters,
 * when a destructive (git/system) action is awaiting confirmation, surfaced via the
 * `destructivePending` prop the controller derives from the live confirmation.
 */

/**
 * One-line gloss of what a permission tier can actually do, so the tier name in the
 * masthead is self-explaining instead of jargon. Mirrors the tier ladder in
 * `safety/policy.ts` (supervisor ⊂ operator ⊂ system).
 */
function tierGloss(tier: string): string {
  switch (tier) {
    case "supervisor":
      return "read & UI only";
    case "operator":
      return "terminals, projects, external";
    case "system":
      // No hard-coded "·" here — it would bypass the glyphs() ASCII fallback
      // (DAINTREE_ASCII); the only separator on this line is the active set.bullet.
      return "full access (git, system)";
    default:
      return "";
  }
}

export function Header({
  columns,
  project,
  runTitle,
  tier,
  destructivePending = false,
  logging = false,
  logFile,
  version,
}: {
  /** Cockpit width for the masthead rule. The header commits to <Static> (prints
   *  once, never repaints), so an explicit count is correct here — yoga's "100%"
   *  flex rule collapses to the content width inside a Static item, and the
   *  resize-orphan hazard that flex avoids only exists in the repainting region. */
  columns?: number;
  /** Name of the bound project, shown beneath the wordmark. */
  project?: string;
  /** Subtitle: the in-flight run's intent, when a turn is active. */
  runTitle?: string;
  /** Permission tier (supervisor|operator|system); shown with a plain-English gloss. */
  tier?: string;
  /** A destructive (git/system) action is awaiting confirmation: escalate the tier
   *  label to danger color. At rest every tier stays dim/neutral. */
  destructivePending?: boolean;
  /** Debug logging is active — surfaced under the rule so it's verifiable at a glance. */
  logging?: boolean;
  /** Path of the active debug log, shown dim so it can be tailed. */
  logFile?: string;
  /** Assistant version; defaults to the resolved package.json version. */
  version?: string;
}) {
  const set = glyphs();
  const ver = version ?? assistantVersion();
  const gloss = tier ? tierGloss(tier) : "";
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
        {tier ? (
          <Text wrap="truncate">
            <Text dimColor>tier </Text>
            <Text
              color={destructivePending ? ui.color.danger : undefined}
              dimColor
            >
              {tier}
            </Text>
            {gloss ? <Text dimColor> {set.bullet} {gloss}</Text> : null}
          </Text>
        ) : null}
      </Box>
      {/* The full-width rule sits directly under the masthead. The blank row below
          it belongs to the header so debug logging always starts one row after the
          rule. "logging" is pinned (flexShrink 0) so it is never clipped to
          "loggin", and only the path truncates on a narrow term. */}
      <Box flexDirection="column">
        <Divider width={columns} />
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
