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
  - watcher.terminal.create
  - queue.digest
---
Use when: the task needs a visible agent — either to change files (implement, refactor, fix, tests, docs) or to explore/investigate a project read-only.
Procedure:
1. Say plainly that the work needs a visible agent: for edits, "This needs file changes, so I'll spawn a visible agent to do them"; for exploration, "I'll spawn a visible agent to explore this and watch it."
2. Read only the minimum context needed to scope the task.
3. Resolve the target worktree before spawning. Check context.snapshot for the worktrees list and the active/current worktree, then apply this rule exactly: if the user named a worktree, branch, path, or id — resolve it to its worktreeId from the snapshot and proceed; if exactly one worktree plausibly matches the request — proceed with that worktreeId; if multiple plausible candidates exist and none was named (or none can be confidently chosen) — STOP, list the candidates, and ask the user which one to use; if no worktree plausibly matches or the worktrees list is unavailable — STOP and ask the user to name the target worktree. Do not silently fall back to the active worktree when the target is ambiguous. Only call agentTask.spawnForEdits once the worktree is resolved.
4. Use agentTask.spawnForEdits — NEVER a raw agent.launch via daintree.call — with: mode ("edit" to change files, "explore" for a read-only investigation), title (short task name — becomes the spawned agent's visible name/tab label in Daintree, so keep it distinct when running several in parallel), taskPrompt (exact instructions), context.filePaths when known, and watcher.create: true unless the task is trivial.
5. For edit mode the taskPrompt must tell the agent to: make changes only in the selected worktree; run relevant tests if practical; report changed files, tests run, and remaining risks. For explore mode: investigate without touching files and report findings. (The wrapper appends the matching constraints automatically.)
6. Never edit files yourself.
7. After the call returns, report what actually happened: quote the real terminalId/watcherId from the result. If the launch errored or returned no terminalId, or the watcher reports the terminal exited, say so — do not claim a clean launch.
Confirmation: spawning the agent mutates real state — confirm before launch per the active tier.
Report back: the terminal id, the watcher id if created, and the expected next update.
