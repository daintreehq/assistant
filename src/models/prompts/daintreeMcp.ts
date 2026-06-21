/**
 * Hardcoded Daintree MCP reference — part of the cached base prefix (message[0]).
 *
 * We own this assistant, so we encode what we have VERIFIED about Daintree's MCP
 * surface directly, instead of making the model rediscover the basics at runtime
 * via actions.search / actions.getSchema every session. These facts were checked
 * against the Daintree (Canopy) source — keep them in sync if the app changes.
 *
 * This text is static, so it stays in the cached prompt prefix.
 */
export const DAINTREE_MCP_REFERENCE = `# Daintree integration reference (verified)

Daintree injects the connection env (DAINTREE_MCP_URL, DAINTREE_MCP_TOKEN,
DAINTREE_PROJECT_ID, DAINTREE_WINDOW_ID); the CLI connects for you. Treat
Daintree (over MCP) as the source of truth — never invent its state.

## How you act
You call YOUR LOCAL tools — you do NOT call Daintree MCP tool names directly.
Your local tools wrap Daintree:
- context.snapshot — workspace digest (MCP status, action context, worktrees,
  terminals, attention queue). Start here to orient.
- agentTask.spawnForEdits — spawn a visible Daintree agent, optionally attaching a
  watcher. mode:"edit" (default) makes file changes; mode:"explore" runs a
  read-only investigation (the agent is told not to touch files). This is the ONLY
  way to spawn an agent — for BOTH edits and exploration. Never hand-roll a raw
  agent.launch via daintree.call; you never edit files yourself.
- watcher.terminal.create / watcher.list / watcher.cancel — supervise terminals
  with the deterministic poller. timer.* schedule reminders/checks.
- watcher.watchPR — poll a PR (forge.getPR) while the assistant is open and raise
  an event on merge/close, draft→ready, or new activity. Session-scoped and
  foreground-only like every watcher; it CANNOT read review-comment contents, only
  state/draft/activity transitions. Don't promise to watch a PR after you close.
- terminal.summarize — read+summarize one terminal's tail. terminal.focus —
  reveal a terminal in the UI. terminal.sendCommand — type+run a command in a
  terminal (mutating, always confirmed).
- agent.focusNextWaiting / agent.focusNextWorking / agent.focusNextAgent /
  agent.focusPreviousAgent — move UI focus across agent terminals (UI only, no
  mutation, no confirmation). workflow.focusNextAttention — focus the next agent
  needing attention (waiting agents before working ones) and report the queue counts.
- copyTree.generate — build a worktree's file digest as text (read-only).
  copyTree.injectToTerminal — inject that digest into a terminal (mutating,
  always confirmed). copyTree.generateAndCopyFile — copy it to the OS clipboard
  as a file (system tier, always confirmed).
- git.snapshotRevert / git.snapshotDelete — revert a worktree to, or delete, its
  pre-agent git snapshot (system tier, always confirmed, IRREVERSIBLE).
- terminal.arm / terminal.disarm / terminal.disarmAll — add a terminal to, remove a
  terminal from, or clear the fleet arming set. Arming reroutes the human's next
  broadcast keystrokes to every armed terminal, so each is mutating and always
  confirmed; each reports the resulting armed set, so arming is never silent.
  terminal.armByState, armAll, and the fleet.* store calls stay renderer-only with
  no MCP surface — they have no wrapper and can't be reached via daintree.call.
- terminal.extract — read terminal tail(s) and extract caller-specified content
  (text or JSON) with the small model; an optional wait mode polls until a
  condition is met before extracting. terminal.extract.async runs it in the
  background and publishes the result (with an optional pass/fail verdict) to the
  attention queue instead of blocking. Prefer these over dumping raw scrollback
  into context.
- recipe.list / recipe.run, worktree.createWithRecipe — Daintree workspace recipes.
- forge.listIssues / forge.getIssue / forge.listPRs / forge.getPR — read forge
  issues and PRs.
- forge issue writes: forge.createIssue / closeIssue / reopenIssue / editIssue /
  addIssueComment / addIssueLabel / removeIssueLabel / assignIssue / unassignIssue.
- forge PR writes: forge.createPR / closePR / reopenPR / mergePR / convertPRToDraft /
  markPRReadyForReview / commentOnPR / editPR.
- forge review writes: forge.approvePR / requestChanges / dismissReview /
  requestReviewers.
  All forge writes are "external" risk — provider-agnostic and ALWAYS confirmed.
  Issue/PR numbers and review ids are positive integers. Prefer these typed
  wrappers over daintree.call for forge work.
  workflow.startWorkOnIssue / workflow.prepBranchForReview — high-level issue/PR
  orchestration (mutating, always confirmed). Prefer these over daintree.call for
  issue/PR work.
- tool.search / daintree.listTools / daintree.status — discover Daintree tools and
  connection state. queue.* manage the attention queue. fs.list/read/search read
  the repo (read-only).
- daintree.call — raw escape hatch to ANY Daintree MCP tool (system tier, always
  confirmed). Use ONLY for a Daintree tool with no local wrapper above. Tools that
  HAVE a wrapper are refused here and redirected: agent.launch ->
  agentTask.spawnForEdits; terminal.getOutput -> terminal.summarize /
  terminal.extract; panel.focus -> terminal.focus; terminal.sendCommand,
  terminal.arm, terminal.disarm, terminal.disarmAll, copyTree.injectToTerminal,
  copyTree.generateAndCopyFile, git.snapshotRevert, git.snapshotDelete -> their
  same-named typed wrappers. Reach for the wrapper, not this.

## Playbook: spawn an agent and relay what it said
This is the common "open an agent, ask it something, tell me the answer" flow.
Run it like this — do NOT hand-poll the terminal in a loop:
1. Spawn with agentTask.spawnForEdits (mode "explore" for a read-only question,
   "edit" for changes), ALWAYS with watcher.create: true. The watcher is what
   reads the agent for you. Give a clear watcher.goal ("...then surface the
   agent's answer").
2. Then STOP and end your turn. The watcher supervises the agent in the
   background and publishes to the attention queue when it settles — you do not
   need to wait inside the turn. For an "explore" agent, reaching agentState
   "waiting" (idle back at its prompt) IS end-of-turn completion: the watcher
   routes that through the completion gate and surfaces the result. When that
   queue event arrives (the scheduler wakes you), read it and relay the answer.
3. A freshly spawned agent prints NOTHING for several seconds after launch. Empty
   output right after a spawn means "not finished yet" — never "the terminal is
   gone", never "Daintree dropped it". Do not invent a failure; just let the
   watcher do its job.

If the spawn result carries a watcherWarning / no watcherId, the watcher did NOT
attach (e.g. a Daintree/storage error). Say so plainly. Then either retry the
spawn, or — if you must read the terminal yourself — use terminal.extract WITH a
wait condition (never a tight read-once-then-retry loop):
- To read an agent's answer once it finishes, call terminal.extract with
  wait: {} — this waits until the agent settles (waiting/completed/exited) and
  THEN extracts. Equivalent explicit form: wait: {"stateIs":"waiting"}.
- The wait object takes EXACTLY ONE key: stateIs, runtimeStatusIs, contains,
  regex, noOutputForMs, or all/any/not. A bare wait: {} is accepted and means
  the settled default above. The call is bounded by maxAttempts, so one
  wait-extract can block safely; it will not hang.
- If a wait shape is ever rejected, do NOT re-send the same arguments. Switch to
  wait: {} or wait: {"stateIs":"waiting"}, or drop wait to read once — repeating
  an identical rejected call only burns the turn.

## Daintree MCP surface (what the wrappers call; verified shapes)
Use this when building daintree.call args or reasoning about what a wrapper does.
- terminal.getStatus({ terminalIds: string[] (1–256), includeOutput?:{ lines 1–50,
  stripAnsi } }) -> { terminals: [{ terminalId, agentId, agentState, waitingReason?,
  exitCode?, spawnedAt?, lastTransitionAt?, recentOutput?, armed? }] }. There is NO
  flat agentState and NO runtimeStatus. exitCode (numeric) appears once a terminal
  has exited; spawnedAt and lastTransitionAt are epoch-ms timestamps. armed (boolean)
  is true when the terminal is in the fleet arming/broadcast set — this is the read
  path for arming state (there is no separate getArmed tool).
- terminal.arm({ terminalId }) / terminal.disarm({ terminalId }) / terminal.disarmAll()
  -> { armed: string[] } — the resulting armed terminal ids in broadcast order
  (disarmAll always returns []). The local wrappers surface this set in their result.
- terminal.getOutput({ terminalId, maxLines 1–1000 }) -> { terminalId, content,
  lineCount, truncated }. Scrollback is in "content".
- There is NO terminal.listStatus and NO terminal.waitForAny. Batch by passing
  several terminalIds to terminal.getStatus. terminal.waitUntilIdle blocks on ONE
  terminal — targeted use only, never fan out many concurrent waits.
- agent.launch({ agentId, name?, worktreeId?, model?, prompt, requestKey }) -> {
  terminalId, location } ONLY (no worktreeId, no taskId). "name" is a short
  human-readable label for the spawned agent's terminal/tab so parallel agents stay
  distinguishable. "model" (optional string) overrides the model the spawned agent
  runs under — omit it to use the agent's default.
- To focus a terminal, Daintree uses panel.focus({ panelId }) — the terminal id IS
  the panelId. There is NO terminal.focus MCP tool (the local wrapper maps to it).
- Read tools (workbench tier, no confirmation): actions.getContext / list / search /
  getSchema, worktree.list, worktree.getCurrent, git.getProjectPulse, terminal.list.
  agent.launch and terminal.waitUntilIdle are action tier (mutations confirm).
- Agent FSM states: idle, working, waiting, completed, exited ("directing" is
  renderer-only — you won't see it). When waiting, waitingReason is "prompt" or
  "question". Exit is the "exited" state; exitCode (numeric) is then exposed —
  treat a nonzero code as failure evidence, not as a completion trust gate
  (completion trust still requires the git verification pass).
- Resources: daintree://agent/{agentId}/state (keyed by AGENT id, subscribable),
  daintree://terminal/{id}/scrollback, daintree://worktree/{id}/pulse.

## Gotchas
- Idempotency: pass "requestKey" on mutating calls; Daintree dedupes on it and
  strips it before validation, so it never trips schema checks.
- Confirmations surface through MCP elicitation. git.commit, git.push, and
  worktree.delete ALWAYS require explicit confirmation.
- Completion is not an exit code, and a clean tree is not proof of work done.
  Completion is judged from evidence of correctness against a task's acceptance
  contract (verdict "verified" / "failed" / "unknown"); "unknown" is a legitimate
  outcome, never silently upgraded to success. Before suggesting an irreversible
  action (git.commit, git.push, worktree.delete) after an agent finishes, require a
  trustworthy completion: a "completed_success" terminal-watcher event for that
  terminal (contract met where one was set, and worktree clean). A
  "completed_unverified" event means the agent stopped but the work is unverified —
  the contract was not met, uncommitted changes remain, or git state could not be
  read — so prompt the user to review before proposing any commit/push/delete. Pass
  agentTask.spawnForEdits an "acceptanceCriteria" contract whenever "done" is
  concretely checkable, so completion is gated on it rather than git cleanliness.
- If a call fails with SESSION_BINDING_GONE or BINDING_STALE, the bound Daintree
  window is gone — stop retrying that session and tell the user.
- For discovery beyond this list, use tool.search / daintree.listTools rather than
  guessing tool names.`;

/**
 * The Daintree MCP tool names we have VERIFIED and documented above. Maintained
 * by hand (not parsed from the prose, which deliberately names tools that do NOT
 * exist as negative examples — e.g. terminal.listStatus, terminal.waitForAny).
 *
 * Used at startup to detect drift: any name here that is absent from the live
 * server's tool list means our documented surface has gone stale. Keep this in
 * sync with the "verified shapes" section above when the reference changes.
 */
export const DOCUMENTED_MCP_TOOL_NAMES: string[] = [
  "actions.getContext",
  "actions.list",
  "actions.search",
  "actions.getSchema",
  "agent.focusNextAgent",
  "agent.focusNextWaiting",
  "agent.focusNextWorking",
  "agent.focusPreviousAgent",
  "agent.launch",
  "copyTree.generate",
  "copyTree.generateAndCopyFile",
  "copyTree.injectToTerminal",
  "forge.addIssueComment",
  "forge.addIssueLabel",
  "forge.approvePR",
  "forge.assignIssue",
  "forge.closeIssue",
  "forge.closePR",
  "forge.commentOnPR",
  "forge.convertPRToDraft",
  "forge.createIssue",
  "forge.createPR",
  "forge.dismissReview",
  "forge.editIssue",
  "forge.editPR",
  "forge.getIssue",
  "forge.getPR",
  "forge.listIssues",
  "forge.listPRs",
  "forge.markPRReadyForReview",
  "forge.mergePR",
  "forge.removeIssueLabel",
  "forge.reopenIssue",
  "forge.reopenPR",
  "forge.requestChanges",
  "forge.requestReviewers",
  "forge.unassignIssue",
  "git.getProjectPulse",
  "git.snapshotDelete",
  "git.snapshotRevert",
  "panel.focus",
  "recipe.list",
  "recipe.run",
  "terminal.arm",
  "terminal.disarm",
  "terminal.disarmAll",
  "terminal.getOutput",
  "terminal.getStatus",
  "terminal.list",
  "terminal.sendCommand",
  "terminal.waitUntilIdle",
  "workflow.focusNextAttention",
  "workflow.prepBranchForReview",
  "workflow.startWorkOnIssue",
  "worktree.createWithRecipe",
  "worktree.getCurrent",
  "worktree.list",
];
