---
id: daintree.workflow.prep-branch-for-review
title: Prepare a branch for review
version: 0.1.2
summary: Get a read-only review-readiness verdict, then do the real prep — commit, push, open the PR — branching on what the verdict reports.
whenToUse: Use when the user asks to prepare a branch for review, open/ready a PR, wrap up work on an issue for review, or check whether the current branch is ready to push.
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
  - worktree.getCurrent
  - forge.listPRs
  - workflow.prepBranchForReview
  - daintree.call
---
Use when: the user wants to ready the current branch for review / open a PR.

The trap this runbook fixes: `workflow.prepBranchForReview` is READ-ONLY despite the
name. It does NOT commit, push, or open a PR — it returns a verdict you act on. The
actual prep (commit → push → open PR) is a SEPARATE, confirmed sequence you run after a
good verdict. So call the verdict freely (no confirmation), then do the real work. The
focus-hint tools above are not a fence — every registry tool is callable this turn.

## Step 1 — Fix the target
Call `context.snapshot`, then `worktree.getCurrent`, so you KNOW which worktree/branch
you are prepping. `prepBranchForReview` takes optional `cwd?`/`projectId?` and otherwise
targets the active worktree; a later commit/push lands wherever you point it, so be sure
it's the branch the user means. `worktree.getCurrent` returns `{ worktree: { id, path,
branch, prNumber, prUrl, ... } | null }` — if `prNumber` is already set, a PR exists on
this branch and the job is usually "ready/update it," not "create a new one."

## Step 2 — Don't open a duplicate PR
`forge.listPRs({ state: "open" })` -> `{ items: [PR...], nextCursor, hasMore }`. Each PR
carries `head`/`number`/`url`/`reviewDecision?`/`ciStatus?`. If an open PR's head is this
branch, you are updating, not creating — skip the PR-create in step 5 and just push.

## Step 3 — Get the verdict (free, read-only, NO confirmation)
Call `workflow.prepBranchForReview` (pass `cwd` only if targeting a non-active worktree).
It returns:
```
{ verdict, hasUncommittedChanges, hasMergeConflicts,
  stagedCount, unstagedCount, currentBranch, repoState, detectedRunners: [...] }
```
`verdict` is one of:
- `ready` — tree is in a committable/pushable shape. GREEN LIGHT to prep. (This does NOT
  mean anything was committed or pushed — that's still all on you in step 4/5.)
- `blocked_uncommitted_changes` — there is work to commit (`stagedCount` /
  `unstagedCount` > 0). Commit + push it (step 4), then re-check or proceed.
- `blocked_merge_conflicts` — the branch has conflicts. This is real file work, and you
  NEVER edit files — see "Conflicts / runner failures" below.
- `blocked_repo_busy` — a git operation is in flight. Wait briefly and re-run step 3; do
  not push into a busy repo.
- `no_runners_detected` — no test/lint runner was found. Not fatal — proceed, but tell
  the user verification is thin (nothing ran the suite). `detectedRunners` lists what, if
  anything, was found.

Use `currentBranch` from this result as the `head` for the PR-create in step 5. Read the
verdict and route; everything below depends on which one you got.

## Step 4 — Commit and push (the REAL prep — confirmed; via daintree.call)
There is NO typed `git.commit` / `git.push` / `getStagingStatus` wrapper — those go
through the raw escape hatch. Do this only when there is work to commit (verdict
`blocked_uncommitted_changes`, or `ready` with pending changes the user wants in):
- `daintree.call git.commit { cwd?, message: "..." }` — `message` is REQUIRED and must be
  non-empty/non-whitespace (the call throws on an empty message). Confirmed.
- `daintree.call git.push { cwd?, setUpstream?: true }` — pass `setUpstream: true` the
  FIRST time you push a brand-new local branch, or the push fails with no upstream.
  Confirmed.
After committing, the tree is clean; you do not need to re-run `prepBranchForReview`
unless you want to re-confirm `ready` before opening the PR.

## Step 5 — Open the PR (confirmed)
Forge WRITES have no typed wrapper — go through the escape hatch:
`daintree.call forge.createPR { head: currentBranch, base: "<target>", title: "...", body?, draft? }`.
This is the one forge mutation here that RETURNS the PR object (`{ number, url, ... }`) —
most forge writes return void — so capture `number`/`url` now rather than re-listing.
Confirmed (external risk). If step 2 found an existing open PR on this head, skip create;
the push already updated it. To flip a draft to ready, `daintree.call forge.markPRReadyForReview`.

## Conflicts / runner failures — spawn and supervise, don't self-fix
`prepBranchForReview` is a READ — it cannot fix anything. If the verdict is
`blocked_merge_conflicts`, or a detected runner failed and the user wants it green before
review, that is edit work: spawn a visible agent with `agentTask.spawnForEdits`
mode:"edit" and supervise it. Pick ONE supervision mode, never both:
- BACKGROUND (fire-and-forget): spawn WITH a watcher (`watch: true`, `watchGoal: "...resolve
  the conflicts / fix the failing checks, then I'll prep the PR"`), then END your turn.
  React on the watcher's `completed_*` event.
- IN-TURN (you drive it now): spawn with NO watcher, then `terminal.awaitAll({
  terminalIds: [...] })` -> `{ allFinished, perTerminal: [{ terminalId, status }] }`
  (status ∈ finished | failed | question | working), read its output with one
  `terminal.extract` / `terminal.summarize`, and relay with `terminal.sendCommand`.
Finish is NEVER a bare `waiting` (an agent shows waiting before it starts, when paused,
or when backgrounded) and NEVER a transient `completed` (it bounces to waiting/working in
seconds — don't wait to see it). Trust only a watcher `completed_*` event or
`terminal.awaitAll` / `terminal.extract wait:{}` (each runs a small-model tail check).
Once the agent has fixed the branch, return to step 3 (re-check) → step 4/5.

## Report back
State the verdict you got, what you committed/pushed, and the PR number + url — or, if
blocked, exactly what is blocking and the next action (and, if you spawned a fixer agent,
its terminalId + watcherId and that supervision runs only while the assistant is open).
Never claim "the PR is ready" off `prepBranchForReview` alone: that verdict only means the
tree is in a committable shape, not that a PR exists.
