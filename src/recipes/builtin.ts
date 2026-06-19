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
    "terminal.extract",
    "terminal.extract.async",
    "timer.schedule",
    "timer.list",
    "timer.cancel",
    "watcher.terminal.create",
    "watcher.list",
    "watcher.cancel",
    "grant.create",
    "grant.list",
    "grant.revoke",
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
  title: "Spawn a visible agent for edits or exploration",
  version: "0.2.0",
  summary: "How to handle requests that need a visible agent — to change files or to explore read-only.",
  whenToUse:
    "Use whenever the user asks to implement, refactor, fix, add tests, update docs, or otherwise change project files — OR to spawn/launch a visible agent to explore, investigate, or survey a project read-only.",
  tags: ["edits", "agent", "worktree", "supervision", "implement", "fix", "refactor", "test", "explore", "investigate", "spawn", "launch"],
  priority: 200,
  maxTurns: 8,
  risk: "project",
  requiredTools: [
    "context.snapshot",
    "agentTask.spawnForEdits",
    "watcher.terminal.create",
    "queue.digest",
  ],
  body: `Use when: the task needs a visible agent — either to change files (implement, refactor, fix, tests, docs) or to explore/investigate a project read-only.
Procedure:
1. Say plainly that the work needs a visible agent: for edits, "This needs file changes, so I'll spawn a visible agent to do them"; for exploration, "I'll spawn a visible agent to explore this and watch it."
2. Read only the minimum context needed to scope the task.
3. Resolve the target worktree before spawning. Check context.snapshot for the worktrees list and the active/current worktree, then apply this rule exactly: if the user named a worktree, branch, path, or id — resolve it to its worktreeId from the snapshot and proceed; if exactly one worktree plausibly matches the request — proceed with that worktreeId; if multiple plausible candidates exist and none was named (or none can be confidently chosen) — STOP, list the candidates, and ask the user which one to use; if no worktree plausibly matches or the worktrees list is unavailable — STOP and ask the user to name the target worktree. Do not silently fall back to the active worktree when the target is ambiguous. Only call agentTask.spawnForEdits once the worktree is resolved.
4. Use agentTask.spawnForEdits — NEVER a raw agent.launch via daintree.call — with: mode ("edit" to change files, "explore" for a read-only investigation), title (short task name — becomes the spawned agent's visible name/tab label in Daintree, so keep it distinct when running several in parallel), taskPrompt (exact instructions), context.filePaths when known, and watcher.create: true unless the task is trivial.
5. For edit mode the taskPrompt must tell the agent to: make changes only in the selected worktree; run relevant tests if practical; report changed files, tests run, and remaining risks. For explore mode: investigate without touching files and report findings. (The wrapper appends the matching constraints automatically.)
6. Never edit files yourself.
7. After the call returns, report what actually happened: quote the real terminalId/watcherId from the result. If the launch errored or returned no terminalId, or the watcher reports the terminal exited, say so — do not claim a clean launch.
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

export const WORKFLOW_START_WORK_RECIPE: Recipe = {
  id: "daintree.workflow.start-work-on-issue",
  title: "Start work on a forge issue",
  version: "0.1.0",
  summary: "How to pick a forge issue and start work on it through Daintree workflow actions.",
  whenToUse:
    "Use when the user asks to start work on an issue, pick up a ticket, begin an issue/PR, or set up a worktree/branch for a forge issue.",
  tags: ["issue", "forge", "workflow", "start-work", "worktree", "branch", "github", "gitlab"],
  priority: 190,
  maxTurns: 8,
  risk: "external",
  requiredTools: [
    "context.snapshot",
    "forge.listIssues",
    "forge.getIssue",
    "workflow.startWorkOnIssue",
    "watcher.terminal.create",
  ],
  body: `Use when: the user wants to start work on a forge issue.
Procedure:
1. Inspect current Daintree context first if the project/worktree is ambiguous.
2. If the issue is not identified, list candidates with forge.listIssues and confirm which one with the user.
3. Read the chosen issue with forge.getIssue to ground the task before mutating anything.
4. Start work with workflow.startWorkOnIssue, forwarding the issue identifier in arguments.
5. Pass an idempotency requestKey on the mutating call when available.
6. If the action creates a terminal/agent, attach a watcher with watcher.terminal.create rather than polling.
7. Prefer these typed wrappers over the raw daintree.call escape hatch.
Confirmation: workflow.startWorkOnIssue mutates real state and touches the forge — confirm before launch per the active tier.
Report back: which issue was started, the worktree/branch/terminal ids created, the watcher id if attached, and the next checkpoint.`,
};

export const WORKFLOW_PREP_BRANCH_RECIPE: Recipe = {
  id: "daintree.workflow.prep-branch-for-review",
  title: "Prepare a branch for review",
  version: "0.1.0",
  summary: "How to ready the current branch for review through Daintree workflow actions.",
  whenToUse:
    "Use when the user asks to prepare a branch for review, open/ready a PR, or wrap up work on an issue for review.",
  tags: ["review", "pr", "branch", "forge", "workflow", "prep", "github", "gitlab"],
  priority: 185,
  maxTurns: 8,
  risk: "external",
  requiredTools: [
    "context.snapshot",
    "forge.listPRs",
    "workflow.prepBranchForReview",
    "watcher.terminal.create",
  ],
  body: `Use when: the user wants to prepare the current branch for review.
Procedure:
1. Inspect current Daintree context first to confirm the active worktree/branch.
2. Check existing PRs with forge.listPRs when relevant to avoid duplicates.
3. Prepare the branch with workflow.prepBranchForReview, forwarding any required arguments (e.g. worktreeId).
4. Pass an idempotency requestKey on the mutating call when available.
5. If the action spawns a terminal/agent, attach a watcher with watcher.terminal.create rather than polling.
6. Prefer these typed wrappers over the raw daintree.call escape hatch.
Confirmation: workflow.prepBranchForReview mutates real state and touches the forge — confirm before launch per the active tier.
Report back: what was prepared, the PR/branch/terminal ids involved, the watcher id if attached, and the next checkpoint.`,
};

export const BUILTIN_RECIPES: Recipe[] = [
  BASIC_DAINTREE_ORCHESTRATION_RECIPE,
  SPAWN_AGENT_FOR_EDITS_RECIPE,
  DAINTREE_RECIPE_RUNNER_RECIPE,
  WORKFLOW_START_WORK_RECIPE,
  WORKFLOW_PREP_BRANCH_RECIPE,
];
