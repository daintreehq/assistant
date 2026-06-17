/**
 * Built-in assistant recipe library.
 *
 * These are the first three "secret sauce" runbooks. They are deliberately short
 * and procedural — operational instructions, not essays. Identity and the hard
 * safety rules live in the base system prompt; recipes never restate or override
 * them (see src/models/prompts/base.ts).
 *
 * Recipe body style (docs §15):
 *   Use when: <one sentence>
 *   Procedure: numbered actions
 *   Preferred tools / Confirmation / Report back
 */
import type { Recipe } from "./types.js";

export const BASIC_DAINTREE_ORCHESTRATION_RECIPE: Recipe = {
  id: "daintree.orchestration.basic",
  title: "Daintree orchestration basics",
  version: "0.1.0",
  summary: "How to inspect Daintree state and choose safe orchestration actions.",
  whenToUse:
    "Use for general requests about current Daintree state, terminals, worktrees, agents, queues, timers, or what to do next.",
  tags: ["daintree", "state", "orchestration", "mcp", "status", "monitor"],
  priority: 100,
  maxTurns: 8,
  risk: "read",
  requiredTools: [
    "context.snapshot",
    "daintree.status",
    "daintree.listTools",
    "tool.search",
    "daintree.call",
    "queue.digest",
    "terminal.summarize",
    "timer.schedule",
    "timer.list",
    "timer.cancel",
    "watcher.terminal.create",
    "watcher.list",
    "watcher.cancel",
  ],
  body: `Use when: the user asks about current Daintree state or what to do next.
Procedure:
1. Establish current state first. Prefer context.snapshot; use Daintree MCP read tools for more detail.
2. Never guess the active worktree, focused terminal, agent state, git state, or recipe availability — read it.
3. For tool discovery, use tool.search before a raw daintree.call.
4. For long-running processes, do not poll terminal output in a loop. Create a watcher or schedule a timer.
5. Use queue.digest to summarize sub-thread updates.
Report back: a concise status, any watcher/timer ids, and the next checkpoint.`,
};

export const SPAWN_AGENT_FOR_EDITS_RECIPE: Recipe = {
  id: "daintree.edits.spawn-visible-agent",
  title: "Spawn a visible agent for file changes",
  version: "0.1.0",
  summary: "How to handle requests that require changing files.",
  whenToUse:
    "Use whenever the user asks to implement, refactor, fix, add tests, update docs, or otherwise change project files.",
  tags: ["edits", "agent", "worktree", "supervision", "implement", "fix", "refactor", "test"],
  priority: 200,
  maxTurns: 8,
  risk: "project",
  requiredTools: [
    "context.snapshot",
    "agentTask.spawnForEdits",
    "watcher.terminal.create",
    "queue.digest",
  ],
  body: `Use when: the task requires file changes (implement, refactor, fix, tests, docs).
Procedure:
1. Say plainly that file changes require a visible agent: "This needs file changes, so I'll spawn a visible agent to do them."
2. Read only the minimum context needed to scope the task.
3. Identify the target worktree. If unknown, inspect Daintree context first.
4. Use agentTask.spawnForEdits with: title (short task name), taskPrompt (exact implementation instructions), context.filePaths when known, and watcher.create: true unless the task is trivial.
5. The taskPrompt must tell the agent to: make changes only in the selected worktree; run relevant tests if practical; report changed files, tests run, and remaining risks.
6. Never edit files yourself.
Confirmation: spawning the agent mutates real state — confirm before launch per the active tier.
Report back: the terminal id, the watcher id if created, and the expected next update.`,
};

export const DAINTREE_RECIPE_RUNNER_RECIPE: Recipe = {
  id: "daintree.recipe.run-or-create",
  title: "Run or create Daintree workspace recipes",
  version: "0.1.0",
  summary:
    "How to work with Daintree workspace recipes for setup and repeatable terminal layouts.",
  whenToUse:
    "Use when the user asks to load, run, create, inspect, or apply a Daintree workspace recipe, or to create a worktree with a recipe.",
  tags: ["recipe", "worktree", "startup", "workspace", "layout", "pr-review", "setup"],
  priority: 180,
  maxTurns: 8,
  risk: "project",
  requiredTools: [
    "tool.search",
    "recipe.list",
    "recipe.run",
    "worktree.createWithRecipe",
    "daintree.call",
    "context.snapshot",
  ],
  body: `Use when: the user asks about Daintree workspace recipes or creating a worktree with one.
Note: "Daintree workspace recipes" (MCP actions) are distinct from the assistant recipes loaded into your context.
Procedure:
1. Inspect current Daintree context first if the project/worktree is ambiguous.
2. List available recipes with the recipe.list tool when needed.
3. To apply a recipe to an existing context, use recipe.run with the recipeId.
4. To create a new worktree with a startup recipe, use worktree.createWithRecipe.
5. Pass an idempotency requestKey for mutating calls when available.
6. These typed tools work at the operator tier; daintree.call is only the system-tier raw fallback for tools without a wrapper.
Confirmation: mutating actions require confirmation before execution.
Report back: what was started, which worktree/terminal ids were created, and whether a watcher should be attached.`,
};

export const BUILTIN_RECIPES: Recipe[] = [
  BASIC_DAINTREE_ORCHESTRATION_RECIPE,
  SPAWN_AGENT_FOR_EDITS_RECIPE,
  DAINTREE_RECIPE_RUNNER_RECIPE,
];
