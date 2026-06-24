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
---
Use when: the user wants to prepare the current branch for review.
Procedure:
1. Inspect current Daintree context first to confirm the active worktree/branch.
2. Check existing PRs with forge.listPRs when relevant to avoid duplicates.
3. Prepare the branch with workflow.prepBranchForReview, forwarding any required arguments (e.g. worktreeId).
4. Pass an idempotency requestKey on the mutating call when available (optional — Daintree auto-generates one if omitted).
5. workflow.prepBranchForReview is a passthrough action: the assistant does not spawn or supervise a terminal for it, so there is no watcher to attach — it returns its result directly. (If the prepared branch then needs an agent to act on review feedback, that is a separate task with its own supervision mode.)
6. Prefer these typed wrappers over the raw daintree.call escape hatch.
Confirmation: workflow.prepBranchForReview mutates real state and touches the forge (push, PR prep) — confirm before launch per the active tier (external risk).
Report back: what was prepared and the PR/branch ids involved, plus the next checkpoint.
