---
id: daintree.workflow.start-work-on-issue
title: Start work on a forge issue
version: 0.1.0
summary: How to pick a forge issue and start work on it through Daintree workflow actions.
whenToUse: Use when the user asks to start work on an issue, pick up a ticket, begin an issue/PR, or set up a worktree/branch for a forge issue.
priority: 190
risk: external
tags:
  - issue
  - forge
  - workflow
  - start-work
  - worktree
  - branch
  - github
  - gitlab
requiredTools:
  - context.snapshot
  - forge.listIssues
  - forge.getIssue
  - workflow.startWorkOnIssue
---
Use when: the user wants to start work on a forge issue.
Procedure:
1. Inspect current Daintree context first if the project/worktree is ambiguous.
2. If the issue is not identified, list candidates with forge.listIssues and confirm which one with the user.
3. Read the chosen issue with forge.getIssue to ground the task before mutating anything.
4. Start work with workflow.startWorkOnIssue, forwarding the issue identifier in arguments.
5. Pass an idempotency requestKey on the mutating call when available (optional — Daintree auto-generates one if omitted).
6. This is BACKGROUND supervision by design: workflow.startWorkOnIssue spawns the work terminal and attaches a supervising watcher AUTOMATICALLY (no separate watcher.terminal.create, and do NOT also drive it in-turn with terminal.awaitAll — that would double-supervise the same terminal). Report the returned watcherId, then END your turn; the watcher confirms completion and wakes you with a completed_* event on the attention queue. Never hand-poll the terminal.
7. Prefer these typed wrappers over the raw daintree.call escape hatch.
Confirmation: workflow.startWorkOnIssue mutates real state and touches the forge — confirm before launch per the active tier (external risk).
Report back: which issue was started, the worktree/branch/terminal ids created, the watcher id attached, and that supervision runs only while the assistant is open — the next checkpoint is the watcher's completed_* event.
