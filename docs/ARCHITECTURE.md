# Daintree Assistant CLI — architecture & build contract

This is the ground-truth contract. The coherent core (schemas, config, storage,
mcp, models, safety, queue, tool framework, runtime) is already written. Tool
modules and tests are built against the **exact** interfaces below.

## Layout

```
src/
  schemas.ts            domain types + Zod (DONE)
  config.ts             AppConfig, loadConfig (DONE)
  queue.ts              Queue (DONE)
  storage/db.ts         Db (node:sqlite) (DONE)
  mcp/client.ts         DaintreeMcpClient (DONE)
  models/
    fireworks.ts        FireworksClient, ChatMessage, ChatTool, ThinkFilter (DONE)
    router.ts           ModelRouter (DONE)
    prompts.ts          system prompts (DONE)
  safety/policy.ts      tier gating, confirmation, no-file-edit guard (DONE)
  tools/
    types.ts            ToolDef, ToolContext, ok(), fail(), NO_ARGS (DONE)
    registry.ts         ToolRegistry (DONE)
    fsTools.ts          export const fsTools: ToolDef[]            <-- build
    mcpTools.ts         export const mcpTools: ToolDef[]           <-- build
    timerTools.ts       export const timerTools: ToolDef[]         <-- build
    watcherTools.ts     export const watcherTools: ToolDef[]       <-- build
    queueTools.ts       export const queueTools: ToolDef[]         <-- build
    contextTools.ts     export const contextTools: ToolDef[]       <-- build
    agentTaskTools.ts   export const agentTaskTools: ToolDef[]     <-- build
    index.ts            buildAllTools(): ToolDef[] (DONE)
  agent/loop.ts         AgentSession (DONE)
  daemon/
    watcherEngine.ts    runTerminalWatcherCheck + pure helpers (DONE)
    scheduler.ts        Scheduler (DONE)
  cli/
    render.ts           render + colors (DONE)
    app.ts              App: wires deps, ctx, session, scheduler (DONE)
    commands.ts         slash commands (DONE)
    repl.ts             interactive REPL (DONE)
    index.ts            commander entry (DONE)
tests/                  vitest specs                                <-- build
```

## Scheduler architecture decision (issue #5)

The scheduler (`daemon/scheduler.ts`) is **foreground-only**: it runs in-process
via `setInterval(...).unref()` and is started only on interactive paths
(`App.startScheduler`). Timers, watchers, and automatic reactions are persisted
in SQLite and resume on the next launch, but **nothing ticks while the CLI is
closed**. This is honest, current behavior (call it *option A*) and the prompt /
tool surfaces now say so explicitly rather than implying background supervision.

Two longer-term options were considered for true background ticking:

- **Option B — detached sidecar.** A separate long-lived process owns the tick
  loop (`ref()`'d interval or a kept-open server) and survives TUI close. Viable,
  but only as an *interim* step and **gated on per-project DB isolation (issue
  #4)**: without a per-project `state.db` plus SQLite WAL mode + busy timeout, a
  sidecar and an open TUI are concurrent writers that can double-fire. Adds
  process supervision / IPC / daemonization complexity.
- **Option C — Daintree-owned watch-sets over MCP.** Daintree owns the lifecycle
  entirely (watch-sets + completion callbacks over an SSE/HTTP MCP transport) and
  the CLI becomes a pure conversation UI with no tick loop. No local concurrency
  problem, but requires Daintree-side primitives that do not exist yet and an
  SSE transport (stdio dies with the parent), and couples scheduling to Daintree
  availability (no offline operation).

**Decision:** keep option A now; the honesty fix is the only in-repo work for
issue #5. Target **option C** long-term once Daintree exposes watch-sets over an
SSE transport. Option B remains a viable intermediate, but only after issue #4
(per-project DB). No sidecar or transport work is undertaken here.

## The ToolDef contract (from src/tools/types.ts)

```ts
interface ToolDef<A = any> {
  name: string;                 // e.g. "fs.read" — MUST NOT imply file mutation
  description: string;          // shown to the model; be specific & action-oriented
  risk: RiskClass;              // "read"|"local"|"ui"|"terminal"|"project"|"git"|"external"|"system"
  parameters: Record<string, unknown>;  // JSON Schema, additionalProperties:false
  schema?: z.ZodType<A>;        // optional runtime validation of parsed args
  readOnly?: boolean;
  handler: (args: A, ctx: ToolContext) => Promise<ToolResult>;
}
```

`ToolContext` provides: `config`, `mcp` (DaintreeMcpClient), `db` (Db), `queue`
(Queue), `router` (ModelRouter), `projectPath`, `actor`, `confirm`, `log`.

Use the helpers `ok(summary, result?)` and `fail(code, message, {recoverable?,details?})`
for results. Use `NO_ARGS` for the parameters of no-arg tools. Confirmation and
audit are handled by the registry — handlers just do the work and return a result.

### Worked example (match this style exactly)

```ts
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";
import { resolveInsideProject } from "../safety/policy.js";
import fs from "node:fs/promises";

const ReadArgs = z.object({
  path: z.string().describe("Path relative to the project root."),
  maxBytes: z.number().int().positive().max(200_000).optional(),
});

export const fsTools: ToolDef[] = [
  {
    name: "fs.read",
    description: "Read a UTF-8 text file from the project (read-only).",
    risk: "read",
    readOnly: true,
    schema: ReadArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        path: { type: "string", description: "Path relative to project root." },
        maxBytes: { type: "number", description: "Max bytes to read." },
      },
      required: ["path"],
    },
    async handler(args, ctx) {
      try {
        const abs = resolveInsideProject(ctx.projectPath, args.path);
        const buf = await fs.readFile(abs, "utf8");
        const sliced = args.maxBytes ? buf.slice(0, args.maxBytes) : buf;
        return ok(`Read ${args.path} (${sliced.length} chars).`, { path: args.path, content: sliced });
      } catch (e) {
        return fail("FS_READ", `Could not read ${args.path}: ${e instanceof Error ? e.message : String(e)}`);
      }
    },
  },
];
```

## Tools to build (name · risk · behavior)

### fsTools.ts — read-only project access (NEVER writes)
- `fs.list` (read) — list a directory relative to project root. Args `{path?: string, depth?: number}`. Use `resolveInsideProject`. Return entries with name/type. Skip `.git`, `node_modules`.
- `fs.read` (read) — see worked example.
- `fs.search` (read) — text search across project files. Args `{query: string, glob?: string, maxResults?: number}`. Pure JS recursive walk (skip `.git`, `node_modules`, `dist`); return matches `{file, line, text}` capped (default 50). No shell.

### mcpTools.ts — Daintree MCP access
- `daintree.status` (read) — return `ctx.mcp.status()` plus a one-line summary. Works even when disconnected (report it).
- `daintree.listTools` (read) — `await ctx.mcp.listTools()`; return names + descriptions, each annotated with a `callable` flag (membership in `ctx.activeToolNames`, the turn's projection; absent ⇒ all callable). If disconnected, `fail("MCP_UNAVAILABLE", ...)`.
- `tool.search` (read) — search Daintree MCP tools by keyword (substring on name/description). Args `{query: string, max?: number}`. Each match carries `callable: boolean` (whether it's offered in this turn's tool spec, `ctx.activeToolNames`) so it never advertises a tool the model can't invoke now; annotate rather than filter so discovery still works.
- `daintree.call` (depends → mark risk `"project"`) — raw passthrough. Args `{name: string, arguments?: object, requestKey?: string}`. Call `ctx.mcp.callTool(name, {...arguments, requestKey})`. Return `{text, structuredContent, isError}`. This is the escape hatch; it is risk "project" so it always confirms. If `isError`, return `fail`.

### timerTools.ts — durable timers (CLI-local, risk "local")
- `timer.schedule` — Args `{title, fireAt?: ISO string, delayMs?: number, repeat?: {everyMs, maxRuns?, until?: ISO}, payload: {type: "enqueue"|"run_check"|"call_safe_tool", message?, checkPrompt?, toolCall?: {toolName, args}}, target?: {projectId?,worktreeId?,terminalId?,workflowRunId?}}`. Compute fireAt from delayMs if needed (Date.now()+delayMs). Insert via `ctx.db.insertTimer({title, fireAt, repeatEveryMs?, repeatUntil?, maxRuns?, payloadType, payloadJson, targetJson?})`. Return the timer id + fireAt.
- `timer.list` — list scheduled timers (`ctx.db.listTimers("scheduled")`), summarized.
- `timer.cancel` — Args `{id}`. `ctx.db.updateTimer(id, {status:"cancelled"})`.

### watcherTools.ts — terminal watchers (CLI-local, risk "local")
- `watcher.terminal.create` — Args `{terminalIds: string[], title, goal, cadenceMs?, startAfterMs?, stopAfterMs?, stopWhen?: WatchCondition, alertWhen?: WatchCondition, modelTier?: "small"|"medium"}`. Default cadenceMs 120000. Insert via `ctx.db.insertWatcher({kind:"terminal", title, goal, targetsJson: JSON.stringify(terminalIds), cadenceMs, modelTier: modelTier??"small", startAfterMs?, stopAfterMs?, stopWhenJson?, alertWhenJson?, nextCheckAt: Date.now()+(startAfterMs??0)})`. Validate conditions with `WatchCondition` from schemas. Return watcher id.
- `watcher.list` — list active watchers summarized.
- `watcher.cancel` — Args `{id}`. `ctx.db.updateWatcher(id, {status:"cancelled"})`.

### queueTools.ts — attention queue (CLI-local, risk "local" for publish/resolve, read for digest)
- `queue.publish` — Args = `QueuePublishArgs` schema. `ctx.queue.publish(args)`. (Used mostly by sub-threads, but expose it.)
- `queue.digest` (read) — Args `{severityAtLeast?, maxItems?, includeResolved?}`. Return `ctx.queue.digest(opts)` + formatted text via `ctx.queue.format(events)`.
- `queue.resolve` — Args `{id}`. `ctx.queue.resolve(id)`.

### contextTools.ts — snapshots & summaries
- `context.snapshot` (read) — Build a compact main-thread snapshot: mcp status; if connected, best-effort call `actions.getContext`, `worktree.list`, `terminal.list` via `ctx.mcp.callTool` (wrap each in try/catch); include open queue digest (`ctx.queue.digest({severityAtLeast:"attention", maxItems:10})`). Return a structured object + a readable summary. Must not throw if MCP is down.
- `terminal.summarize` (read) — Args `{terminalId, purpose?: string, tailBytes?: number}`. Read terminal output via `ctx.mcp.callTool("terminal.getOutput", {terminalId, lines:200})`, then summarize with the SMALL model using `SUMMARIZER_SYSTEM_PROMPT` + `buildSummarizerUserPrompt` (import from ../models/prompts.js) via `ctx.router.chat("small", {...})`. Return the summary text. If MCP down, fail cleanly.

### agentTaskTools.ts — the no-file-edit escape hatch (risk "project")
- `agentTask.spawnForEdits` — Args `{worktreeId?: string, agentId?: string (default "claude"), mode?: "edit"|"explore" (default "edit"), title, taskPrompt, context?: {filePaths?: string[], includeDiff?: boolean}, watcher?: {create: boolean, goal?: string, cadenceMs?: number}}`. Behavior: build an agent prompt (compose taskPrompt + a mode-specific constraints block — edit mode: "Make changes only in this worktree… Report back changed files/tests/risks"; explore mode: "READ-ONLY exploration: do not modify files… report findings"), then `ctx.mcp.callTool("agent.launch", {agentId, worktreeId?, prompt})` (requestKey via randomUUID). From the result, get the terminalId (structuredContent.terminalId ?? parse). If `watcher.create`, insert a terminal watcher (same shape as watcher.terminal.create) for that terminalId. Return `{terminalId, worktreeId, watcherId?}`. If MCP down, fail with guidance. This is the ONLY agent-spawn path (edits AND exploration); the CLI never edits files and never hand-rolls a raw agent.launch.
- `daintree.call` — raw escape hatch. Before forwarding, it checks `WRAPPED_MCP_TOOLS`: if `name` has a typed wrapper (agent.launch → agentTask.spawnForEdits; terminal.getOutput → terminal.summarize/extract; panel.focus → terminal.focus), it fails fast with code `USE_TYPED_WRAPPER` naming the wrapper instead of forwarding. This stops the recurring "use the escape hatch with empty args, then retry the identical broken call" loop.

## Confirmation / risk
The registry calls `ctx.confirm` automatically for risk in {terminal, project, git, external, system}. Handlers do NOT call confirm themselves.

## Tests (tests/*.test.ts) — vitest
Build these. Import from `../src/...`. Use `:memory:` DBs and fake MCP/model objects — NO network.
1. `noFileEditGuard.test.ts` — `assertNoFileEditTools` throws on "fs.write"/"apply_patch"; passes for the real registry (`buildAllTools()` names). `resolveInsideProject` blocks traversal.
2. `policy.test.ts` — confirmation matrix: supervisor denies "terminal"; operator allows+confirms "project"; system allows "git"; read/local never confirm.
3. `config.test.ts` — loadConfig picks up overrides + defaults; describeConfig redacts secrets.
4. `db.test.ts` — timers due query; watcher due query; event dedupe bumps count; resolve hides from digest; severity ordering in listEvents.
5. `queue.test.ts` — publish + dedupe + digest formatting; ttl expiry.
6. `thinkFilter.test.ts` — ThinkFilter strips `<think>…</think>` across chunk boundaries; visible/reasoning correct; streaming split mid-tag.
7. `registry.test.ts` — dispatch unknown tool; invalid args; tier-denied; a read tool runs + writes an audit row; a mutating tool with a confirm() that returns false yields USER_DECLINED.
8. `watcherEngine.test.ts` — `evaluateCondition` for stateIs/contains/regex/noOutputForMs/all/any/not; `decideOutcome` publishes on meaningful change, suppresses repeated still_working, stops on completed_success/stopWhen/timeout.
9. `scheduler.test.ts` — a one-shot enqueue timer fires and publishes once, then status fired; a repeating timer reschedules with run count; due watcher (terminal) runs through a fake MCP+model and updates lastClassification. (Use fake `ctxFor`.)
10. `fsTools.test.ts` — fs.read/list/search over a temp dir confined to project; traversal blocked.

Keep each test file focused and fast.
