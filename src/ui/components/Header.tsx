import { TextAttributes } from "@opentui/core";
import { assistantVersion } from "../../config.js";
import { glyphs, ui } from "../theme.js";
import { Divider } from "../primitives.js";

/**
 * The control-room masthead — deliberately plain text (Claude Code model): the
 * wordmark + version on one line, the bound project's name beneath it, the tier
 * gloss, and the debug-log line when active. NO full-width rule: the masthead
 * scrolls away into the host's native scrollback, and a committed rule would be
 * wrapped by the host on a narrow resize and break the historical layout. Brand
 * identity at startup is handled separately (a centered splash while the session
 * loads); once the cockpit is up the header is just a quiet label, not a logo.
 *
 * The permission tier — session-level capability that rarely changes — sits in the
 * identity block as a plain-English line so "system" stops reading as a cryptic
 * token; the live MCP link stays in the StatusLine (it only ever surfaces there, by
 * exception, as DEGRADED).
 *
 * The tier line stays QUIET at rest (dim, no color) for every tier including
 * `system`: a steady red `system` capsule is alarm fatigue. Red is reserved for the
 * moment it actually matters — a destructive (git/system) action awaiting
 * confirmation, surfaced via the `destructivePending` prop the controller derives.
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
  /** Cockpit width for the masthead block; bounds the `truncate` rows. */
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
  /** Debug logging is active — surfaced under the identity block so it's verifiable. */
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
    <box flexDirection="column" width={columns}>
      {/* Identity: wordmark + version on one line, project name beneath it.
          minWidth=0 + truncate so a briefly-narrow terminal can't detonate the
          wordmark into a vertical char stack. */}
      <box flexDirection="column" minWidth={0}>
        <text truncate>
          <span attributes={TextAttributes.BOLD}>Daintree Assistant</span>
          <span attributes={TextAttributes.DIM}> v{ver}</span>
        </text>
        {project ? (
          <text attributes={TextAttributes.DIM} truncate>
            {project}
          </text>
        ) : null}
        {runTitle ? (
          <text attributes={TextAttributes.DIM} truncate>
            {runTitle}
          </text>
        ) : null}
        {tier ? (
          <text truncate>
            <span attributes={TextAttributes.DIM}>tier </span>
            <span
              fg={destructivePending ? ui.color.danger : undefined}
              attributes={TextAttributes.DIM}
            >
              {tier}
            </span>
            {gloss ? (
              <span attributes={TextAttributes.DIM}>
                {" "}
                {set.bullet} {gloss}
              </span>
            ) : null}
          </text>
        ) : null}
      </box>
      {/* A rule closes the identity band, right below the wordmark / project / tier
          lines. It MUST take a fixed width (`columns`): the masthead is committed to
          the host's native scrollback as a fixed snapshot that does NOT reflow, so a
          flex `width:'100%'` rule would be wrapped by the host on a narrow resize and
          break the historical layout. A fixed-length rule snapshots cleanly. */}
      <Divider width={columns} />
      {/* Debug logging is a SEPARATE concern, so it sits BELOW the rule. "logging" is
          pinned (flexShrink 0) so it's never clipped to "loggin"; only the path
          truncates. flexDirection="row" because OpenTUI <box> defaults to column. */}
      {logging ? (
        <box flexDirection="row">
          <box flexShrink={0}>
            <text fg={ui.color.warning}>{set.active} logging</text>
          </box>
          {logFile ? (
            <box flexShrink={1} minWidth={0}>
              <text attributes={TextAttributes.DIM} truncate>
                {` ${set.bullet} `}
                {logFile}
              </text>
            </box>
          ) : null}
        </box>
      ) : null}
    </box>
  );
}
