# Daintree MCP — integration notes

The CLI connects to Daintree's **local MCP server** as an external client. In production
Daintree launches the CLI and passes the connection details via environment / flags.

## Connection

| Setting        | Value                                                        |
| -------------- | ----------------------------------------------------------- |
| Transport      | Streamable HTTP (primary) at `/mcp`; legacy SSE at `/sse`    |
| Host/port      | `127.0.0.1:45454` (default; configurable)                   |
| URL env var    | `DAINTREE_MCP_URL` (e.g. `http://127.0.0.1:45454/mcp`)      |
| Token env var  | `DAINTREE_MCP_TOKEN` (bearer)                               |
| Project id     | `DAINTREE_PROJECT_ID` (optional)                            |
| Auth header    | `Authorization: Bearer <token>`                            |

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

Worktrees: `worktree.list`, `worktree.getCurrent`, `worktree.createWithRecipe`,
`worktree.delete`, `worktree.setActive`, `worktree.refresh`, `worktree.listBranches`.

Git: `git.getProjectPulse`, `git.getStagingStatus`, `git.commit`, `git.push`,
`git.stageFile`, `git.unstageFile`, `git.snapshotList`, `git.snapshotRevert`.

Forge: `forge.listIssues`, `forge.listPRs`, `forge.getIssue`, `forge.openIssue`,
`forge.openPR`, `forge.assignIssue`.

Agents/Recipes: `agent.launch`, `agent.terminal`, `agent.getState`, `recipe.list`,
`recipe.run`.

Code/Files (Daintree-side): `copyTree.generate`, `copyTree.injectToTerminal`,
`files.search`, `file.view`, `file.openInEditor`.

Meta: `actions.list`, `actions.getContext`, `actions.search`, `actions.getSchema`.

## State models (from Daintree)

```ts
type AgentState = "idle" | "working" | "waiting" | "directing" | "completed" | "exited";
type TerminalActivityStatus = "working" | "waiting" | "success" | "failure";
```

`actions.getContext` returns the active project / worktree / focused terminal snapshot.

## Idempotency

Mutating tools accept a `requestKey` for dedup (`terminal.new`, `worktree.createWithRecipe`,
`agent.launch`, `recipe.run`, `git.commit`, etc.). The CLI sends an idempotency key for every
autonomous mutation.

## Confirmation

`danger: "confirm"` tools may trigger an MCP elicitation. The CLI confirms with the user
locally BEFORE calling such tools (see safety/policy).
