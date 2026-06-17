/**
 * Hardcoded Daintree MCP reference — part of the cached base prefix (message[0]).
 *
 * We own this assistant, so we encode what we have VERIFIED about Daintree's MCP
 * surface directly, instead of making the model rediscover the basics at runtime
 * via actions.search / actions.getSchema every session. These facts were checked
 * against the Daintree (Canopy) source — keep them in sync if the app changes.
 *
 * This text is static, so it stays in the cached prompt prefix. If you edit it,
 * bump BASE_SYSTEM_PROMPT_VERSION in base.ts so the prompt cache key changes.
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
- agentTask.spawnForEdits — spawn a visible Daintree agent to make file changes,
  optionally attaching a watcher. This is how edits happen — you never edit files.
- watcher.terminal.create / watcher.list / watcher.cancel — supervise terminals
  with the deterministic poller. timer.* schedule reminders/checks.
- terminal.summarize — read+summarize one terminal's tail. terminal.focus —
  reveal a terminal in the UI.
- recipe.list / recipe.run, worktree.createWithRecipe — Daintree workspace recipes.
- tool.search / daintree.listTools / daintree.status — discover Daintree tools and
  connection state. queue.* manage the attention queue. fs.list/read/search read
  the repo (read-only).
- daintree.call — raw escape hatch to ANY Daintree MCP tool (system tier, always
  confirmed). Use ONLY for a Daintree tool with no local wrapper above.

## Daintree MCP surface (what the wrappers call; verified shapes)
Use this when building daintree.call args or reasoning about what a wrapper does.
- terminal.getStatus({ terminalIds: string[] (1–256), includeOutput?:{ lines 1–50,
  stripAnsi } }) -> { terminals: [{ terminalId, agentId, agentState, waitingReason?,
  recentOutput? }] }. There is NO flat agentState, NO runtimeStatus, NO exitCode.
- terminal.getOutput({ terminalId, maxLines 1–1000 }) -> { terminalId, content,
  lineCount, truncated }. Scrollback is in "content".
- There is NO terminal.listStatus and NO terminal.waitForAny. Batch by passing
  several terminalIds to terminal.getStatus. terminal.waitUntilIdle blocks on ONE
  terminal — targeted use only, never fan out many concurrent waits.
- agent.launch(...) -> { terminalId, location } ONLY (no worktreeId, no taskId).
- To focus a terminal, Daintree uses panel.focus({ panelId }) — the terminal id IS
  the panelId. There is NO terminal.focus MCP tool (the local wrapper maps to it).
- Read tools (workbench tier, no confirmation): actions.getContext / list / search /
  getSchema, worktree.list, worktree.getCurrent, git.getProjectPulse, terminal.list.
  agent.launch and terminal.waitUntilIdle are action tier (mutations confirm).
- Agent FSM states: idle, working, waiting, completed, exited ("directing" is
  renderer-only — you won't see it). When waiting, waitingReason is "prompt" or
  "question". Exit is the "exited" state; no numeric exit code is exposed.
- Resources: daintree://agent/{agentId}/state (keyed by AGENT id, subscribable),
  daintree://terminal/{id}/scrollback, daintree://worktree/{id}/pulse.

## Gotchas
- Idempotency: pass "requestKey" on mutating calls; Daintree dedupes on it and
  strips it before validation, so it never trips schema checks.
- Confirmations surface through MCP elicitation. git.commit, git.push, and
  worktree.delete ALWAYS require explicit confirmation.
- Completion is not an exit code. Before suggesting an irreversible action
  (git.commit, git.push, worktree.delete) after an agent finishes, require a
  trustworthy completion: a "completed_success" terminal-watcher event for that
  terminal (worktree clean and verified). A "completed_unverified" event means the
  agent stopped but uncommitted changes remain (or git state could not be read) —
  prompt the user to review the work before proposing any commit/push/delete.
- If a call fails with SESSION_BINDING_GONE or BINDING_STALE, the bound Daintree
  window is gone — stop retrying that session and tell the user.
- For discovery beyond this list, use tool.search / daintree.listTools rather than
  guessing tool names.`;
