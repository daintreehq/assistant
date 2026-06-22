# Port spec: `src/mcp/client.ts` → Go (`internal/mcp`)

Authoritative reference for porting the **DaintreeMcpClient** subsystem to Go.
Implement from this file; do not re-read the TS. Source of truth: TS
`src/mcp/client.ts` (+ its imports from `src/reliability.ts`,
`src/models/prompts/daintreeMcp.ts`, and the env contract in `src/config.ts`).

---

## 1. What this subsystem does

Connects the assistant to Daintree's **local MCP server** and exposes a small,
degradation-tolerant API the rest of the app calls. Key behaviors:

- **Transport:** Streamable HTTP **primary**, legacy **SSE fallback**, both with a
  `Authorization: Bearer <token>` header.
- **Graceful degradation:** if no URL/token is configured, offline mode is set, or
  the connection fails, the client never throws on construction/connect — it records
  a `lastError` and reports `connected:false`. Tools that need Daintree then fail
  cleanly with `MCP_UNAVAILABLE`.
- **Tool-list caching** (warmed once on connect) + **drift detection** (documented
  tool names absent from the live server, warning-only).
- **Normalized call result:** flattened `text` + raw `content[]` +
  `structuredContent` + `isError`.
- **Read-only retry/timeout** for transient transport hiccups (opt-in, never for
  mutating calls).
- A **test injection seam** (`clientOverride`) so no network is needed in tests.

The `actions.getContext` tool is the canonical **doctor probe** (read-only,
workbench tier) and the **project-name** source — both live in *callers*, not in
this file, but are part of this subsystem's contract (§9).

---

## 2. Exported types / interfaces (exact)

> Go note: these are TS `interface`s = data shapes. Map to Go structs. Optional TS
> fields (`?`) → pointer or omitempty / zero-value-with-presence as noted.

### `McpToolInfo`
| Field | TS type | Notes |
|---|---|---|
| `name` | `string` | required |
| `description` | `string?` | optional |
| `inputSchema` | `Record<string, unknown>` | JSON object; defaulted (see below) |

When a live tool returns no `inputSchema`, the client substitutes
`{ type: "object", properties: {} }`. Preserve this default exactly.

### `McpCallResult`
| Field | TS type | Notes |
|---|---|---|
| `text` | `string` | flattened text of all text content blocks, joined by `"\n"` |
| `content` | `unknown[]` | raw content blocks verbatim (default `[]`) |
| `structuredContent` | `unknown?` | optional; pass through verbatim |
| `isError` | `boolean` | `Boolean(res.isError)` — defaults `false` |

### `McpRequestOptions` (SDK-facing)
| Field | TS type | Notes |
|---|---|---|
| `signal` | `AbortSignal?` | cancellation |
| `timeout` | `number?` | per-request ms; SDK rejects with `-32001` RequestTimeout `McpError` when exceeded |

Go: replace `signal` with `context.Context`; `timeout` with a per-call deadline.

### `McpCallOptions` (caller-facing knobs for one `callTool`)
| Field | TS type | Default | Notes |
|---|---|---|---|
| `timeoutMs` | `number?` | unset | per-request timeout |
| `retries` | `number?` | `0` | **read-only callers only**; clamped via `Math.max(0, …)` |

`retries` defaults to 0 so a mutating tool (sendInput, git ops) is never silently
re-issued — a retried mutation could double-apply. **Preserve this default.**

### `LowLevelMcpClient` (the SDK subset we depend on; also the test-fake surface)
```
listTools(params?, options?: McpRequestOptions)
  -> { tools: Array<{ name: string; description?: string; inputSchema?: unknown }> }
callTool(args: { name: string; arguments?: Record<string,unknown> },
         resultSchema?, options?: McpRequestOptions)
  -> { content?: unknown[]; structuredContent?: unknown; isError?: boolean }
close?(): Promise<void>     // optional
```
Go: define an interface (e.g. `LowLevelClient`) with `ListTools`, `CallTool`,
`Close`. This is the **mockable seam** for tests — keep it.

### `McpClientOptions`
| Field | TS type | Notes |
|---|---|---|
| `clientOverride` | `LowLevelMcpClient?` | pre-built, already-connected low-level client (tests). When set: client is `connected=true`, `transport="injected"` from construction. |

### `McpStatus`
| Field | TS type | Notes |
|---|---|---|
| `connected` | `boolean` | |
| `url` | `string?` | mirrors `cfg.mcpUrl` |
| `transport` | `"streamable-http" \| "sse" \| "injected" \| "none"` | enum — exact value set |
| `toolCount` | `number?` | `toolCache?.length` (undefined when cache cold) |
| `error` | `string?` | last error message |
| `driftWarnings` | `string[]?` | human sentences; **undefined when empty** (never `[]`) |
| `driftToolNames` | `string[]?` | bare names, **same order** as `driftWarnings`; undefined when empty |
| `serverInfo` | `{ name?: string; version?: string }?` | server's reported implementation info |

`status()` returns **defensive copies** of the slices/objects (`[...]`, `{...}`),
and collapses empty arrays to `undefined`. Replicate: in Go return copies and
`nil` (not empty slice) when there is no drift.

### `DaintreeMcpClient` (the class)
Private state to port to struct fields:
| Field | TS type | Notes |
|---|---|---|
| `cfg` | `AppConfig` | |
| `low` | `LowLevelMcpClient?` | active low-level client |
| `raw` | `Client?` | typed SDK client kept **only** to read server metadata (`getServerVersion`). Undefined when `clientOverride` injected. |
| `connected` | `boolean` | default `false` |
| `transportKind` | `McpStatus["transport"]` | default `"none"` |
| `lastError` | `string?` | |
| `toolCache` | `McpToolInfo[]?` | undefined = cold |
| `driftWarnings` | `string[]` | default `[]` |
| `driftToolNames` | `string[]` | default `[]` |
| `serverInfo` | `{ name?; version? }?` | |

### `McpUnavailableError`
- `extends Error`, `name = "McpUnavailableError"`, **`code = "MCP_UNAVAILABLE"`** (readonly).
- Constructed with the current `lastError` (or `"Daintree MCP is not connected"`).
- Go: a sentinel error type carrying `Code = "MCP_UNAVAILABLE"` + message. Callers
  distinguish it from transport errors (e.g. `errors.As`), so it MUST be a distinct
  type, not a string. Used to decide *not* to degrade the connection (see §6).

### `DAINTREE_GRANT_TOOL_NAMES`
`readonly string[]` — **empty today** (`[]`). Forward-compatible seam: when Daintree
ships an external grants API, populate with real tool names and
`hasDaintreeGrantSupport()` lights up. Keep it an **exact allowlist**, not a heuristic.
Port as an exported package var/const slice (currently empty).

---

## 3. Exported functions / methods

### `toolsAdvertiseGrantSupport(tools: {name}[], grantToolNames = DAINTREE_GRANT_TOOL_NAMES): boolean`
Pure predicate. Returns `false` immediately if `grantToolNames` is empty (today,
always). Otherwise: build a `Set` of live tool names, return whether **any**
grant-tool name is present. Exported so the seam is unit-testable without a live
connection. Keep the empty-list short-circuit (`length === 0 → false`).

### `DaintreeMcpClient` methods
| Method | Signature (TS) | Behavior (1–2 lines) |
|---|---|---|
| `constructor` | `(cfg, opts={})` | Store cfg. If `opts.clientOverride`: set `low`, `connected=true`, `transport="injected"`. **No cache warm here.** |
| `isConnected()` | `→ boolean` | returns `connected`. |
| `hasDaintreeGrantSupport()` | `→ boolean` | `false` if not connected; else `toolsAdvertiseGrantSupport(toolCache ?? [])`. Observational only — never opens a connection, never an auth gate. |
| `status()` | `→ McpStatus` | Snapshot with defensive copies; empty drift arrays → undefined. |
| `connect()` | `async → McpStatus` | **Never throws.** See §4. |
| `reconnect()` | `async → McpStatus` | `close()` then reset all state, then `connect()`. See §4. |
| `warmToolCache()` (private) | `async → void` | Calls `listTools(true)` + `runDriftCheck()`; on failure restores pre-call `connected`/`lastError` (best-effort, must not flip a healthy transport to degraded). |
| `runDriftCheck()` (private) | `→ void` | Records server info + missing-documented-tool warnings. Never throws, never affects `connected`. See §7. |
| `ensure()` (private) | `→ LowLevelMcpClient` | Throws `McpUnavailableError(lastError ?? "Daintree MCP is not connected")` when not connected / no `low`. |
| `markDegraded(e)` (private) | `→ void` | Sets `connected=false`, clears cache/drift/serverInfo, `lastError = errMsg(e)`. |
| `listTools(force=false, signal?, timeoutMs?)` | `async → McpToolInfo[]` | Cache-first unless `force`. See §5. |
| `callTool(name, args={}, signal?, opts={})` | `async → McpCallResult` | Dispatch + retry + normalize. See §6. |
| `close()` | `async → void` | `low?.close?.()` wrapped in try/catch (ignore errors); set `connected=false`. |

### Helper
`errMsg(e: unknown): string` — `e instanceof Error ? e.message : String(e)`. Port as
a small `errMsg(err error) string`.

---

## 4. `connect()` / `reconnect()` exact control flow

`connect()` (must never throw — always returns `status()`):
1. If already `connected`:
   - If `transport === "injected"` **and** `toolCache` is cold → `await warmToolCache()`
     (injected client is "connected" from construction but cache never warmed).
   - return `status()`.
2. If `cfg.offline` → `lastError = "offline mode"`, return `status()`.
3. If `!cfg.mcpUrl || !cfg.mcpToken` → `lastError = "DAINTREE_MCP_URL / DAINTREE_MCP_TOKEN not set"`, return `status()`.
4. Parse `cfg.mcpUrl` as a URL. On parse failure: `lastError = "invalid DAINTREE_MCP_URL: <msg>"`, `connected=false`, return.
5. `headers = { Authorization: "Bearer " + cfg.mcpToken }`.
6. **Try Streamable HTTP:** new `Client({name:"daintree-assistant-cli", version:"0.1.0"}, {capabilities:{}})`,
   `StreamableHTTPClientTransport(url, { requestInit: { headers } })`, `connect`. On success:
   set `raw`, `low`, `connected=true`, `transport="streamable-http"`, `lastError=undefined`,
   `warmToolCache()`, return.
7. **On HTTP failure, try SSE fallback:**
   - Build `sseUrl` = clone of `url`, with `pathname` rewritten:
     **`pathname.replace(/\/mcp\/?$/, "/sse")`** — i.e. a trailing `/mcp` or `/mcp/`
     becomes `/sse`. (If the path doesn't end in `/mcp`, it's left unchanged.)
   - Same client name/version, `SSEClientTransport(sseUrl, { requestInit: { headers } })`,
     `connect`. On success: `transport="sse"`, etc. (same as step 6).
   - On SSE failure too: `lastError = "streamable-http: <httpErr>; sse: <sseErr>"`,
     `connected=false`, return. **Both error messages are concatenated in this exact format.**

`reconnect()`:
1. `await close()`.
2. Reset: `connected=false`, `toolCache=undefined`, `transport="none"`,
   `lastError=undefined`, `driftWarnings=[]`, `driftToolNames=[]`, `serverInfo=undefined`,
   `raw=undefined`.
3. `return connect()`.

> Client identity constants (must stay byte-stable on the wire as the MCP client
> implementation info): **name `"daintree-assistant-cli"`, version `"0.1.0"`**,
> capabilities `{}`.

---

## 5. `listTools(force, signal?, timeoutMs?)` exact behavior

1. If `toolCache` set and `!force` → return cache.
2. Build `reqOpts` only when `signal` or `timeoutMs` is present (merge signal +
   timeout into one options object).
3. `res = await ensure().listTools(undefined, reqOpts)`.
   - On error: if **not** `McpUnavailableError` **and** `!signal?.aborted` →
     `markDegraded(e)`. Then re-throw. (An abort says nothing about connection health
     → don't degrade; the SDK wraps both abort and real failures as timeout-shaped
     `McpError`.)
4. Map `res.tools` → `McpToolInfo[]`: copy `name`, `description`, and
   `inputSchema ?? { type:"object", properties:{} }`. Store in `toolCache`, return it.

Caching contract: the cache is populated once (on warm) and reused until `force` or a
`markDegraded`/`reconnect` clears it. Callers pass `force=false` for steady-state.

---

## 6. `callTool(name, args, signal?, opts)` exact behavior

1. Build `reqOpts` from `signal` + `opts.timeoutMs` (same merge rule as listTools).
2. `retries = max(0, opts.retries ?? 0)`.
3. **Attempt loop** (`attempt = 0, 1, …`):
   - `res = await ensure().callTool({ name, arguments: args }, undefined, reqOpts)`; on
     success `break`.
   - On error `e`:
     - `aborted = signal?.aborted`.
     - **Retry path** (in this exact order, BEFORE degrading): if
       `!aborted && !(e instanceof McpUnavailableError) && attempt < retries && isRetriableMcpError(e)`
       → `abortableSleep(fullJitterDelay(attempt, MCP_READ_RETRY_POLICY.baseDelayMs,
       MCP_READ_RETRY_POLICY.maxDelayMs), signal)` then `continue`.
       > Why retry-before-degrade: marking the connection down on the first blip would
       > make the next attempt's `ensure()` throw, defeating the retry. Only after the
       > budget is spent do we degrade.
     - **Degrade path:** if `!(e instanceof McpUnavailableError) && !aborted` →
       `markDegraded(e)`. Then re-throw `e`.
4. Normalize the result (`McpCallResult`):
   - `content = res.content ?? []`.
   - `text` = for each block: if it's an object with a `"text"` key → `String(block.text)`,
     else `""`; then **filter out empties** (`filter(Boolean)`) and `join("\n")`.
   - `structuredContent = res.structuredContent` (verbatim).
   - `isError = Boolean(res.isError)`.

Abort semantics: an aborted call is **never** retried and **never** degrades the
connection; the error propagates and *callers* map it to a `CANCELLED` tool result by
checking `signal.aborted`. In Go this is `ctx.Err() == context.Canceled`.

---

## 7. `runDriftCheck()` exact behavior

Warning-only; never throws; never affects `connected`. Two independent try/catch
zones:
1. **Reset** `driftWarnings=[]`, `driftToolNames=[]`.
2. **Server info (isolated try/catch):** `info = raw?.getServerVersion?.()`; if present
   set `serverInfo = { name: info.name, version: info.version }`, else `undefined`.
   Isolated so a metadata fetch failure can't suppress the drift comparison.
3. `live = Set(toolCache?.map(name) ?? [])`. **If `live.size === 0` → return** (treat as
   "unknown", not "everything drifted").
4. For each `name` in `DOCUMENTED_MCP_TOOL_NAMES` (in array order): if not in `live`,
   push `name` to `driftToolNames` and push the sentence
   **`MCP drift: tool '<name>' is documented but missing from the live server`** to
   `driftWarnings`. The two arrays stay index-aligned.
5. Outer catch resets both arrays to `[]` (drift must never break startup).

**Drift is missing-only:** documented names absent from live. Extra live tools are
*expected and ignored* — the reference documents a verified subset, not the whole
surface.

---

## 8. Constants, limits, timeouts (exact numbers)

From `src/reliability.ts` (used by `callTool`'s read-only retry):
| Name | Value | Meaning |
|---|---|---|
| `MCP_READ_RETRY_POLICY.maxRetries` | `2` | additional attempts after the first (callers pass this as `retries`) |
| `MCP_READ_RETRY_POLICY.baseDelayMs` | `250` | base backoff |
| `MCP_READ_RETRY_POLICY.maxDelayMs` | `2_000` | backoff ceiling |
| `MCP_READ_TIMEOUT_MS` | `20_000` | per-attempt read timeout (callers pass as `timeoutMs`) |

`fullJitterDelay(attempt, baseMs, maxMs)`: uniform random integer in
`[0, min(maxMs, baseMs * 2^max(0,attempt))]`, computed as
`floor(random() * (ceiling + 1))`. `attempt` is 0-based. **Port exactly** — full
jitter (not equal/decorrelated) to avoid retry thundering herds.

`isRetriableMcpError(err)`: retriable iff
- `err` is an `McpError` with code `RequestTimeout` (**-32001**) or `ConnectionClosed`
  (**-32000**); **or**
- the error message matches (case-insensitive):
  `/fetch failed|ECONNRESET|ETIMEDOUT|ECONNREFUSED|socket hang up|network error|timed out/i`.

A JSON-RPC **application** error (a tool that genuinely failed) is NOT retriable —
only the transport is. In Go, map MCP error codes -32001/-32000 + a regex on
connection-error strings.

`abortableSleep(ms, signal)`: sleep that rejects with an `AbortError`-named error the
moment the signal fires (no listener leak). Go: `select` on `time.After(ms)` vs
`ctx.Done()`.

Doctor-probe timeouts (caller side, §9): **5_000 ms** (`5_000`) on both
`listTools()` and the `actions.getContext` call, via a `withTimeout(...)` helper.

Other magic strings/values:
| Constant | Value |
|---|---|
| MCP client name | `"daintree-assistant-cli"` |
| MCP client version | `"0.1.0"` |
| MCP capabilities | `{}` |
| SSE path rewrite regex | `/\/mcp\/?$/` → `"/sse"` |
| Default MCP URL (`DEFAULTS.defaultMcpUrl`, config) | `http://127.0.0.1:45454/mcp` |
| Bearer header | `Authorization: Bearer <token>` |
| Unavailable error code | `"MCP_UNAVAILABLE"` |
| `inputSchema` default | `{ type: "object", properties: {} }` |
| text-block join | `"\n"` |

---

## 9. External contracts (must remain wire/schema/env compatible)

### Environment variables (Daintree injects these)
| Var | Maps to | Precedence (highest first) |
|---|---|---|
| `DAINTREE_MCP_URL` | `cfg.mcpUrl` | override → env → project `.env` → assistant own `.env` → (no default at config; default URL `http://127.0.0.1:45454/mcp` is a constant, not auto-applied to `mcpUrl`) |
| `DAINTREE_MCP_TOKEN` | `cfg.mcpToken` | same chain |
| `DAINTREE_PROJECT_ID` | `cfg.projectId` | override → env (no `.env`) |
| `DAINTREE_WINDOW_ID` | `cfg.windowId` | override → env (no `.env`); **env-only, never a CLI flag** |
| `DAINTREE_ASSISTANT_OFFLINE` | `cfg.offline` (`=="1"`) | **trusted env only** (real `process.env`, never a bound project's `.env`) |

`firstString(...)` precedence helper: returns the first non-empty trimmed value.
`projectId`/`windowId` are read from `overrides` then `process.env` (NOT `.env`).
`mcpUrl`/`mcpToken` are read from `overrides` then merged `process.env` (the project
`.env` and assistant own-`.env` are loaded into `process.env` via dotenv, which never
overrides an already-set var). **Security:** `offline`/`tier`/`autoApprove`/`stateDir`/
`logDir` come from a `trustedEnv` snapshot taken *before* any `.env` load, so a bound
project's `.env` cannot escalate or redirect them. Port this trusted/merged split.

### MCP wire identity
The MCP `Client` advertises `{ name: "daintree-assistant-cli", version: "0.1.0" }` —
keep byte-stable.

### Documented tool surface — `DOCUMENTED_MCP_TOOL_NAMES` (drift baseline)
This **exact, ordered** list (from `src/models/prompts/daintreeMcp.ts`) is the drift
baseline. Hand-maintained (NOT parsed from prose, which names non-existent tools like
`terminal.listStatus`, `terminal.waitForAny` as negative examples). Port verbatim:

```
actions.getContext, actions.list, actions.search, actions.getSchema,
agent.focusNextAgent, agent.focusNextWaiting, agent.focusNextWorking,
agent.focusPreviousAgent, agent.launch,
copyTree.generate, copyTree.generateAndCopyFile, copyTree.injectToTerminal,
forge.addIssueComment, forge.addIssueLabel, forge.approvePR, forge.assignIssue,
forge.closeIssue, forge.closePR, forge.commentOnPR, forge.convertPRToDraft,
forge.createIssue, forge.createPR, forge.dismissReview, forge.editIssue, forge.editPR,
forge.getIssue, forge.getPR, forge.listIssues, forge.listPRs,
forge.markPRReadyForReview, forge.mergePR, forge.removeIssueLabel, forge.reopenIssue,
forge.reopenPR, forge.requestChanges, forge.requestReviewers, forge.unassignIssue,
git.getProjectPulse, git.snapshotDelete, git.snapshotRevert,
panel.focus, recipe.list, recipe.run,
terminal.arm, terminal.disarm, terminal.disarmAll, terminal.getOutput,
terminal.getStatus, terminal.list, terminal.sendCommand, terminal.waitUntilIdle,
workflow.focusNextAttention, workflow.prepBranchForReview, workflow.startWorkOnIssue,
worktree.createWithRecipe, worktree.getCurrent, worktree.list
```
(55 names. Keep in sync with the reference prose when it changes.)

### `actions.getContext` — doctor probe + project-name (caller contract)
- **Doctor probe** (`src/cli/commandData.ts`): only runs when `status().connected`.
  1. `listTools()` (5s timeout). If `actions.getContext` not advertised → check fails
     with "workbench tier may be unavailable".
  2. Else `callTool("actions.getContext", {})` (5s timeout); pass iff **`!res.isError`**;
     report round-trip ms. A *throw* = live transport failure/timeout (not a tier
     issue). `actions.getContext` is chosen because it's read-only, workbench tier, no
     confirmation — verifies end-to-end access without mutating.
- **Project name** (`src/ui/hooks/useDaintreeController.ts` → `readProjectName`/
  `fetchProjectName`): call `actions.getContext`, read `structuredContent` first; if
  absent, **`JSON.parse(res.text)`** and read again. Extract `projectName` (top-level)
  or nested `project.name`, trimmed non-empty. Daintree only emits `structuredContent`
  when an action declares an output schema, but **always** serializes the same object
  into `text` — so the text-JSON fallback is load-bearing. Any failure → `undefined`
  (caller keeps its provisional name). Port `readProjectName` as a pure helper.

### Terminal MCP error markers (SESSION_BINDING_GONE / BINDING_STALE)
Daintree returns these as **error strings** in a tool result (surfaced in
`McpCallResult.text` / inside the JSON-RPC error). They mean **the bound Daintree
window is gone** — the agent should **stop retrying that session and tell the user**.
NOT handled in `client.ts` itself (this file does not parse them); the prompt
(`daintreeMcp.ts`) instructs the model. They are **terminal, non-retriable**: do NOT
add them to `isRetriableMcpError`, and do NOT treat them as transient. Document for the
porter so retry logic never accidentally re-issues a call after one of these.

### Grant-support seam
`DAINTREE_GRANT_TOOL_NAMES` empty today → `hasDaintreeGrantSupport()` always `false`.
Daintree exposes its grant lifecycle only to its own renderer (IPC), not over MCP. The
allowlist exists so the seam lights up with no other change when Daintree ships an
external grants API. Keep the allowlist (not a heuristic) so it can never
false-positive.

---

## 10. Non-obvious behavior / ordering / "why" to preserve

- **Injected client warms on first `connect()`** — construction does not warm the
  cache, so `connect()` checks `transport==="injected" && !toolCache` to warm once.
- **`warmToolCache` is best-effort and restores prior state on failure**: it snapshots
  `{connected, lastError}` before `listTools(true)`, and on catch restores them — a
  transient tool-list failure must not flip a healthy transport to degraded (because
  `listTools`'s own catch calls `markDegraded`). The connection stays up; tool count is
  simply unknown.
- **Retry-before-degrade ordering** in `callTool` (§6) is load-bearing: degrading first
  would make the next `ensure()` throw `McpUnavailableError`, killing the retry.
- **Abort ≠ unhealthy.** Both `listTools` and `callTool` check `signal?.aborted` and
  refuse to `markDegraded` on an abort, because the SDK wraps a user-abort and a real
  transport failure with the same timeout-shaped `McpError`.
- **`McpUnavailableError` is never a degrade trigger** — it already means
  disconnected; re-degrading would clobber the real `lastError`.
- **`status()` defensive copies** + empty→undefined collapse for drift arrays.
- **SSE path rewrite only touches a trailing `/mcp`** (regex anchored `$`); a URL with
  a different path is reused unchanged for SSE.
- **Drift `live.size===0` → return** (unknown, not "all drifted").
- **`close()` swallows errors** and always sets `connected=false`.
- **Combined per-attempt AbortSignals are deliberately avoided** in `reliability.ts`
  (the comment cites Node #54614 `AbortSignal.any` listener leak). Each SDK call races
  its own `timeout`. In Go, use a per-attempt `context.WithTimeout` derived from the
  caller ctx — the natural idiom — and don't try to fuse signals.

---

## 11. Proposed Go mapping

**Package:** `internal/mcp` (client) + reuse of `internal/reliability` (jitter/retry
predicates) and the env contract in `internal/config`.

**Suggested Go surface:**
```
package mcp

type ToolInfo struct {
    Name        string
    Description string
    InputSchema map[string]any   // default {"type":"object","properties":{}}
}

type CallResult struct {
    Text              string
    Content           []any
    StructuredContent any
    IsError           bool
}

type CallOptions struct {
    Timeout time.Duration // 0 = unset
    Retries int           // clamped >= 0; read-only callers only
}

type Status struct {
    Connected      bool
    URL            string
    Transport      string // "streamable-http" | "sse" | "injected" | "none"
    ToolCount      *int
    Error          string
    DriftWarnings  []string // nil when none
    DriftToolNames []string // nil when none; index-aligned with DriftWarnings
    ServerInfo     *ServerInfo
}
type ServerInfo struct{ Name, Version string }

// Mockable seam (test fakes implement this).
type LowLevelClient interface {
    ListTools(ctx context.Context) ([]rawTool, error)
    CallTool(ctx context.Context, name string, args map[string]any) (rawResult, error)
    GetServerVersion() *ServerInfo // may return nil
    Close() error
}

type Client struct { /* cfg, low, connected, transportKind, lastError,
                        toolCache, driftWarnings, driftToolNames, serverInfo, mu */ }

func New(cfg config.App, opts Options) *Client
func (c *Client) IsConnected() bool
func (c *Client) HasGrantSupport() bool
func (c *Client) Status() Status
func (c *Client) Connect(ctx context.Context) Status      // never returns error
func (c *Client) Reconnect(ctx context.Context) Status
func (c *Client) ListTools(ctx context.Context, force bool) ([]ToolInfo, error)
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any, opts CallOptions) (CallResult, error)
func (c *Client) Close() error

// pure helpers
func ToolsAdvertiseGrantSupport(tools []ToolInfo, grantNames []string) bool
func ReadProjectName(v any) string

var GrantToolNames = []string{} // empty today
var ErrUnavailable = &UnavailableError{} // Code: "MCP_UNAVAILABLE"
```

**Concurrency:** the TS client is single-threaded (event loop); a Go port must guard
mutable state (`connected`, `toolCache`, drift, `lastError`) with a `sync.Mutex` since
callers (UI hook, daemon ticks, doctor) may invoke concurrently.

**Context vs AbortSignal:** replace every `signal?: AbortSignal` with
`ctx context.Context`; replace `timeoutMs` with a derived `context.WithTimeout`.
"aborted" → `errors.Is(ctx.Err(), context.Canceled)`.

**MCP transport / library options (Go):**
- The official Go MCP SDK is **`github.com/modelcontextprotocol/go-sdk`** (`mcp`
  package) — it provides a `Client`, a **Streamable HTTP** client transport, and an
  **SSE** client transport, plus `ListTools`/`CallTool`. Prefer it to match the
  `@modelcontextprotocol/sdk` semantics (error codes, server-version metadata).
  Verify it exposes per-request timeout + custom headers (`Authorization: Bearer`); if
  the header hook differs, wrap the underlying `*http.Client`/`http.RoundTripper` to
  inject the bearer header.
- If a suitable feature is missing, fall back to `net/http` + a hand-rolled JSON-RPC
  client; but first preference is the official SDK so error-code mapping
  (`-32001`/`-32000`) is faithful.
- JSON: `encoding/json`; `InputSchema`/`StructuredContent`/`Content` stay `any` /
  `json.RawMessage`-backed maps (pass through verbatim).

**Retry/jitter:** port `fullJitterDelay`, `isRetriableMcpError`, `abortableSleep`,
`MCP_READ_RETRY_POLICY`, `MCP_READ_TIMEOUT_MS` into `internal/reliability`. Jitter uses
`math/rand` (a non-crypto PRNG is fine and intended).

**Server metadata:** `raw` exists only to call `getServerVersion`. In Go, expose
`GetServerVersion()` on the `LowLevelClient` interface (nil-safe).

---

## 12. DELETE — do not port (Node/Bun/TS-specific)

- `import` of `@modelcontextprotocol/sdk/client/{index,streamableHttp,sse}.js` — replace
  with the Go MCP SDK / `net/http`.
- The `client as unknown as LowLevelMcpClient` casts — Go uses real interface impls.
- `AbortSignal` plumbing → `context.Context` (don't port `addEventListener`/listener
  cleanup; that's a JS event-loop concern, see Node #54614 note).
- `Promise`/`async`/`await` machinery; `Awaited<ReturnType<…>>` type gymnastics.
- The `errMsg` `instanceof Error` shim → Go `err.Error()`.
- `firstString` + dotenv loading belong to the **config** port, not this file; this
  module only consumes `cfg.McpUrl`/`cfg.McpToken`/`cfg.Offline`.
- Defensive `[...]`/`{...}` spreads → Go slice/struct copies.
- Anything referencing OpenTUI/React/Ink (none in this file — the project-name parse
  lives in a UI hook; only `ReadProjectName` (pure) is in scope, the hook is not).
```
