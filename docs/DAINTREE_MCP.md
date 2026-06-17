# Daintree MCP — integration notes

The CLI connects to Daintree's **local MCP server** as an external client. In production
Daintree launches the CLI and passes the connection details via environment / flags.

> **Source of truth.** The CLI-side verified contract lives in
> [`src/models/prompts/daintreeMcp.ts`](../src/models/prompts/daintreeMcp.ts) — it is
> embedded in the cached system prompt, so it is what the model actually reasons
> against. This doc is the human-facing companion; keep the two in sync. If they ever
> disagree, `daintreeMcp.ts` wins.

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
can be told apart from another. It is read into config (`AppConfig.windowId`, env-only —
never a CLI flag) and surfaced by `/status` via `describeConfig()`. (`/doctor` reports
connection health and a live MCP probe rather than the raw config values.)

Client uses `@modelcontextprotocol/sdk`:

```ts
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { SSEClientTransport } from "@modelcontextprotocol/sdk/client/sse.js";

const transport = new StreamableHTTPClientTransport(new URL(url), {
  requestInit: { headers: { Authorization: `Bearer ${token}` } },
});
await client.connect(transport); // fall back to SSEClientTransport on failure
```

`client.listTools()` → `{ tools: [{ name, description, inputSchema }] }`.
`client.callTool({ name, arguments })` → `{ content: [{ type:"text", text }], structuredContent?, isError? }`.

## Tiers

`workbench` (read/safe) ⊂ `action` (+mutations) ⊂ `system` (+dangerous). External API-key
clients get an `external` tier (~70 read-safe + creation tools, no dangerous mutators like
`worktree.delete`). The CLI treats the tier as advisory and adds its OWN safety layer on top.

## Representative action / tool ids (call via `daintree.call`)

Terminals: `terminal.list`, `terminal.new`, `terminal.getOutput`, `terminal.getStatus`,
`terminal.sendCommand`, `terminal.inject`, `terminal.waitUntilIdle`, `terminal.rename`.

> `terminal.waitUntilIdle` blocks on **one** terminal — targeted use only; never fan out
> many concurrent waits. Batch reads by passing several `terminalIds` to
> `terminal.getStatus` instead. There is **no** `terminal.listStatus` and **no**
> `terminal.waitForAny`.

Worktrees: `worktree.list`, `worktree.getCurrent`, `worktree.createWithRecipe`,
`worktree.delete`, `worktree.setActive`, `worktree.refresh`, `worktree.listBranches`.

Git: `git.getProjectPulse`, `git.getStagingStatus`, `git.commit`, `git.push`,
`git.stageFile`, `git.unstageFile`, `git.snapshotList`, `git.snapshotRevert`.

Forge: `forge.listIssues`, `forge.listPRs`, `forge.getIssue`, `forge.openIssue`,
`forge.openPR`, `forge.assignIssue`.

Agents/Recipes: `agent.launch`, `agent.terminal`, `recipe.list`, `recipe.run`.

Code/Files (Daintree-side): `copyTree.generate`, `copyTree.injectToTerminal`,
`files.search`, `file.view`, `file.openInEditor`.

Meta: `actions.list`, `actions.getContext`, `actions.search`, `actions.getSchema`.

### Verified call/response shapes

- `terminal.getStatus({ terminalIds: string[] (1–256), includeOutput?: { lines 1–50, stripAnsi } })`
  → `{ terminals: [{ terminalId, agentId, agentState, waitingReason?, recentOutput? }] }`.
  There is **no** flat `agentState`, **no** `runtimeStatus`, and **no** `exitCode`.
- `terminal.getOutput({ terminalId, maxLines 1–1000 })` → `{ terminalId, content, lineCount, truncated }`.
  Scrollback is in `content`.
- `agent.launch(...)` → `{ terminalId, location }` **only** (no `worktreeId`, no `taskId`).
- Terminal focus is **not** a `terminal.focus` MCP tool — Daintree uses
  `panel.focus({ panelId })` where the terminal id *is* the `panelId`. The local
  `terminal.focus` wrapper maps onto it.
- Read tools (workbench tier, no confirmation): `actions.getContext` / `list` / `search` /
  `getSchema`, `worktree.list`, `worktree.getCurrent`, `git.getProjectPulse`, `terminal.list`.
  `agent.launch` and `terminal.waitUntilIdle` are action tier (mutations confirm).

## State models (from Daintree)

```ts
type AgentState = "idle" | "working" | "waiting" | "completed" | "exited";
```

- `"directing"` exists in Daintree's renderer but is **renderer-only** — you will not see
  it over MCP, so the CLI must not depend on it.
- When `agentState` is `"waiting"`, `waitingReason` is `"prompt"` or `"question"`.
- Exit is the `"exited"` state; **no numeric exit code is exposed** (see gaps below).
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

1. **No exit code / completion metadata.** `terminal.getStatus` exposes `agentState` but no
   numeric exit code, so the watcher infers success purely from `agentState === "completed"`
   (`src/daemon/watcherEngine.ts`). This is the shared root cause behind the irreversible-action
   gate work in **issue #3** — if Daintree exposes a real exit code, both the watcher inference
   and #3's gate can rely on it instead of the agentState heuristic.
2. **`agent.launch` returns only `{ terminalId, location }`.** No `worktreeId` or `taskId`, so
   `src/tools/agentTaskTools.ts` degrades gracefully (caller-supplied `worktreeId`, `taskId`
   undefined). If Daintree returns these, the spawn tool can stop guessing.
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
