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
  - terminal.read
  - terminal.summarize
  - terminal.extract
  - terminal.extract.async
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
4. For long-running processes, do not poll terminal output in a loop. Create a watcher or schedule a timer.
5. When creating a terminal watcher, set stopWhen/alertWhen with the WatchCondition DSL. Members: stateIs, runtimeStatusIs, contains, regex, noOutputForMs, modelJudge, and one level of all/any/not combinators. Prefer the deterministic leaves (contains/regex/stateIs/runtimeStatusIs/noOutputForMs) — they are free. Reach for modelJudge only when a condition needs semantic inference the deterministic leaves cannot express: each distinct modelJudge question is one model call per check at the watcher's configured tier (modelTier, default small; deduped across stopWhen and alertWhen). Worked example — stop a build watcher once the build resolves, alert if it breaks:
   watcher.terminal.create({ terminalIds: ["term_1"], title: "build", goal: "wait for green",
     stopWhen: { any: [{ contains: "BUILD SUCCEEDED" }, { stateIs: "exited" }] },
     alertWhen: { modelJudge: "Did the build fail or report errors?" } })
6. Use queue.digest to summarize sub-thread updates.
Report back: a concise status, any watcher/timer ids, and the next checkpoint.
