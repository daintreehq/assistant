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
5. Pass an idempotency requestKey on the mutating call when available.
6. workflow.startWorkOnIssue attaches a supervising watcher to the launched terminal automatically — no separate watcher.terminal.create step. Report the returned watcherId; do not poll.
7. Prefer these typed wrappers over the raw daintree.call escape hatch.
Confirmation: workflow.startWorkOnIssue mutates real state and touches the forge — confirm before launch per the active tier.
Report back: which issue was started, the worktree/branch/terminal ids created, the watcher id attached, and the next checkpoint.
