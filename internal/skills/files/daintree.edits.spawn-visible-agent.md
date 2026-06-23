---
id: daintree.edits.spawn-visible-agent
title: Spawn a visible agent for edits or exploration
version: 0.2.0
summary: How to handle requests that need a visible agent — to change files or to explore read-only.
whenToUse: Use whenever the user asks to implement, refactor, fix, add tests, update docs, or otherwise change project files — OR to spawn/launch a visible agent to explore, investigate, or survey a project read-only.
priority: 200
risk: project
tags:
  - edits
  - agent
  - worktree
  - supervision
  - implement
  - fix
  - refactor
  - test
  - explore
  - investigate
  - spawn
  - launch
requiredTools:
  - context.snapshot
  - agentTask.spawnForEdits
  - agentTask.status
  - agentTask.list
  - watcher.terminal.create
  - watcher.cancel
  - queue.digest
  - queue.resolve
---
Use when: the task needs a visible agent — either to change files (implement, refactor, fix, tests, docs) or to explore/investigate a project read-only.
Procedure:
1. Say plainly that the work needs a visible agent: for edits, "This needs file changes, so I'll spawn a visible agent to do them"; for exploration, "I'll spawn a visible agent to explore this and watch it."
2. Read only the minimum context needed to scope the task.
3. Resolve the target worktree before spawning. Check context.snapshot for the worktrees list and the active/current worktree, then apply this rule exactly: if the user named a worktree, branch, path, or id — resolve it to its worktreeId from the snapshot and proceed; if exactly one worktree plausibly matches the request — proceed with that worktreeId; if multiple plausible candidates exist and none was named (or none can be confidently chosen) — STOP, list the candidates, and ask the user which one to use; if no worktree plausibly matches or the worktrees list is unavailable — STOP and ask the user to name the target worktree. Do not silently fall back to the active worktree when the target is ambiguous. Only call agentTask.spawnForEdits once the worktree is resolved.
4. Use agentTask.spawnForEdits — NEVER a raw agent.launch via daintree.call — with: agentId (a REAL configured Daintree agent — claude, codex, gemini, antigravity, …; a name the user spoke or typed is only a HINT, since voice transcription drops letters like "antigravity"→"antiravity", so do not pass an unverified name through — the wrapper rejects an unknown id with the available roster and a "did you mean" suggestion, and you then re-spawn with the right id, never the typo), mode ("edit" to change files, "explore" for a read-only investigation), title (short task name — becomes the spawned agent's visible name/tab label in Daintree, so keep it distinct when running several in parallel), taskPrompt (exact instructions), context.filePaths when known, and — unless the task is trivial — supervise it with the FLAT top-level fields `watch: true` and `watchGoal: "..."` (plain top-level scalars; providing `watchGoal` alone also attaches the watcher). Emit them as their own top-level keys — never a dotted/flattened `watcher.create` key (an unknown field the strict decoder rejects). A complete legacy nested `watcher: {"create": true, "goal": "..."}` object is still accepted, but prefer the flat fields.
5. For edit mode the taskPrompt must tell the agent to: make changes only in the selected worktree; run relevant tests if practical; report changed files, tests run, and remaining risks. For explore mode: investigate without touching files and report findings. (The wrapper appends the matching constraints automatically.)
6. Never edit files yourself.
7. After the call returns, report what actually happened: quote the real terminalId/watcherId from the result. If the launch errored or returned no terminalId, or the watcher reports the terminal exited, say so — do not claim a clean launch.
8. Close the loop. Once the watcher settles and you have relayed the agent's result, clear that inbox item with `queue.resolve {"id": "<the inbox id>"}` so it stops counting as needing attention — a finished agent's watcher has already stopped ITSELF, so there is nothing to cancel, only the lingering inbox item to resolve. Never call `watcher.cancel` on an already-finished watch (it is refused as "already ended"). If instead you abandon a STILL-ACTIVE watch — you got what you needed early, or the task was dropped — cancel it with `watcher.cancel {"id": "<watcherId>"}` and say so plainly, noting you can re-attach later. Leave OPEN any item that still needs the user (an agent waiting on a question).
Confirmation: spawning the agent mutates real state — confirm before launch per the active tier.
Report back: the terminal id, the watcher id if created, and the expected next update.
