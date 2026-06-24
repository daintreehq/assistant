---
id: daintree.orchestration.basic
title: Daintree orchestration basics
version: 0.1.1
summary: How to inspect Daintree state and choose safe orchestration actions.
whenToUse: Use for general requests about current Daintree state, terminals, worktrees, agents, queues, timers, or what to do next.
priority: 100
risk: read
tags:
  - daintree
  - state
  - orchestration
  - mcp
  - status
  - monitor
requiredTools:
  - context.snapshot
  - daintree.status
  - worktree.list
  - worktree.getCurrent
  - git.getProjectPulse
  - daintree.listTools
  - tool.search
  - daintree.call
  - queue.digest
  - queue.resolve
  - terminal.read
  - terminal.summarize
  - terminal.extract
  - terminal.extract.async
  - terminal.awaitAll
  - terminal.sendCommand
  - timer.schedule
  - timer.list
  - timer.cancel
  - watcher.terminal.create
  - watcher.list
  - watcher.cancel
  - grant.create
  - grant.list
  - grant.revoke
---
Use when: the user asks about current Daintree state or what to do next.
Procedure:
1. Establish current state first. Prefer context.snapshot; use Daintree MCP read tools for more detail. For worktrees specifically, prefer the typed reads worktree.list (enumerate all worktrees by id/status) and worktree.getCurrent (the active one) over digging through the snapshot or a raw daintree.call.
2. Never guess the active worktree, focused terminal, agent state, git state, or recipe availability — read it.
3. For tool discovery, use tool.search before a raw daintree.call.
4. To wait on an agent or long-running process, never hand-poll terminal output in a loop. Choose ONE supervision mode: (a) IN-TURN — when you are driving an interactive relay the user is waiting on, spawn the agents with NO watcher and wait for the whole cohort with one terminal.awaitAll (it polls their agentState concurrently — fast, no model call), then read with terminal.extract/terminal.read (verify each, and self-heal any that reported finished but still look busy — re-await/watch it) and relay with terminal.sendCommand; (b) BACKGROUND — for fire-and-forget work, spawn WITH a watcher (watch:true, watchGoal) or schedule a timer, then end your turn and react on the attention-queue wake. Never attach a watcher to an agent you are also waiting on in-turn.
5. When creating a terminal watcher (BACKGROUND supervision), set stopWhen/alertWhen with the WatchCondition DSL. Members: stateIs, runtimeStatusIs, contains, regex, noOutputForMs, modelJudge, and one level of all/any/not combinators. Prefer the deterministic leaves (contains/regex/stateIs/runtimeStatusIs/noOutputForMs) — they are free. Reach for modelJudge only when a condition needs semantic inference the deterministic leaves cannot express: each distinct modelJudge question is one model call per check at the watcher's configured tier (modelTier, default small; deduped across stopWhen and alertWhen). Worked example — stop a build watcher once the build resolves, alert if it breaks:
   watcher.terminal.create({ terminalIds: ["term_1"], title: "build", goal: "wait for green",
     stopWhen: { any: [{ contains: "BUILD SUCCEEDED" }, { stateIs: "exited" }] },
     alertWhen: { modelJudge: "Did the build fail or report errors?" } })
   Special case — "stop when the AGENT finishes its turn": do NOT use a bare `stateIs: "waiting"` stopWhen. A `waiting` state is an unreliable proxy — an agent reads "waiting" before it starts, when paused, or when its window is backgrounded — so a raw `stateIs: "waiting"` stop fires too early. For an explore agent you usually need NO custom stopWhen: the default supervisor already treats working→waiting as a candidate and CONFIRMS it with a small-model tail check before stopping (and publishes completed_*). If you must express "finished" yourself, pair the state with a judge so a bare idle can't false-trip: `stopWhen: { all: [{ stateIs: "waiting" }, { modelJudge: "Has the agent finished its work and stopped, not just paused?" }] }`. Trust that confirmed completion — never hand-poll the terminal in a loop to decide it yourself. (For IN-TURN coordination you do not need a watcher at all: terminal.awaitAll blocks on the whole cohort's agentState and hands the result straight back — reach for it instead of building a watcher just to block once. Note awaitAll is state-based, not a small-model check, so verify its result by reading the tail and self-heal a terminal that still looks busy.)
6. Use queue.digest to summarize sub-thread updates. When you have fully handled an inbox item — reported a finished agent, surfaced a result the user has now seen — resolve it with queue.resolve {"id": "<id>"} so the "needs attention" badge reflects only what still needs the user; a finished watch has already stopped its own watcher, so resolve the item rather than trying to watcher.cancel it. Leave OPEN anything still awaiting the user (an agent waiting on a question).
Report back: a concise status, any watcher/timer ids, and the next checkpoint.
