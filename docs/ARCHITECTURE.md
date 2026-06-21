# Daintree Assistant — architecture reference

Architecture reference for the Go binary. Every package described below is
implemented; the contracts (the `ToolDef` shape, the scheduler decision record, the
per-package behavior notes) describe the system as it stands. The cockpit's own
rendering architecture has a dedicated doc — see [`BUBBLE_TEA.md`](BUBBLE_TEA.md).

## Layout

```
cmd/daintree-assistant/   main.go — flags → one-shot | doctor | cockpit | classic
internal/
  domain/        pure vocabulary (uuid + stdlib only): RiskClass, Tier, ModelTier, RunPhase,
                 ToolResult (Ok/Fail), AgentEvent union, DB-row records, WatchCondition DSL,
                 constants (MainPromptCacheKey = "daintree-main", MaxToolIterations = 12), IDs
  config/        LoadConfig(ConfigOverrides) → AppConfig; trusted-env boundary; DEFAULTS
  ports/         interface seams (EventSink, Store, Router, ToolRegistry, MCPClient, Queue)
  projectinstructions/  Load(projectPath) → DAINTREE.md (16 KiB cap)
  debuglog/      StartDebugLog / LogDebug / CurrentDebugLogPath
  storage/       Store (store.go) over modernc.org/sqlite — durable state
  models/
    fireworks.go FireworksClient — net/http Chat Completions, SSE streaming, think-filter
    router.go    Router — ModelFor(tier), Chat, Stream(onToken), JSON
    pricing.go   per-model cost estimation
    prompts/     base.go (BaseSystemPrompt), runtime context + loaded-skills builders
  mcp/           Daintree MCP client (go-sdk: Streamable HTTP, SSE fallback) + typed wrappers
  safety/        policy.go — Decide(risk, tier), AlwaysConfirm, no-file-edit guard
  tools/
    registry.go  Registry — register, project per-turn tool set, AssertSafe
    dispatch.go  Dispatch — validate → tier gate → confirm/grant → run → audit
    fsx/         fs.list / fs.read / fs.search (read-only project access)
    mcpx/        daintree.status / daintree.listTools / tool.search / daintree.call
    mcpwrap/     typed MCP wrappers (USE_TYPED_WRAPPER guard)
    contextx/    context.snapshot / terminal.summarize
    extractionx/ terminal.extract / terminal.extract.async
    timer/       timer.schedule / timer.list / timer.cancel
    watcher/     watcher.terminal.create / watcher.list / watcher.cancel
    queue/       queue.publish / queue.digest / queue.resolve
    grant/       grant.create / grant.list / grant.revoke
    workflow/    workflow.create / get / list / update
    skill/       skill.find / skill.load / skill.step.advance / skill.run.get
    auditx/      audit.export
    memory/      memory.recall / list / save / forget / pin / unpin
    artifactx/   artifact.read
    agenttaskx/  agentTask.spawnForEdits (the no-file-edit escape hatch)
  agent/         Session (session.go) main turn loop + EventSink (events.go)
  daemon/        scheduler.go (3s tick) + watcher.go (terminal watcher state machine)
  queue/         Queue — attention queue
  skills/        embedded runbooks (go:embed files/*.md) + SkillRegistry + SelectSkills
  app/           App.Create — wires deps, ctx, session, scheduler; ToolContext factory
  commands/      slash-command catalog + handlers (cockpit & classic)
  cli/           Run(Options) entry, repl.go (classic), CockpitRunner seam, render/, jsonout/
  ui/            Bubble Tea cockpit (the ONLY bubbletea importers) — see BUBBLE_TEA.md
  host/          embedded host (run.go) — stdio NDJSON, PROTOCOL_VERSION 2
  terminal/      TTY-gated raw escapes (clear.go) — the only host-scrollback wipe
```

## Wiring & data flow

`app.App.Create(CreateOptions)` builds every dependency **once**, in order: config
(`config.LoadConfig`), `storage.Store`, the MCP client, `queue.Queue`, `models.Router`,
`tools.Registry` (populated then gated by `AssertSafe`), the embedded skill registry,
and `agent.Session`. It exposes a `ToolContext` factory so each tool dispatch gets the
config, MCP client, store, queue, router, project path, actor, confirm hook, and logger.

A turn runs through `agent.Session.Send()`:

1. Phase `Received`; optional **auto-compact** of the conversation; push the user message.
2. Build the per-turn allowed-tool set (read-only set, widened by any loaded skills'
   `requiredTools`).
3. Loop, up to `domain.MaxToolIterations` (12):
   - Phase `Analyzing` / `Integrating`; `router.Stream("large", …)` with a token callback.
   - Append the assistant message. **No tool calls** → phase `Complete`, return the answer.
   - Otherwise announce the whole tool batch (`ToolBatch`, all `queued`), then
     `registry.Dispatch()` each in the safe sequence, promoting and resolving each, and
     feed the results back as `tool` messages.
4. Re-select skills (`FindSkills`, small model, ≤3) for the next turn.

`Dispatch` = parse/validate args → tier gate (`safety.Decide`) → confirmation (interactive
`main` actor) or scoped automation grant (watcher/timer/workflow actors) → run the handler
→ write an audit row. Handlers return a `domain.ToolResult` via `Ok` / `Fail`; `Dispatch`
recovers panics into a `Fail` so a tool can never crash the loop.

The daemon `Scheduler` ticks every 3s (foreground only), firing due timers and watcher
checks. Everything off the main thread publishes to the **attention queue** (a digest the
main thread reads), never interrupting the conversation with raw logs.

## Scheduler architecture decision

The scheduler (`daemon/scheduler.go`) is **foreground-only**: it ticks in-process every 3s
and runs only on interactive paths. Timers, watchers, and automatic reactions persist in
SQLite and resume on the next launch, but **nothing ticks while the assistant is closed**.
This is honest, current behavior (*option A*) and the prompt / tool surfaces say so rather
than implying background supervision.

Two longer-term options were considered:

- **Option B — detached sidecar.** A separate long-lived process owns the tick loop and
  survives cockpit close. Viable, but only as an interim step and **gated on per-project DB
  isolation**: without a per-project `state.db` plus WAL + busy timeout, a sidecar and an
  open cockpit are concurrent writers that can double-fire.
- **Option C — Daintree-owned watch-sets over MCP.** Daintree owns the lifecycle entirely
  (watch-sets + completion callbacks over an SSE/HTTP MCP transport) and the assistant
  becomes a pure conversation UI with no tick loop. No local concurrency problem, but
  requires Daintree-side primitives that don't exist yet.

**Decision:** keep option A now; target option C long-term. Option B remains a viable
intermediate, only after per-project DB isolation.

## The ToolDef contract

A tool is a `ToolDef` (its exact Go shape lives in `internal/tools`):

- `Name` — e.g. `fs.read`. **MUST NOT imply file mutation** (the `AssertSafe` guard
  rejects forbidden fragments such as `write_file`, `edit_file`, `fs.write`,
  `apply_patch`, `file.edit` at startup).
- `Description` — shown to the model; specific and action-oriented.
- `Risk` — a `domain.RiskClass` (`read` | `local` | `ui` | `terminal` | `project` |
  `external` | `git` | `system`).
- `Parameters` — a JSON Schema (`additionalProperties: false`) advertised to the model;
  args are validated before the handler runs.
- `Handler(args, ctx) → domain.ToolResult` — does the work and returns a result.

Handlers use `domain.Ok(summary, result)` and `domain.Fail(code, message, opts…)`. The
`ToolContext` provides `Config`, `MCP`, `Store`, `Queue`, `Router`, `ProjectPath`,
`Actor`, the confirm hook, and the logger. **Confirmation and audit are handled by
`Dispatch`** — handlers never call confirm themselves and never throw to the caller.

## Tool families (name · risk · behavior)

- **fsx** — `fs.list` / `fs.read` / `fs.search` (read). Read-only project access, confined
  to the project root (path traversal blocked), skipping `.git` / `node_modules` / build
  dirs. **Never writes.**
- **mcpx** — `daintree.status` (read), `daintree.listTools` / `tool.search` (read,
  annotate each match with a `callable` flag for the turn's projection), `daintree.call`
  (project, raw passthrough escape hatch — always confirms).
- **mcpwrap** — typed wrappers; `daintree.call` fails fast with `USE_TYPED_WRAPPER` when a
  raw call has a typed equivalent (e.g. `agent.launch` → `agentTask.spawnForEdits`).
- **contextx** — `context.snapshot` (read; MCP status + best-effort context/worktree/
  terminal lists + open queue digest, never throws if MCP is down), `terminal.summarize`
  (read; small-model summary of terminal output).
- **extractionx** — `terminal.extract` / `terminal.extract.async` (structured field
  extraction via the small model; async lands the result on the attention queue).
- **timer** — `timer.schedule` / `timer.list` / `timer.cancel` (local; durable timers in
  SQLite with one-shot or repeating fire).
- **watcher** — `watcher.terminal.create` / `watcher.list` / `watcher.cancel` (local;
  terminal watchers with `WatchCondition` stop/alert DSL, default cadence 120s).
- **queue** — `queue.publish` (local) / `queue.digest` (read) / `queue.resolve` (local).
- **grant** — `grant.create` / `grant.list` / `grant.revoke` (local; scoped automation
  grants that let non-interactive actors run mutating tools without an interactive confirm).
- **workflow** — `workflow.create` / `get` / `list` / `update` (durable multi-step records
  advanced over turns).
- **skill** — `skill.find` / `skill.load` / `skill.step.advance` / `skill.run.get`.
- **auditx** — `audit.export` (read; the audit log of every dispatched tool call).
- **memory** — `memory.recall` / `list` / `save` / `forget` / `pin` / `unpin` (durable
  cross-session memory).
- **artifactx** — `artifact.read` (read; a stored artifact by id).
- **agenttaskx** — `agentTask.spawnForEdits` (project; the **only** agent-spawn path,
  modes `edit` | `explore`; optionally attaches a supervising watcher). The assistant
  never edits files and never hand-rolls a raw `agent.launch`.

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
**single clean baseline** (`schemaUserVersion = 1`); pre-release, a schema change is a
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
cockpit's no-alt-screen / no-mouse contract (`internal/ui/view_test.go`).
