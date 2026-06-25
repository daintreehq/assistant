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
  - terminal.awaitAll
  - terminal.summarize
  - terminal.extract
  - terminal.sendCommand
  - queue.publish
  - queue.digest
  - queue.resolve
---
Use when: the task needs a visible agent — either to change files (implement, refactor, fix, tests, docs) or to explore/investigate a project read-only.
Procedure:
1. Say plainly that the work needs a visible agent: for edits, "This needs file changes, so I'll spawn a visible agent to do them"; for exploration, "I'll spawn a visible agent to investigate this."
   Decide the supervision mode up front (it shapes the spawn in step 4):
   - EDITS, or any longer fire-and-forget job you'll review later → BACKGROUND: spawn WITH a watcher and end your turn; react when its completed_* event lands on the attention queue. Don't block the turn on a long edit.
   - A quick read-only EXPLORE the user is waiting on right now → IN-TURN: spawn with NO watcher, then wait with terminal.awaitAll and relay the answer in the same turn. (For several explore agents at once, use the multi-agent orchestration runbook.)
   Pick ONE — never attach a watcher to an agent you are also waiting on in-turn.
2. Read only the minimum context needed to scope the task.
3. Resolve the target worktree before spawning. Check context.snapshot for the worktrees list and the active/current worktree, then apply this rule exactly: if the user named a worktree, branch, path, or id — resolve it to its worktreeId from the snapshot and proceed; if exactly one worktree plausibly matches the request — proceed with that worktreeId; if multiple plausible candidates exist and none was named (or none can be confidently chosen) — STOP, list the candidates, and ask the user which one to use; if no worktree plausibly matches or the worktrees list is unavailable — STOP and ask the user to name the target worktree. Do not silently fall back to the active worktree when the target is ambiguous. Only call agentTask.spawnForEdits once the worktree is resolved.
4. Use agentTask.spawnForEdits — NEVER a raw agent.launch via daintree.call — with: agentId (a REAL configured Daintree agent — claude, codex, gemini, antigravity, …; a name the user spoke or typed is only a HINT, since voice transcription drops letters like "antigravity"→"antiravity", so do not pass an unverified name through — the wrapper rejects an unknown id with the available roster and a "did you mean" suggestion, and you then re-spawn with the right id, never the typo), mode ("edit" to change files, "explore" for a read-only investigation), title (short task name — becomes the spawned agent's visible name/tab label in Daintree, so keep it distinct when running several in parallel), taskPrompt (exact instructions), and context.filePaths when known. Then supervise per the mode you chose in step 1:
   - BACKGROUND (edits / fire-and-forget) — attach a watcher with the FLAT top-level fields `watch: true` and `watchGoal: "..."` (plain top-level scalars; providing `watchGoal` alone also attaches the watcher). Emit them as their own top-level keys — never a dotted/flattened `watcher.create` key (an unknown field the strict decoder rejects). A complete legacy nested `watcher: {"create": true, "goal": "..."}` object is still accepted, but prefer the flat fields.
   - IN-TURN (quick explore you're waiting on) — OMIT `watch`/`watchGoal` entirely; you will block on terminal.awaitAll in step 8 instead. Attaching a watcher here would double-supervise the agent you are already waiting on.
5. For edit mode the taskPrompt must tell the agent to: make changes only in the selected worktree; run relevant tests if practical; report changed files, tests run, and remaining risks. For explore mode: investigate without touching files and report findings. (The wrapper appends the matching constraints automatically.)
6. Never edit files yourself.
7. After the call returns, report what actually happened: quote the real terminalId/watcherId from the result. If the launch errored or returned no terminalId, or the watcher reports the terminal exited, say so — do not claim a clean launch.
8. Close the loop, per mode:
   - BACKGROUND — end your turn after the spawn; the watcher confirms completion and wakes you with a completed_* event. Once you've relayed the agent's result, clear that inbox item with `queue.resolve {"id": "<the inbox id>"}` so it stops counting as needing attention — a finished agent's watcher has already stopped ITSELF, so there is nothing to cancel, only the lingering inbox item to resolve. Never call `watcher.cancel` on an already-finished watch (it is refused as "already ended"). If instead you abandon a STILL-ACTIVE watch — you got what you needed early, or the task was dropped — cancel it with `watcher.cancel {"id": "<watcherId>"}` and say so plainly, noting you can re-attach later. Leave OPEN any item that still needs the user (an agent waiting on a question).
   - IN-TURN — block on `terminal.awaitAll({ terminalIds: ["<the terminalId>"] })` (bounded; polls agentState only, no model call), then read the result with terminal.summarize (or terminal.extract for a specific field) and relay it in the same turn — and since awaitAll is state-based, if the tail shows the agent is still working, re-await/watch it rather than relaying a half-done screen. The result names the stragglers at the top level (`stillWorking` / `askingQuestion` id arrays): if the budget ran out, re-await the `stillWorking` id directly **at most twice** (three awaitAll calls total on the same terminal; raise `maxAttempts` — default 30 ≈ 60s, max 240 ≈ 480s — on a re-await for a known-slow agent rather than looping). After two re-awaits with no finish, the agent is HUNG — stop re-awaiting (never a fourth await), hand it off to the background, and END the turn: publish a blocked inbox item and attach a watcher so the human is woken when it eventually finishes.
   ```
   queue.publish({ "source": "model_worker", "severity": "blocked",
     "title": "Agent hung — did not finish",
     "summary": "Agent <terminalId> still working after 2 re-awaits; attaching a watcher so you're notified when it finishes.",
     "target": { "terminalId": "<terminalId>" }, "dedupeKey": "hung-<terminalId>" })
   watcher.terminal.create({ "terminalIds": ["<terminalId>"], "title": "hung agent recovery",
     "goal": "notify when the stuck agent finishes",
     "stopWhen": { "all": [ { "stateIs": "waiting" }, { "modelJudge": "Has the agent finished its work and stopped, not just paused?" } ] } })
   ```
   If it reports the agent in `askingQuestion`, answer it with terminal.sendCommand and await again; if it reports failed, say so. On the happy path no watcher means no inbox item to resolve — the hung-agent escape is the one exception (it attaches a watcher and publishes an item, which you resolve once the agent finishes).
Confirmation: spawning the agent mutates real state — confirm before launch per the active tier.
Report back: the terminal id, the watcher id if you attached one (BACKGROUND), and either the relayed result (IN-TURN) or the expected next update (BACKGROUND).
