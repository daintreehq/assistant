---
id: daintree.workflow.prep-branch-for-review
title: Prepare a branch for review
version: 0.1.0
summary: How to ready the current branch for review through Daintree workflow actions.
whenToUse: Use when the user asks to prepare a branch for review, open/ready a PR, or wrap up work on an issue for review.
priority: 185
risk: external
tags:
  - review
  - pr
  - branch
  - forge
  - workflow
  - prep
  - github
  - gitlab
requiredTools:
  - context.snapshot
  - forge.listPRs
  - workflow.prepBranchForReview
  - watcher.terminal.create
---
Use when: the user wants to prepare the current branch for review.
Procedure:
1. Inspect current Daintree context first to confirm the active worktree/branch.
2. Check existing PRs with forge.listPRs when relevant to avoid duplicates.
3. Prepare the branch with workflow.prepBranchForReview, forwarding any required arguments (e.g. worktreeId).
4. Pass an idempotency requestKey on the mutating call when available.
5. If the action spawns a terminal/agent, attach a watcher with watcher.terminal.create rather than polling.
6. Prefer these typed wrappers over the raw daintree.call escape hatch.
Confirmation: workflow.prepBranchForReview mutates real state and touches the forge — confirm before launch per the active tier.
Report back: what was prepared, the PR/branch/terminal ids involved, the watcher id if attached, and the next checkpoint.
