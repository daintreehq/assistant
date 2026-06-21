import { Box, Text } from "ink";
import { assistantVersion } from "../../config.js";
import { glyphs, ui } from "../theme.js";

/**
 * The control-room masthead — deliberately plain text (Claude Code model): the
 * wordmark + version on one line, the bound project's name beneath it, the tier
 * gloss, and the debug-log line when active. NO full-width rule: the masthead
 * commits to native scrollback and scrolls away, and a committed rule would be
 * wrapped by the host on a narrow resize and permanently break the layout. Brand
 * identity at startup is handled separately (a centered splash while the session
 * loads); once the cockpit is up the header is just a quiet label, not a logo.
 *
 * The permission tier — session-level capability that rarely changes — sits in the
 * identity block as a plain-English line so "system" stops reading as a cryptic
 * token; the live MCP link stays in the StatusLine (it only ever surfaces there, by
 * exception, as DEGRADED).
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
    // Size to the EXPLICIT numeric `columns`, never `width="100%"`. The header
    // commits to <Static>, where Ink lays each item out in an isolated tree with no
    // parent width: a percentage there collapses to the CONTENT width, leaving the
    // `wrap="truncate"` rows below (notably the long log path) with no bound to
    // truncate against — so the terminal physically wraps them. A definite width is
    // the bound truncate needs, and the Static-prints-once context means the
    // resize-lag hazard that forces flex sizing in the live region does not apply
    // here (same reason the Divider below already takes an explicit count).
    <Box flexDirection="column" width={columns}>
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
      {/* NO full-width rule. The masthead commits to native scrollback (<Static>) and
          scrolls away (Claude Code model); a committed full-width rule would be wrapped
          by the host on shrink and permanently break the historical layout. The debug-
          log line just sits a blank row under the identity block. "logging" is pinned
          (flexShrink 0) so it is never clipped to "loggin"; only the path truncates. */}
      {logging ? (
        <Box marginTop={1}>
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
  );
}
