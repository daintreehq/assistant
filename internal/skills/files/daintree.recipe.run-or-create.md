---
id: daintree.recipe.run-or-create
title: Run or create Daintree workspace recipes
version: 0.1.2
summary: Load, run, or create a Daintree workspace recipe (or a worktree with one), and supervise any LIVE agent the recipe spawns.
whenToUse: Use when the user asks to load, run, create, inspect, or apply a Daintree workspace recipe, or to create a worktree with a startup recipe.
priority: 180
risk: project
tags:
  - recipe
  - worktree
  - startup
  - workspace
  - layout
  - pr-review
  - setup
requiredTools:
  - context.snapshot
  - recipe.list
  - recipe.run
  - worktree.createWithRecipe
  - terminal.awaitAll
  - terminal.extract
  - terminal.summarize
  - terminal.read
  - terminal.sendCommand
  - agentTask.superviseTerminal
  - watcher.terminal.create
  - daintree.call
---
Use when: the user asks about Daintree workspace recipes or creating a worktree with one.

Note: "Daintree workspace recipes" (MCP actions) are distinct from the assistant skills loaded into your context. A recipe is a SAVED SET of terminal definitions — and a recipe terminal can be a plain shell, a dev-preview, OR a LIVE agent that starts running its CLI the moment it spawns, with NO watcher attached. So a recipe SETS UP a workspace; it never supervises. If a recipe launches an agent whose work you must track, you supervise that terminal SEPARATELY (see step 6). Do not assume "recipe = just shells."

Procedure:

1. Orient if the target is ambiguous. recipe.run lands in the focused/active worktree by default, so before mutating anything pin WHERE the recipe should go: context.snapshot (its terminals digest shows what is already running), then worktree.getCurrent / worktree.list for the worktree set. Skip this only when the user already named the worktree (or you're creating a fresh one in step 3).

2. Discover recipes when needed. recipe.list({ worktreeId? }) ->
   { recipes: [{ id, name, worktreeId, terminalCount, showInEmptyState }], isLoading }.
   Pick the recipe by name/id with the user. LOOK AHEAD: terminalCount tells you HOW MANY terminals will spawn, but recipe.list gives NO per-terminal kind — you cannot tell from listing whether any terminal is a live agent. You only learn that AFTER the run, by inspecting the terminals (step 5). Don't promise "this is just a dev server" off a recipe.list entry.

3. Run or create. Both are mutating and will confirm; pass a requestKey when you can (Daintree dedupes on it and strips it before validation).
   - Apply a recipe to an EXISTING worktree -> recipe.run({ recipeId, worktreeId? }) ->
     { spawnedCount, failedCount, failedTerminals: [{ index, reason }] }.
   - Create a NEW worktree AND run a startup recipe -> worktree.createWithRecipe({ branchName?, baseBranch?, recipeId?, fromRemote?, useExistingBranch?, issueNumber? | pullRequestNumber?, assignToSelf? }) ->
     { worktreeId, worktreePath, branch, recipeLaunched, spawnedTerminalCount, failedTerminalCount, assignedToSelf, assignedUsername, assignmentError }.
     (issueNumber and pullRequestNumber are mutually exclusive.) recipeLaunched is true iff ≥1 terminal spawned.

4. Read the result. The recipe spawns ALL terminals synchronously and reports partial failure INLINE — check failedCount / failedTerminalCount. If nonzero, surface which indices failed and why (failedTerminals[].reason); a partial spawn is normal to report, not a hard error. NOTE the cap: agent-sourced recipe runs are limited to 3 agent terminals — overflow shows up as failed entries, not a thrown error.

5. Detect whether the recipe spawned a LIVE agent — this is the step the old runbook got wrong. Inspect the worktree's terminals via context.snapshot (its terminals digest lists each terminal with its agentId / agentState / kind); for a precise per-worktree read use daintree.call terminal.list({ worktreeId }) ->
   { terminals: [{ id, kind, type, worktreeId, title, agentId, agentState, isInputLocked, isFocused }] }.
   An entry with an agentId / agentState is a LIVE agent running its CLI right now, unsupervised. Entries that are plain shells or dev-previews need nothing from you. A freshly spawned agent prints NOTHING for several seconds — empty output right after the run means "starting up," never "it died."

6. Supervise any live agent you must TRACK — pick exactly ONE mode, never both (two supervisors on one terminal is the classic churn-and-duplicate-notification bug):
   - IN-TURN (you drive it to completion right now): attach NO watcher. Wait for the whole cohort with ONE terminal.awaitAll({ terminalIds: [...] }) ->
     { allFinished, perTerminal: [{ terminalId, status }] } where status is one of "finished" | "failed" | "question" | "working". It confirms each agent with a small-model tail check (not a bare "waiting"). awaitAll returns NO content — once it says finished, YOU read: one terminal.extract over the same ids (a field), or terminal.summarize (clean gist of a noisy TUI), or a bounded terminal.read (short verbatim). For a SINGLE agent, use terminal.extract wait:{} instead of awaitAll. If a status is "question", answer with terminal.sendCommand and await again; if "failed", note it and don't respawn in a loop.
   - BACKGROUND ("set it up and wake me later"): adopt the running terminal with agentTask.superviseTerminal({ terminalId: "<id>", goal: "...", spawnMode: "edit"|"explore", acceptanceCriteria? }) — the purpose-built way to attach a supervisor watcher to an ALREADY-running terminal (it dedupes, so it never double-attaches). (Lower-level alternative: watcher.terminal.create({ terminalIds: ["<id>"], title: "...", goal: "..." }) — the field is goal, NOT watchGoal; watchGoal belongs to agentTask.spawnForEdits.) Then END your turn. The watcher confirms completion with the same finished check and publishes a completed_* event to the attention queue — react on that wake, don't poll.

   Finish-detection discipline (do not get this wrong): confirm completion ONLY via terminal.awaitAll, terminal.extract wait:{}, or a watcher completed_* event — each runs a small-model tail check. A bare agentState "waiting" is NOT proof of completion (an agent shows "waiting" before it starts, when paused mid-task, or when its window is backgrounded), and "completed" is TRANSIENT — it bounces back to "waiting"/"working" within seconds, so never wait to see it and never read it off a status poll as "done." Only "exited" (process ended) is authoritative on its own. terminal.getStatus reports only the raw current FSM state.

7. If the recipe spawned no live agent — or its agents are dev tooling the user watches themselves — there is nothing for you to supervise. Just report what was set up.

8. Scope check. A recipe is workspace setup, not issue work. If the user's real goal is "have an agent IMPLEMENT or INVESTIGATE something in this worktree," that is a separate task: agentTask.spawnForEdits (mode:"edit" or "explore", with its own supervision) or workflow.startWorkOnIssue (which spawns the agent AND the assistant auto-attaches a background watcher — report the terminalId + watcherId and end your turn, reacting on the completed_* event). Don't bend a recipe into an agent-work spawn.

Report back: which recipe ran, the worktree/branch ids created, spawnedCount and any failed terminals, which terminals are live agents, and — if you attached supervision — the mode you chose and the next checkpoint (an in-turn await you're driving now, or a background watcher's completed_* event you'll react to). Be explicit that watchers run only while the assistant is open.
