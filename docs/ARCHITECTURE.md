# Daintree Assistant — architecture reference

Architecture reference for the Go binary. Every package described below is
implemented; the contracts (the `ToolDef` shape, the scheduler decision record, the
per-package behavior notes) describe the system as it stands. The binary is **headless**:
Daintree embeds it over `host --stdio` and renders the conversation natively, so there is
no terminal UI package any more — see [`DAINTREE_HOST.md`](DAINTREE_HOST.md).

## Layout

```
cmd/daintree-assistant/   main.go — flags → one-shot | doctor | host | mcp | daemon | repl
internal/
  domain/        pure vocabulary (uuid + stdlib only): RiskClass, Tier, ModelTier, RunPhase,
                 ToolResult (Ok/Fail), AgentEvent union, DB-row records, WatchCondition DSL,
                 constants (MainPromptCacheKey = "daintree-main", MaxToolIterations = 12), IDs
  config/        LoadConfig(ConfigOverrides) → AppConfig; trusted-env boundary; DEFAULTS
  ports/         interface seams (EventSink, Store, Router, ToolRegistry, MCPClient, Queue)
  projectinstructions/  Load(projectPath) → DAINTREE.md (16 KiB cap)
  debuglog/      StartDebugLog / LogDebug / CurrentDebugLogPath
  storage/       Store (store.go) over modernc.org/sqlite — durable state
  backend/       Daintree backend client — the CLI's ONLY model gateway; the backend
                 reaches every model through OpenRouter. client.go (Respond/RunTask/
                 Health), contracts.go (wire envelope), sse.go (named-event parser),
                 tasks.go (server-owned utility tasks). See BACKEND.md
  models/        conversation wire VOCABULARY only (ChatMessage/ChatTool/ToolCallRequest/
                 ChatResult/Usage) — NOT a model client; the direct provider transport,
                 Router, SSE parser, retry layer and pricing were deleted with the backend
                 migration. Do NOT add a provider client back here
  prompts/       MainPromptContext (structured runtime facts; the prompt-text +
                 loaded-skills builders were deleted, the backend owns them)
  mcp/           Daintree MCP client (go-sdk: Streamable HTTP, SSE fallback) + typed wrappers
  safety/        policy.go — Decide(risk, tier), AlwaysConfirm, no-file-edit guard
  tools/
    registry.go  Registry — register, project per-turn tool set, AssertSafe
    dispatch.go  Dispatch — validate → tier gate → confirm/grant → run → audit
    fsx/         fs.list / fs.read / fs.search (read-only project access)
    mcpx/        daintree.status / listTools / call · tool.search · terminal.focus /
                 sendCommand / arm / disarm / disarmAll · copyTree.* · agent.focus*
    mcpwrap/     typed MCP wrappers (USE_TYPED_WRAPPER guard): forge reads ·
                 worktree.list / getCurrent / createWithRecipe · git.getProjectPulse ·
                 recipe.* · workflow.startWorkOnIssue / prepBranchForReview
    contextx/    context.snapshot / terminal.summarize / terminal.read
    extractionx/ terminal.extract / terminal.extract.json / terminal.awaitAll
    timer/       timer.schedule / timer.list / timer.cancel
    watcher/     watcher.terminal.create / watcher.watchPR / watcher.list / watcher.cancel
    queue/       queue.publish / queue.digest / queue.resolve
    grant/       grant.create / grant.list / grant.revoke
    workflow/    workflow.create / get / list / update
    skill/       skill.run.get / skill.step.advance (stepwise run-tracking; selection is server-owned)
    auditx/      audit.export
    memory/      memory.recall / list / save / forget / pin / unpin
    artifactx/   artifact.read
    agenttaskx/  agentTask.spawnForEdits / superviseTerminal / status / list
                 (spawnForEdits is the no-file-edit escape hatch)
  agent/         Session (session.go) main turn loop + EventSink (events.go)
  daemon/        scheduler.go (3s tick) + watcher.go (terminal watcher state machine)
  queue/         Queue — attention queue
  app/           App.Create — wires deps, ctx, session, scheduler; ToolContext factory
  commands/      slash-command catalog + handlers (structured results for the host + REPL)
  cli/           Run(Options) entry, repl.go (line REPL), host.go, mcpserve.go, render/, jsonout/
  host/          embedded host (host.go) — stdio NDJSON, PROTOCOL_VERSION 3
  terminal/      TTY-gated raw escapes (clear.go) — the only host-scrollback wipe
```

## Wiring & data flow

`app.App.Create(CreateOptions)` builds every dependency **once**, in order: config
(`config.LoadConfig`), `storage.Store`, the MCP client, `queue.Queue`, the `backend`
client (the model gateway), `tools.Registry` (populated then gated by `AssertSafe`),
and `agent.Session`. It exposes a `ToolContext` factory so each tool dispatch gets the
config, MCP client, store, queue, backend, project path, actor, confirm hook, and logger.

A turn runs through `agent.Session.Send()`:

1. Phase `Received`; optional **auto-compact** of the conversation (threshold + behavior
   in [`RUNTIME.md`](RUNTIME.md)); push the user message.
2. Project the FULL tool registry into the request inventory (skills never narrow it).
3. Loop:
   - Phase `Analyzing` / `Integrating`; `Backend.RespondStream(req, …)` with a token
     callback — sends structured stable startup data separately from the visible
     conversation, plus fresh runtime/turn context and the tool inventory
     + the opaque state token. The first SSE `meta` event carries the refreshed state +
     the server's `skills` block (selection + injection are server-owned).
   - Append the assistant message. **No tool calls** → phase `Complete`, return the answer.
   - Otherwise announce the whole tool batch (`ToolBatch`, all `queued`), then
     `registry.Dispatch()` each in the safe sequence, promoting and resolving each, feed
     the results back as `tool` messages, and re-`RespondStream` (replaying the state token).

`Dispatch` = parse/validate args → tier gate (`safety.Decide`) → confirmation (interactive
`main` actor) or scoped automation grant (watcher/timer/workflow actors) → run the handler
→ write an audit row. Handlers return a `domain.ToolResult` via `Ok` / `Fail`; `Dispatch`
recovers panics into a `Fail` so a tool can never crash the loop.

The `Scheduler` ticks every 3s in whichever process owns the project (an open assistant
or the persistent supervisor daemon — see below), firing due timers and watcher checks.
Everything off the main thread publishes to the **attention queue** (a digest the main
thread reads), never interrupting the conversation with raw logs.

## Scheduler architecture decision

The scheduler (`daemon/scheduler.go`) ticks in-process every 3s in the project's
**owner process** — the open assistant while one is attached, and the **persistent
supervisor daemon** (`internal/supervisor`, `daintree-assistant daemon`) the rest of the
time. This is the built-out form of what this section used to call *option B* (a detached
sidecar), with the concurrent-writer hazard it was gated on solved at the architecture
level: an flock **owner lease** (`internal/ipc`) guarantees exactly one process opens a
project's `state.db` at a time, and ownership hands over through attach/detach (or a
crash — the kernel releases the flock). Watchers, timers, async futures, and the inbox
are project-scoped and ADOPTED by each new owner rather than torn down; the daemon runs
autonomous wake turns for completions that land while the user is away. Full design:
[`SUPERVISOR.md`](SUPERVISOR.md).

**Option C — Daintree-owned watch-sets over MCP** (Daintree owns the lifecycle entirely;
the assistant becomes a pure conversation UI with no tick loop) remains the long-term
target once Daintree grows those primitives; the supervisor's engine seams (`daemon.MCP`,
`WatcherModel`, the queue) are the surfaces such a migration would swap.

## The tool contract

A tool is a `tools.Tool` (its exact Go shape lives in `internal/tools/types.go`):

- `Name` — internal dotted name, e.g. `fs.read` (the registry maps it to/from the
  OpenAI wire form `fs__read`). **MUST NOT imply file mutation** (the `AssertSafe`
  guard rejects forbidden fragments such as `write_file`, `edit_file`, `fs.write`,
  `apply_patch`, `file.edit` at startup).
- `Description` — shown to the model; specific and action-oriented.
- `Risk` — a `domain.RiskClass` (`read` | `local` | `ui` | `terminal` | `project` |
  `external` | `git` | `system`). Drives tier gating + the confirmation matrix.
- `Consequence` — optional short human Y/N prose that leads the approval sheet.
- `Schema` — a JSON Schema (`additionalProperties: false`) advertised to the model as
  the OpenAI `parameters`; `tools.NoArgs` is the standard empty schema.
- `Decode` — optional `DecodeFunc` (typically `tools.StrictDecoder`) that validates/
  coerces args before the handler runs; nil ⇒ pass raw args through unvalidated.
- `Handle(ctx, args, *ToolContext) → ToolResult` — does the work and returns a result.

Handlers use `tools.Ok(summary, result)` and `tools.Fail(code, message, opts…)` and
**must never panic to the caller** — `Dispatch` recovers any panic into `TOOL_THREW`.
The `ToolContext` provides `Config`, `MCP`, `DB`, `Queue`, `Router`, `ProjectPath`,
`Actor`, the confirm hook, and the logger. **Confirmation and audit are handled by
`Dispatch`** — handlers never call confirm themselves and never throw to the caller.

> The full contributor guide — the struct field-by-field, the load-bearing dispatch
> order, the `AssertSafe` fragment list, and the complete family inventory — is in
> [`TOOLS.md`](TOOLS.md). The registry (`internal/tools` `Register` calls) is the
> source of truth; the lists below are a snapshot.

## Tool families (name · risk · behavior)

- **fsx** — `fs.list` / `fs.read` / `fs.search` (read). Read-only project access, confined
  to the project root (path traversal blocked), skipping `.git` / `node_modules` / build
  dirs. **Never writes.**
- **mcpx** — `daintree.status` (read), `daintree.listTools` / `tool.search` (read,
  annotate each match with a `callable` flag for the turn's projection), `daintree.call`
  (system, raw passthrough escape hatch — always confirms). Plus the UI/terminal
  wrappers: `terminal.focus` (ui), `terminal.sendCommand` (terminal),
  `terminal.arm` / `disarm` / `disarmAll` (terminal), `copyTree.generate` (read) /
  `generateAndCopyFile` (system) / `injectToTerminal` (terminal), and the UI-focus
  verb `terminal.focus` (ui). The five zero-arg focus CYCLERS
  (`agent.focusNext*`/`focusPreviousAgent`, `workflow.focusNextAttention`) were removed:
  they cost ~2 KB of the per-round tool inventory to expose UI navigation the model has
  no reason to drive, and nothing in the prompts or skills referenced them.
- **mcpwrap** — typed wrappers over Daintree MCP actions; a raw `daintree.call` fails
  fast with `USE_TYPED_WRAPPER` when a typed equivalent exists (e.g. `agent.launch` →
  `agentTask.spawnForEdits`). Members: `forge.listIssues` / `getIssue` / `listPRs` /
  `getPR` (read), `worktree.list` / `getCurrent` (read) / `createWithRecipe` (project),
  `recipe.list` (read) / `recipe.run` (project), `git.getProjectPulse` (read),
  `workflow.startWorkOnIssue` / `prepBranchForReview` (external) /
  `focusNextAttention` (ui).
- **contextx** — `context.snapshot` (read; MCP status + best-effort context/worktree/
  terminal lists + open queue digest, never throws if MCP is down), `terminal.summarize`
  (read; small-model summary of terminal output), `terminal.read` (read; raw scrollback
  tail verbatim — no model, no token cap).
- **extractionx** — `terminal.extract` / `terminal.extract.async` (structured field
  extraction via the small model; async lands the result on the attention queue).
- **timer** — `timer.schedule` / `timer.list` / `timer.cancel` (local; durable timers in
  SQLite with one-shot or repeating fire).
- **watcher** — `watcher.terminal.create` / `watcher.watchPR` / `watcher.list` /
  `watcher.cancel` (local; terminal watchers with the `WatchCondition` stop/alert DSL,
  default cadence 120s; `watchPR` polls a PR's state/draft/activity transitions).
- **queue** — `queue.publish` (local) / `queue.digest` (read) / `queue.resolve` (local).
- **grant** — `grant.create` / `grant.list` / `grant.revoke` (local; scoped automation
  grants that let non-interactive actors run mutating tools without an interactive confirm).
- **workflow** — `workflow.create` / `get` / `list` / `update` (durable multi-step records
  advanced over turns).
- **skill** — `skill.run.get` / `skill.step.advance` (stepwise run-tracking; selection is server-owned).
- **auditx** — `audit.export` (read; the audit log of every dispatched tool call).
- **memory** — `memory.recall` / `list` / `save` / `forget` / `pin` / `unpin` (durable
  cross-session memory).
- **artifactx** — `artifact.read` (read; a stored artifact by id).
- **agenttaskx** — `agentTask.spawnForEdits` (project; the **only** agent-spawn path,
  modes `edit` | `explore`; optionally attaches a supervising watcher),
  `agentTask.superviseTerminal` (project; attach a watcher to an already-running
  terminal), `agentTask.status` / `agentTask.list` (read; the spawn-saga records). The
  assistant never edits files and never hand-rolls a raw `agent.launch`.

## Confirmation / risk

`safety.Decide(risk, tier)` gates every dispatch. Tiers widen the allowed set:
`supervisor` (read/local/ui), `operator` (+terminal/project/external), `system`
(+git/system). `AlwaysConfirm` risk classes (terminal/project/external/git/system) require
confirmation for the interactive `main` actor; non-interactive actors need a matching
automation grant. read/local/ui never confirm.

## Storage

`storage.Store` is built on `modernc.org/sqlite` (pure Go, no CGO). State lives at
`~/.daintree/assistant-cli/state.db` (a per-project subdir when a project id is set) and
holds timers, watchers, events, audit, conversation, grants, and memory. The schema is a
**single clean baseline** (`schemaUserVersion`, currently 7); pre-release, a schema change is a
hard reset, not a migration chain. On open, the store cancels any stale (non-terminal)
watchers so a new session never inherits a prior one's supervision.

## Test coverage

Go `testing` across all packages — no network (`:memory:` SQLite, fakes for MCP/models).
Highlights: the no-file-edit guard rejects forbidden tool names; `safety.Decide` honours
the confirmation matrix; `config.LoadConfig` resolves overrides/defaults and redacts
secrets; the store's due-timer / due-watcher / dedupe queries; the think-filter strips
`<think>…</think>` across chunk boundaries; `Dispatch` audits a read tool and yields
`USER_DECLINED` on a declined confirm; the watcher engine's condition evaluation and
outcome decisions; the scheduler firing a one-shot vs rescheduling a repeat; and the
and the embedded host's transport framing, bridge, interrupt and wake/shutdown paths
(`internal/host/*_test.go`).
