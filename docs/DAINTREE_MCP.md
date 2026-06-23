# Daintree MCP — integration notes

The CLI connects to Daintree's **local MCP server** as an external client. In production
Daintree launches the CLI and passes the connection details via environment / flags.

> **Source of truth.** The CLI-side verified contract lives in
> [`internal/models/prompts`](../internal/models/prompts) — it is embedded in the
> cached system prompt, so it is what the model actually reasons against. This doc is
> the human-facing companion; keep the two in sync. If they ever disagree, the prompt
> source wins.

## Connection

| Setting        | Value                                                        |
| -------------- | ----------------------------------------------------------- |
| Transport      | Streamable HTTP (primary) at `/mcp`; legacy SSE at `/sse`    |
| Host/port      | `127.0.0.1:45454` (default; configurable)                   |
| URL env var    | `DAINTREE_MCP_URL` (e.g. `http://127.0.0.1:45454/mcp`)      |
| Token env var  | `DAINTREE_MCP_TOKEN` (bearer)                               |
| Project id     | `DAINTREE_PROJECT_ID` (optional)                            |
| Window id      | `DAINTREE_WINDOW_ID` (Daintree-injected; identifies the bound window) |
| Auth header    | `Authorization: Bearer <token>`                            |

`DAINTREE_WINDOW_ID` is injected by Daintree so a CLI bound to one Daintree window
can be told apart from another. It is read into config (`AppConfig.WindowID`, env-only —
never a CLI flag) and surfaced by `/status` via `config.DescribeConfig`. (`/doctor`
reports connection health and a live MCP probe rather than the raw config values.)

The client (`internal/mcp`) uses `github.com/modelcontextprotocol/go-sdk`: it connects
over the Streamable HTTP transport with an `Authorization: Bearer <token>` header and
falls back to the legacy SSE transport on failure.

`ListTools` → tools with `{ name, description, inputSchema }`.
`CallTool(name, arguments)` → content text plus optional `structuredContent` and an
`isError` flag, normalized into a `domain.ToolResult`.

## Tiers

`workbench` (read/safe) ⊂ `action` (+mutations) ⊂ `system` (+dangerous). External API-key
clients get an `external` tier (~70 read-safe + creation tools, no dangerous mutators like
`worktree.delete`). The CLI treats the tier as advisory and adds its OWN safety layer on top.

## Representative action / tool ids (call via `daintree.call`)

Terminals: `terminal.list`, `terminal.new`, `terminal.getOutput`, `terminal.getStatus`,
`terminal.sendCommand`, `terminal.inject`, `terminal.waitUntilIdle`, `terminal.rename`,
`terminal.armByState`.

> `terminal.waitUntilIdle` blocks on **one** terminal — targeted use only; never fan out
> many concurrent waits. Batch reads by passing several `terminalIds` to
> `terminal.getStatus` instead. There is **no** `terminal.listStatus` and **no**
> `terminal.waitForAny`.

Worktrees: `worktree.list`, `worktree.getCurrent`, `worktree.createWithRecipe`,
`worktree.delete`, `worktree.setActive`, `worktree.refresh`, `worktree.listBranches`,
`worktree.compareDiff` (read-only: the files that differ between two worktrees' branches).
Resource lifecycle: `worktree.resource.provision`, `worktree.resource.teardown`,
`worktree.resource.pause`, `worktree.resource.resume`, `worktree.resource.status`,
`worktree.resource.connect`.

Git: `git.getProjectPulse`, `git.getStagingStatus`, `git.commit`, `git.push`,
`git.stageFile`, `git.unstageFile`, `git.snapshotList`, `git.snapshotRevert`.

Forge (reads): `forge.listIssues`, `forge.listPRs`, `forge.getIssue`, `forge.getPR`.
Forge (issue writes): `forge.createIssue`, `forge.closeIssue`, `forge.reopenIssue`,
`forge.editIssue`, `forge.addIssueComment`, `forge.addIssueLabel`,
`forge.removeIssueLabel`, `forge.assignIssue`, `forge.unassignIssue`.
Forge (PR writes): `forge.createPR`, `forge.closePR`, `forge.reopenPR`, `forge.mergePR`,
`forge.convertPRToDraft`, `forge.markPRReadyForReview`, `forge.commentOnPR`, `forge.editPR`.
Forge (review writes): `forge.approvePR`, `forge.requestChanges`, `forge.dismissReview`,
`forge.requestReviewers`. All forge writes are `external`-risk and always confirmed.

> The `forge.open*` actions (`forge.openIssue`, `forge.openPR`, `forge.openIssues`,
> `forge.openPRs`, `forge.openCommits`) exist on the Daintree server but are
> **renderer/UI-only** — they open a browser/editor window via Electron IPC and do
> nothing useful for a headless MCP client, so the CLI deliberately omits them. Use the
> `forge.list*` / `forge.get*` reads instead.

Agents/Recipes: `agent.launch`, `agent.terminal`, `recipe.list`, `recipe.run`.

Code/Files (Daintree-side): `copyTree.generate`, `copyTree.injectToTerminal`,
`files.search`, `file.view`, `file.openInEditor`.

Meta: `actions.list`, `actions.getContext`, `actions.search`, `actions.getSchema`.

### Verified call/response shapes

- `terminal.getStatus({ terminalIds: string[] (1–256), includeOutput?: { lines 1–50, stripAnsi } })`
  → `{ terminals: [{ terminalId, agentId, agentState, waitingReason?, exitCode?, spawnedAt?, lastTransitionAt?, recentOutput? }] }`.
  There is **no** flat `agentState` and **no** `runtimeStatus`. `exitCode` (numeric)
  is present once a terminal has exited; `spawnedAt` / `lastTransitionAt` are epoch-ms
  timestamps. All three are read defensively (absent/non-numeric → treated as unset).
- `terminal.getOutput({ terminalId, maxLines 1–1000 })` → `{ terminalId, content, lineCount, truncated }`.
  Scrollback is in `content`.
- `agent.launch({ agentId, name?, worktreeId?, model?, prompt, requestKey })` →
  `{ terminalId, location }` **only** (no `worktreeId`, no `taskId` in the response).
  `model?` (optional string) overrides the model the spawned agent runs under; omit
  it to use the agent's default.
- `terminal.armByState({ state: "working"|"waiting"|"finished", scope?: "current"|"all"
  (default "current"), extend?: boolean })` arms every eligible agent terminal in the
  given state. The exposed action id is `terminal.armByState`, **not** `fleet.armByState`
  (the latter is an internal store call, not a callable tool).
- Terminal focus is **not** a `terminal.focus` MCP tool — Daintree uses
  `panel.focus({ panelId })` where the terminal id *is* the `panelId`. The local
  `terminal.focus` wrapper maps onto it.
- Read tools (workbench tier, no confirmation): `actions.getContext` / `list` / `search` /
  `getSchema`, `worktree.list`, `worktree.getCurrent`, `git.getProjectPulse`, `terminal.list`.
  `agent.launch` and `terminal.waitUntilIdle` are action tier (mutations confirm).

## State models (from Daintree)

`domain.AgentState` is one of: `idle` | `working` | `waiting` | `completed` | `exited`.

- `"directing"` exists in Daintree's renderer but is **renderer-only** — you will not see
  it over MCP, so the CLI must not depend on it.
- When `agentState` is `"waiting"`, `waitingReason` is `"prompt"` or `"question"`.
- Exit is the `"exited"` state; a numeric `exitCode` is then exposed. The CLI treats
  a nonzero code as failure evidence, **not** as a completion trust gate (completion
  trust still requires the git verification pass — see gaps below).
- There is no `agent.getState` tool. Agent state is exposed as the subscribable resource
  `daintree://agent/{agentId}/state`, keyed by **agent** id (not terminal id).

`actions.getContext` returns the active project / worktree / focused terminal snapshot.

### Resources

- `daintree://agent/{agentId}/state` — subscribable, keyed by agent id.
- `daintree://terminal/{id}/scrollback`
- `daintree://worktree/{id}/pulse`

## Idempotency

Mutating tools accept a `requestKey` for dedup (`terminal.new`, `worktree.createWithRecipe`,
`agent.launch`, `recipe.run`, `git.commit`, etc.). The CLI sends an idempotency key for every
autonomous mutation. Daintree dedupes on `requestKey` and strips it before validation, so it
never trips schema checks.

## Confirmation

`danger: "confirm"` tools may trigger an MCP elicitation. The CLI confirms with the user
locally BEFORE calling such tools (see safety/policy). `git.commit`, `git.push`, and
`worktree.delete` **always** require explicit confirmation.

If a call fails with `SESSION_BINDING_GONE` or `BINDING_STALE`, the bound Daintree window
is gone — stop retrying that session and tell the user.

## Known Daintree-side gaps

These are limitations in Daintree's MCP surface that the CLI works around locally. They are
**not fixable in this repo** — they are tracked here so the workarounds can be retired if and
when Daintree closes the gap.

1. **No test/lint completion signal.** `terminal.getStatus` now exposes a numeric `exitCode`
   (plus `spawnedAt` / `lastTransitionAt`), which the watcher consumes as signal evidence — a
   nonzero exit is surfaced as failure evidence on a `terminal_exited` event
   (`internal/daemon/watcher.go`). It is **not** a completion trust gate: there is still no
   test/lint runner signal, so the irreversible-action gate from **issue #3** continues to
   derive completion trust from a deterministic git-cleanliness check, not the exit code.
2. **`agent.launch` returns only `{ terminalId, location }`.** No `worktreeId` or `taskId`, so
   `internal/tools/agenttaskx` degrades gracefully (caller-supplied `worktreeId`, no `taskId`).
   If Daintree returns these, the spawn tool can stop guessing.
3. **`DAINTREE_WINDOW_ID` contract.** Daintree injects it but it had been referenced only inside
   a prompt string; it is now read into config so per-window/per-project state isolation can use
   it. This overlaps directly with **issue #4** (per-project state isolation) — #4 owns the
   richer per-window state-dir derivation; this repo only standardizes reading the env into config.
4. **No live capability probe in the protocol.** A healthy connection + non-empty tool list does
   not prove the token's tier can actually call anything. `/doctor` works around this by calling
   `actions.getContext` (workbench tier, read-only) as a functional probe. If Daintree adds a
   dedicated health/capability endpoint, the probe can target that instead.

For discovery beyond the lists above, use `tool.search` / `daintree.listTools` rather than
guessing tool names.

## Keeping the references in sync

There are three CLI-side records of the Daintree MCP surface; when Daintree's tool surface
changes, update them together:

1. **This doc** (`docs/DAINTREE_MCP.md`) — the human-facing companion. May list more raw
   Daintree tools than the model is told about (the catalog above is illustrative, not the
   model's allowlist).
2. **The model prompt** (`internal/models/prompts/daintree_mcp.go`, `daintreeMCPReference`)
   — the cached, model-facing reference. It names the local wrappers plus a few high-value
   unwrapped tools, and reminds the model to use `tool.search` for the rest. It deliberately
   does **not** enumerate every server tool.
3. **The drift baseline** — `DocumentedMCPToolNames` (prompts) and `DocumentedMcpToolNames`
   (`internal/mcp/tools.go`), kept identical and pinned by tests. This is an **exact,
   minimal, verified** subset: at startup the CLI checks each name is still on the live
   server (a missing one signals the doc went stale). Drift is *missing-only* — extra live
   tools (like `worktree.compareDiff` or the `worktree.resource.*` family) are expected and
   ignored, so they don't need to be added here to be callable via `daintree.call`.
