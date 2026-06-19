/**
 * The single source of truth for the assistant's slash commands.
 *
 * Four surfaces used to carry their own hand-maintained copy of this list and
 * silently drifted apart (issue #50: `/models` was handled but listed nowhere):
 *   - the filterable command palette in the Ink composer (`Composer.tsx`)
 *   - the `/help` overlay in the Ink UI (`HelpOverlay.tsx`)
 *   - the `HELP_TEXT` blob in the Ink command handler (`cli/commandData.ts`)
 *   - the `HELP` blob in the REPL handler (`cli/commands.ts`)
 *
 * They all now derive from `COMMAND_REGISTRY`. This module is intentionally pure
 * data — no `App`, no Ink, no runtime imports — so both the `src/cli` and
 * `src/ui` layers can import it without pulling in heavy dependencies or risking
 * a circular import. A registry test asserts every command here is actually
 * handled by both switch statements, so adding a command in one place forces the
 * surfaces (and the test) to stay in sync.
 */

export interface CommandMeta {
  /** Bare command name without the leading slash (e.g. `inbox`). */
  name: string;
  /** Brief, intent-focused label for the filterable command palette. */
  palette: string;
  /** Slash + argument syntax shown in the help surfaces' left column. */
  syntax: string;
  /** Full one-line description for the help overlay / text help. */
  help: string;
}

/**
 * Ordered so the help surfaces read top-down from everyday inspection commands
 * to session/teardown ones. The handler switches in `cli/commandData.ts` and
 * `cli/commands.ts` accept the same names (plus the `help`/`quit` aliases
 * `?`/`exit`/`q`, which are not user-facing list entries).
 */
export const COMMAND_REGISTRY: CommandMeta[] = [
  {
    name: "status",
    palette: "connection and session",
    syntax: "/status",
    help: "Daintree connection, project, models, tier",
  },
  {
    name: "inbox",
    palette: "items requiring attention",
    syntax: "/inbox [sev]",
    help: "queued watcher/timer events (info|attention|urgent|blocked)",
  },
  {
    name: "tools",
    palette: "list / search tools",
    syntax: "/tools [query]",
    help: "list/search available tools",
  },
  {
    name: "timers",
    palette: "scheduled operations",
    syntax: "/timers",
    help: "scheduled timers",
  },
  {
    name: "watchers",
    palette: "supervised agents",
    syntax: "/watchers",
    help: "active watchers",
  },
  {
    name: "audit",
    palette: "recent tool calls · export",
    syntax: "/audit [n]",
    help: "recent tool calls (default 15); export <json|csv> [actor= tool= outcome= from= to= limit=]",
  },
  {
    name: "models",
    palette: "model routing",
    syntax: "/models",
    help: "model routing across the large/medium/small tiers",
  },
  {
    name: "permissions",
    palette: "supervisor | operator | system",
    syntax: "/permissions [tier]",
    help: "show or set tier (supervisor|operator|system)",
  },
  {
    name: "recipes",
    palette: "loaded · reload · load · clear",
    syntax: "/recipes [sub]",
    help: "loaded | reload | load <id…> | clear",
  },
  {
    name: "compact",
    palette: "summarize the conversation",
    syntax: "/compact",
    help: "summarize + condense the conversation",
  },
  {
    name: "doctor",
    palette: "environment check",
    syntax: "/doctor",
    help: "check MCP / config / project mapping (with fixes)",
  },
  {
    name: "reconnect",
    palette: "retry the Daintree connection",
    syntax: "/reconnect",
    help: "retry the Daintree MCP connection",
  },
  {
    name: "help",
    palette: "all commands and keys",
    syntax: "/help",
    help: "this help",
  },
  {
    name: "quit",
    palette: "exit",
    syntax: "/quit",
    help: "exit",
  },
];

/** Palette tuples for the Ink composer: `["/name", palette-desc]`. */
export function paletteEntries(): Array<[string, string]> {
  return COMMAND_REGISTRY.map((c) => [`/${c.name}`, c.palette]);
}

/** Help-overlay tuples: `[syntax, help-desc]`. */
export function overlayEntries(): Array<[string, string]> {
  return COMMAND_REGISTRY.map((c) => [c.syntax, c.help]);
}

/**
 * Plain-text help lines — syntax left-padded to `pad`, then the description.
 * Shared by the Ink `HELP_TEXT` blob and the REPL `HELP` blob so both read
 * identically. `pad` is 24 to clear the widest syntax (`/permissions [tier]`).
 */
export function helpLines(pad = 24): string[] {
  return COMMAND_REGISTRY.map((c) => `${c.syntax.padEnd(pad)}${c.help}`);
}
