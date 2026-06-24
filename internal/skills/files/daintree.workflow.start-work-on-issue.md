---
id: daintree.workflow.start-work-on-issue
title: Start work on a forge issue
version: 0.2.1
summary: How to pick a forge issue and start work on it through workflow.startWorkOnIssue, which spawns the agent and auto-attaches a BACKGROUND supervisor watcher.
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
  - terminal.summarize
  - queue.resolve
  - watcher.cancel
  - daintree.call
---
Use when: the user wants to start work on a forge issue.

This is the one-call "begin this issue" path. workflow.startWorkOnIssue does a COMPLETE
synchronous workflow inside Daintree — fetch the issue, derive the branch, create the
worktree, SPAWN the work agent, (optionally) run a recipe first, best-effort inject
worktree context, (optionally) assign the issue — and then the assistant wrapper
auto-attaches ONE supervisor watcher to the spawned terminal (attachWatcher defaults
true). So it is BACKGROUND supervision by design: you set it up, end your turn, and
react when the watcher's completed_* event lands on the attention queue. Never also
drive this agent in-turn (terminal.awaitAll / terminal.sendCommand) and never attach a
second watcher — that puts two supervisors on one terminal (the classic way a quick job
sprawls into minutes of churn and duplicate notifications).

Procedure:

1. Orient first. context.snapshot for the worktrees list, terminals, and attention
   queue. Scan the worktree entries — each carries `issueNumber`/`prNumber` — so you
   DON'T start a duplicate worktree for an issue already in flight. If the project or
   target worktree is ambiguous, resolve it before mutating.

2. Identify the issue. If the user named a number, go straight to step 3. Otherwise
   list candidates with `forge.listIssues({ state: "open" })` -> `{ items: Issue[],
   nextCursor, hasMore }` and confirm WHICH one with the user. Then ground the task
   with `forge.getIssue({ issueNumber })` -> the Issue object (title, body, `labels[]`,
   `assignees[]`, `commentCount?`, …). Read it before mutating anything — it shapes the
   agent's prompt and tells you if it's already assigned.

3. Resolve the agentId — this is the silent-failure trap. startWorkOnIssue REQUIRES an
   `agentId`, and its internal spawn does NOT validate it: an unknown or mis-transcribed
   id still creates a terminal, but command generation fails silently -> a dead,
   zero-output panel the watcher then babysits forever. A name the user spoke or typed
   is only a HINT (voice transcription drops letters — "antigravity"->"antiravity"), so
   resolve it to a REAL configured id from the roster, which you read with
   `daintree.call agentSettings.get` -> `{ agents }` (keyed by agent id: claude, codex,
   gemini, antigravity, opencode, aider, …). Never pass an unverified name through.

4. Confirm, then launch. workflow.startWorkOnIssue mutates real state and touches the
   forge (external risk) — confirm per the active tier. The wrapper takes the Daintree
   workflow fields NESTED inside an `arguments` object, NOT as top-level keys (the
   top-level schema is strict — only `arguments`, `requestKey`, and `attachWatcher` are
   allowed there, and any stray top-level key is rejected as an unknown field). Call it
   with this LITERAL shape:

       workflow.startWorkOnIssue({
         "arguments": {
           "issueNumber": <int>,            // required
           "agentId": "<resolved id>",      // required
           "branchName": "<optional override>",
           "baseBranch": "<optional, e.g. main>",
           "assignToSelf": true,            // optional
           "injectContext": true,           // optional, best-effort worktree digest
           "recipeId": "<optional>"         // optional: run a recipe in the new worktree first
         },
         "attachWatcher": true,             // optional, assistant-side ONLY, defaults true
         "requestKey": "<optional>"         // optional idempotency key (sibling of arguments)
       })

   Put EVERY Daintree-side field (issueNumber, agentId, branchName, baseBranch,
   assignToSelf, injectContext, recipeId) INSIDE `arguments` — never flatten them to the
   top level. `attachWatcher` is assistant-side only (it controls the auto-watcher and is
   never forwarded to Daintree); leave it out to get the default-true watcher. `requestKey`
   is an optional top-level sibling forwarded to Daintree for idempotency (Daintree dedupes
   on it and strips it before its own validation) — optional, you don't need to invent one.
   If you set `recipeId`, the recipe's terminals spawn synchronously alongside the work
   agent; agent-sourced recipe terminals are capped at 3 (overflow -> failed). Those extra
   recipe terminals are UNSUPERVISED workspace setup — only the issue work agent gets the
   auto-watcher.

5. Read the return and report the REAL values. The call returns:

       { issueNumber, issueTitle, issueUrl, worktreeId, worktreePath, branch,
         terminalId, recipeLaunched, spawnedTerminalCount, failedTerminalCount,
         assignedToSelf, assignedUsername, assignmentError, contextInjected }

   and the assistant wrapper adds the attached `watcherId`. Use `terminalId` (NOT
   worktreeId) for any later follow-up. Report: which issue (issueTitle/issueUrl), the
   worktreePath + branch created, the terminalId, the watcherId, whether it was assigned
   (assignedToSelf / assignmentError), and whether context injected (contextInjected).
   Look ahead for partial failure — DON'T claim a clean launch if you see any of these:
   - `terminalId` is null, or a partialSuccessError surfaced (worktree made but the
     spawn/recipe failed) — say the agent did NOT start.
   - `failedTerminalCount > 0` — a recipe terminal failed; name it.
   - no `watcherId` / a watcherWarning — the watcher did NOT attach (Daintree/storage
     error). Say so; either retry the spawn, or supervise the terminal yourself without
     a tight read-loop (terminal.summarize once it has settled).

6. END your turn. The watcher now supervises the agent in the background and publishes
   to the attention queue when it settles — you do not wait inside the turn, and you do
   NOT hand-poll the terminal. Do NOT also call terminal.awaitAll / terminal.sendCommand
   here (that double-supervises the agent the watcher already owns). Tell the user the
   work is running and the next checkpoint is the watcher's completed_* event — and that
   supervision runs only while the assistant is open (foreground-only, session-scoped).

7. Why trust the watcher and not your own glance: the agent FSM's "completed" is
   TRANSIENT — working->completed fires only on a detected completion event, then
   bounces back to "waiting"/"working" within seconds, so a status poll rarely catches
   it and you must NEVER wait to see it. A bare "waiting" is ALSO not proof of
   completion — an agent reads "waiting" before it starts, when paused mid-task, or when
   Daintree backgrounds its window. The watcher's completed_* event is the only
   trustworthy signal because it confirms a real working->waiting (or exited) transition
   AND runs a small-model tail check before surfacing. A freshly spawned agent also
   prints NOTHING for several seconds — empty output right after launch means "not
   started yet", never "the terminal is gone".

8. React on the wake (the scheduler wakes you when the queue event arrives):
   - `completed_success` — the agent finished, the acceptance contract (where one was
     set) was met, and the worktree is clean. Relay the gist with terminal.summarize
     (the default — a coding agent's raw TUI scrollback is garbled; don't dump it), and
     offer the obvious next step (prepare the branch for review). Only after this
     trustworthy signal is it safe to propose an irreversible action (commit/push).
   - `completed_unverified` — the agent stopped but the work is UNVERIFIED (contract not
     met, uncommitted changes remain, or git state couldn't be read). Tell the user
     plainly and prompt them to review BEFORE proposing any commit/push/delete. Do not
     silently upgrade it to success.

9. Close the loop. A FINISHED watch has ALREADY stopped its own watcher — there is
   nothing to cancel, but its inbox item lingers and keeps the "needs attention" badge
   lit. After you relay the result, clear it with `queue.resolve {"id": "<inbox id>"}`
   (the wake-up note prints the id). Do NOT call `watcher.cancel` on an already-finished
   watch — it is refused as "already ended". Leave OPEN any item that still needs the
   user (an agent waiting on a question — `agentState "waiting"`, `waitingReason
   "question"` — which you answer with terminal.sendCommand). When instead you abandon a
   STILL-ACTIVE watch (you got what you needed early, or the task was dropped), cancel
   it with `watcher.cancel {"id": "<watcherId>"}` and say so plainly, noting you can
   re-attach later.

Prefer the typed wrapper over the raw daintree.call escape hatch for all of the above.

Confirmation: workflow.startWorkOnIssue mutates real state and touches the forge —
confirm before launch per the active tier (external risk).

Report back: which issue was started (title + url), the worktree path / branch / the
real terminalId and watcherId, whether it was assigned and context injected, any partial
failure, and that supervision runs only while the assistant is open — the next
checkpoint is the watcher's completed_* event, not a status you poll yourself.
