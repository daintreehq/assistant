# Port-spec: CLI subsystem (`src/cli/` + `src/commandRegistry.ts`)

Faithful Go (Bubble Tea) port reference. Source: `daintree-assistant` TS,
`src/cli/{index,app,jsonSink,consoleSink,repl,render,terminalClear,commands,commandData}.ts`
and `src/commandRegistry.ts`. Cross-refs into `src/config.ts`, `src/schemas.ts`,
`src/mcp/client.ts`, `src/models/router.ts` where the CLI's external contracts live.

WIP branch — **no backward compat, no old-DB-format preservation required**. Existing
shapes below are *behavioral reference*; design notes favor a clean Go-native schema.

---

## 1. Entry / routing (`src/cli/index.ts`)

### 1.1 CLI surface (commander)

Program: `name "daintree-assistant"`, `version "0.1.0"`,
description "Daintree's local orchestration assistant (Fireworks-powered)."

| Flag | Arg | Maps to override | Notes |
|---|---|---|---|
| `--mcp-url <url>` | string | `mcpUrl` | else env `DAINTREE_MCP_URL` |
| `--mcp-token <token>` | string | `mcpToken` | else env `DAINTREE_MCP_TOKEN` |
| `--project <path>` | string | `projectPath` (→`path.resolve(cwd)`) | defaults to cwd |
| `--tier <tier>` | string | `tier` (cast `Tier`) | `supervisor`\|`operator`\|`system` |
| `--offline` | bool | `offline` | no network calls |
| `--classic` | bool | (routing only) | force legacy readline REPL |
| `--inline` | bool | **DEPRECATED NO-OP** | cockpit always inline now; accept + ignore |
| `--json` | bool | (routing only) | one-shot JSONL to stdout; diagnostics→stderr |
| `[prompt]` (positional, optional) | string | — | one-shot, then exit |
| subcommand `doctor` | — | — | environment check |

`CliOptions` interface fields: `mcpUrl?, mcpToken?, project?, tier?, offline?, classic?, inline?, json?` (all optional).

`overridesFromOptions(opts)` → `ConfigOverrides { mcpUrl, mcpToken, projectPath: opts.project, tier: opts.tier as Tier, offline }`.
(Note: `classic`, `inline`, `json` are routing-only, NOT carried into config.)

### 1.2 Action dispatch (the `.action` for the default command)

```
if prompt        → runOneShot(prompt, opts)
else if opts.json → STDERR "--json requires a prompt argument (one-shot mode only).\n"; process.exitCode = 1
else             → runInteractive(opts)
```
`doctor` subcommand → `runDoctor(program.opts())`.

**Edge case (must replicate):** `--json` without a prompt is a usage error (exit 1, message
to stderr). It must NOT launch the TUI (would pollute stdout with non-JSONL).

`main().catch` final guard: prints `render.error(err.stack ?? message)`, `process.exit(1)`.

### 1.3 `buildOverrides(opts)` (async, pre-`App.create`)

1. `overridesFromOptions(opts)`.
2. `projectPath = path.resolve(opts.project ?? cwd)`.
3. `loadProjectInstructions(projectPath)` → `{ content, warning }` (reads repo-root `DAINTREE.md`,
   best-effort, oversize/unreadable → non-fatal `warning`). If warning, `render.warn(warning)`.
4. Returns `{ ...overrides, projectInstructions: content }`.

The instruction-file read is async and lives in the entry path so `loadConfig()` stays sync.

### 1.4 `runOneShot(prompt, opts)` — the scriptable path

- `json = opts.json === true`; `jsonSink = json ? createJsonSink() : undefined`.
- **stdout purity rule (CRITICAL):** in JSON mode, stdout carries ONLY the JSONL stream.
  Every human line (debug-log notice, confirm-skip warning, loop log, *and errors*) → stderr.
- `reportError(err)`: message = `err.stack ?? err.message` (or `String(err)`).
  JSON mode → `jsonSink.sink.error(message)`. Else → `render.error`, `process.exitCode = 1`.
- Boot funnel: `App.create({ overrides: await buildOverrides(opts) })` inside try/catch →
  on throw, `reportError` then (json) `process.exitCode = jsonSink.finish().exitCode` and return.
  So a JSON-mode boot failure still ends stdout with a `result` envelope, never an ANSI trace.
- Debug log: json → `startDebugLog`; if path, write `logging to <path>\n` to **stderr**.
  Non-json → `announceDebugLog(app)` (writes gray `logging to <path>` to stdout via render).
- `app.setHooks({ agentEvents: json ? jsonSink.sink : createConsoleSink(), confirm, log })`.
  - `confirm`: one-shot is non-interactive → **auto-decline**. Message
    `Skipping ${toolName} (${risk}) — confirmation needed; run interactively to approve.`
    json → stderr; else `render.warn`. Returns `false`.
  - `log`: json → stderr `  · ${m}\n`; else `render.line(gray("  · "+m))`.
- Run: `await app.connectMcp(); await app.session.send(prompt)` in try; catch → `reportError`.
- `finally`: `app.shutdown()` in nested try (swallow + route off stdout: json→stderr
  `shutdown error: ...`, else `render.error`). Then if jsonSink:
  `process.exitCode = jsonSink.finish().exitCode` — terminal `result` LAST, after shutdown.

**Ordering invariant:** the `result` envelope is the last stdout line; shutdown happens before it;
no event may interleave after `finish()` flips the `finished` flag.

### 1.5 `runInteractive(opts)` — Bun re-exec + cockpit/REPL routing

**This whole mechanism is DELETED in Go** (single static binary). Record for behavior:
- `wantsCockpit = !opts.classic && stdin.isTTY && stdout.isTTY`.
- `underBun = typeof globalThis.Bun !== "undefined"`.
- If `wantsCockpit && !underBun`: `resolveBunPath()`; if found, `spawnSync(bun, [argv[1], ...argv.slice(2)], {stdio:"inherit"})`, then `process.exit(child.status ?? 0)`.
- `app = App.create(...)`; `ttyOk = stdin.isTTY && stdout.isTTY`.
- If `opts.classic || !ttyOk` → `announceDebugLog(app); startRepl(app)` and return.
- Else (cockpit): `startDebugLog` (open log, print nothing — header badge shows it), then
  lazy `import("../ui/runApp.js"); startCockpit(app)`. On throw → stderr Bun-required notice,
  `announceDebugLog`, fall back to `startRepl`.

`resolveBunPath()` candidate order (DELETE in Go): `$BUN_INSTALL/bin/bun`, `$HOME/.bun/bin/bun`,
`/opt/homebrew/bin/bun`, `/usr/local/bin/bun`, then `which`/`where bun`. `existsSync`-gated, returns null.

**Go routing replacement:** `classic || !isatty(stdin) || !isatty(stdout)` → console/REPL path
(or just the console-sink loop); else Bubble Tea cockpit. No re-exec, no Bun, no lazy import.

### 1.6 `runDoctor(opts)` (the `doctor` subcommand — distinct from `/doctor` slash)

`App.create` → `connectMcp` → `st = mcp.status()`, then prints (plain `render.line`, no checklist):
```
Daintree Assistant — doctor
  fireworks key  : present | "MISSING — set FIREWORKS_API_KEY"
  mcp url        : <url> | (unset)
  mcp connection : ok (<transport>, <toolCount> tools) | <error|"not connected">
  project        : <projectPath>
  instructions   : DAINTREE.md (<bytes> bytes) | (none)
  tools loaded   : <registry.list().length>
  tier           : <tier>
```
Then `app.shutdown()`. NOTE: this is the *terse banner* doctor; the `/doctor` slash command
runs the richer `runDoctor(app)` checklist in `commandData.ts` (§6). Keep both.

### 1.7 `announceDebugLog(app)`

`startDebugLog(app.config, app.sessionId)` → if path returned, `render.line(gray("logging to "+path))`.

---

## 2. `App` wiring & lifecycle (`src/cli/app.ts`)

`App` is the single composition root: builds every dependency once, exposes a `ToolContext`
factory, the main `AgentSession`, and the `Scheduler`. Drives both CLI and cockpit.

### 2.1 Interfaces

`AppHooks { confirm?(req: ConfirmRequest): Promise<boolean>; log?(msg): void; agentEvents?: AgentEventSink }`
`AppCreateOptions { overrides?: ConfigOverrides; hooks?: AppHooks; mcpOptions?: McpClientOptions; sessionId?: string }`

### 2.2 Fields (readonly unless noted)

| Field | Type | Built from |
|---|---|---|
| `config` | `AppConfig` | `loadConfig(overrides)` |
| `db` | `Db` | `new Db(config.dbPath)` |
| `mcp` | `DaintreeMcpClient` | `new DaintreeMcpClient(config, mcpOptions)` |
| `queue` | `Queue` | `new Queue(db)` |
| `router` | `ModelRouter` | `new ModelRouter(config)` |
| `registry` | `ToolRegistry` | `registerAll(buildAllTools()); assertSafe()` |
| `skills` | `SkillRegistry` | `new SkillRegistry()` |
| `sessionId` | string | `opts.sessionId ?? "ses_" + randomUUID().slice(0,8)` |
| `runIdRef` | `{ current?: string }` | mutable per-turn pointer |
| `artifactStore` | `Map<string,string>` (private) | session-scoped oversized-tool-result cache |
| `session` | `AgentSession` (`!`) | built in `create()` |
| `scheduler?` | `Scheduler` | set by `startScheduler()` |
| `hooks` (private) | `AppHooks` | `opts.hooks ?? {}` |

**ID format:** session id = `ses_` + first 8 hex chars of a UUIDv4. Go: `"ses_"+uuid.NewString()[:8]`.

### 2.3 `App.create(opts)` — construction order (replicate exactly)

1. `new App(opts)` (private ctor: builds config→db→mcp→queue→router→registry(+assertSafe)→skills→hooks→sessionId).
2. `restore = rehydrateSession(db.listMessages(sessionId))` — resumes prior turns if this
   sessionId has persisted history; `undefined` for a fresh session.
3. `app.session = new AgentSession({ router, registry, skillRegistry, ctx: buildContext("main"),
   promptContext, sessionId, restoredMessages: restore?.restoredMessages, initialSeq: restore?.initialSeq,
   events: multiSink(new RunEventSink(db, runIdRef), agentEventProxy()), runIdRef })`.

**Sink composition:** `multiSink` wraps (a) durable `RunEventSink`→`run_events` table and
(b) the live `agentEventProxy()`→whatever UI sink is registered via `setHooks`. Isolated so a
DB-write failure can't break the UI stream. Session stamps current run id onto `runIdRef` per turn.

### 2.4 `agentEventProxy()`

Returns a stable `AgentEventSink` whose every method delegates to `this.hooks.agentEvents?.X` —
so `setHooks` swaps the live UI sink without rebuilding the session (which would drop history).
Methods: `assistantStart, assistantToken(token), assistantEnd(content, reasoning),
assistantCancelled(content), toolCall(event), toolResult(event), error(message), info(message),
usage(event)` (usage is `?.()` optional).

### 2.5 `buildContext(actor, actorId?)` → `ToolContext`

`actor: ToolActor` (`"main" | "watcher" | "timer" | "workflow"`-class — main is interactive).
Fields: `config, mcp, db, queue, router, projectPath, sessionId, actor, actorId, artifactStore`,
plus:
- `confirm`: actor==="main" ? `(req)=>(hooks.confirm ?? async()=>false)(req)` : `async()=>false`.
- `log`: `(msg)=>(hooks.log ?? noop)(msg)`.
- `daemonActive`: `()=>Boolean(this.scheduler)` (read live — one-shot never starts it).
- `skillSource`: `this.skills`.
- `loadSkills`: main-only → `(ids)=>this.session.loadAdditionalSkills(ids)` (read `this.session` lazily).
- `findSkills`: main-only → `(query, signal)=>this.session.findSkills(query, signal)`.

Hooks/closures read `this.hooks` **live** so `setHooks` partial updates take effect without rebuild.

### 2.6 `promptContext()` → `MainPromptContext`

Built from `mcp.status()`: `statusLine = connected ? "connected (<transport>, <toolCount|?> tools)"
: "not connected — <error|'no url/token'>"`. Fields: `tier, projectPath, projectId, mcpConnected,
mcpStatusLine, largeModel, smallModel, schedulerActive: Boolean(scheduler), projectInstructions`.

### 2.7 Lifecycle methods

| Method | Behavior |
|---|---|
| `connectMcp()` | `st = await mcp.connect(); warnOnDrift(st); session.refreshRuntimeContext(promptContext())` |
| `reconnectMcp()` | `st = await mcp.reconnect(); warnOnDrift(st); session.refreshRuntimeContext(...)` |
| `warnOnDrift(st)` (private) | if `st.driftToolNames` non-empty: one rollup `log` line: `⚠️  MCP drift: N documented tool(s) not advertised by the live server (a, b, c, +K more). Run /doctor for the full list.` (preview first 3, noun singular/plural) |
| `startScheduler(onAttention?)` | **idempotent**: if `scheduler` exists, `setOnAttention(onAttention)` + return it (rebind callback, don't leak a 2nd interval). Else build `new Scheduler({db, queue, router, registry, ctxFor:(a,id)=>buildContext(a,id), onAttention})`, `start()`, refresh runtime context (scheduler now active), return. |
| `setHooks(hooks)` | `this.hooks = {...this.hooks, ...hooks}` (merge; partial updates keep prior confirm/log) |
| `shutdown()` | `scheduler?.stop(); await scheduler?.drain(); await mcp.close(); db.close()` |

---

## 3. JSONL one-shot contract (`src/cli/jsonSink.ts` + `src/schemas.ts`) — schema v1

### 3.1 Constants (exact)

| Const | Value | Source |
|---|---|---|
| `JSON_OUTPUT_SCHEMA_VERSION` | `1` (plain int, not semver; bump only on breaking line-shape change) | schemas.ts:738 |
| `ONE_SHOT_EXIT_CODE.success` | `0` | turn completed, assistant replied |
| `ONE_SHOT_EXIT_CODE.error` | `1` | model/general error, max iterations, unexpected throw, process catch-all |
| `ONE_SHOT_EXIT_CODE.cancelled` | `2` | turn cancelled mid-flight |
| `ONE_SHOT_EXIT_CODE.toolFailure` | `3` | **RESERVED** — never emitted today (failed tool calls feed back as recoverable context; turn ends 0) |

### 3.2 Event line schema

Every line: `{ type, ts, seq, ...payload }`. `ts = Date.now()` (ms epoch, injectable clock).
`seq` int ≥ 0, **monotonic within a run, starts at 0, incremented on every emit**.
`type ∈ JsonlEventType`:

| `type` | Payload fields | Emitted by |
|---|---|---|
| `assistant:start` | — | `assistantStart()` (flushes buffer first, defensive) |
| `assistant:content` | `content` (buffered round prose) | `flushContent()` on round boundary |
| `assistant:end` | `content` (authoritative final text) | `assistantEnd(text)` |
| `assistant:cancelled` | `content` | `assistantCancelled(text)` |
| `tool:call` | `id, name, args` | `toolCall(event)` (flushes buffer first) |
| `tool:result` | `id, name, ok, summary, error` (null on success), `auditId` (may be absent) | `toolResult(event)` |
| `error` | `message` | `error(message)` (flushes buffer first) |
| `info` | `message` | `info(message)` |
| `result` | (terminal envelope — §3.4) | `finish()` |

These `type` strings deliberately **reuse the durable `RunEventRecord` vocabulary** so the live
JSONL stream and the DB run-log describe a run identically. `result` is the one extra terminal line.

### 3.3 Buffering / flush logic (`createJsonSink`) — replicate exactly

State (closed over): `seq=0`, `contentBuffer=""`, `content=""`, `status:JsonOutputStatus="error"`
(default error — a turn ending with no terminal event is itself a failure), `exitCode=ONE_SHOT_EXIT_CODE.error`,
`errorMessage=null`, `finished=false`.

- `emit(type, payload?)`: if `finished` → drop (no line may follow `result`). Build `{type, ts:now(), seq:seq++, ...payload}`.
  `JSON.stringify`; on throw (circular `details`) emit degraded valid line `{type, ts, seq, serializationError:true}` —
  **never drop a line (would leave a seq gap)**. Write `serialized + "\n"`.
- `flushContent()`: if buffer empty, return. Else emit `assistant:content {content: buffer}`, clear buffer.
- `assistantToken(t)`: `contentBuffer += t` (no emit).
- `assistantEnd(text)`: `contentBuffer=""` (drop streamed dup), `content=text`, `status="success"`,
  `exitCode=0`, `errorMessage=null`, emit `assistant:end {content:text}`.
- `assistantCancelled(text)`: `contentBuffer=""`, `content=text`, `status="cancelled"`, `exitCode=2`,
  emit `assistant:cancelled {content:text}`.
- `toolCall(e)`: `flushContent()` (prose precedes the call) then emit `tool:call {id, name, args}`.
- `toolResult(e)`: emit `tool:result {id, name, ok:e.result.ok, summary:e.result.summary,
  error:e.result.error ?? null, auditId:e.result.auditId}`.
- `error(m)`: `flushContent()`, `status="error"`, `exitCode=1`, `errorMessage=m`, emit `error {message:m}`.
- `info(m)`: emit `info {message:m}` (no flush, no status change).

### 3.4 `finish()` → `{exitCode}` (idempotent terminal)

If already `finished` → return `{exitCode}`. Else `flushContent()` (don't lose trailing prose),
emit `result { schemaVersion:1, status, exitCode, content, error: errorMessage===null ? null : {message:errorMessage} }`,
set `finished=true`, return `{exitCode}`.

`JsonResultEnvelopeSchema` (strict): `{type:"result", ts, seq, schemaVersion:literal(1), status:JsonOutputStatus,
exitCode: 0|1|2, content:string, error: {message:string}|null}`. `JsonOutputStatus ∈ {success, error, cancelled}`.

**Consumer guarantee:** read only the last line to get the outcome; `exitCode` mirrors the process
exit code so a script never needs `$?`.

---

## 4. Console / human sink (`src/cli/consoleSink.ts` + `render.ts`)

`createConsoleSink()` → `AgentEventSink` mapping events to `render`:
- `assistantStart` → `render.assistantStart()` (blank line)
- `assistantToken(t)` → `render.streamToken(t)` (raw stdout write, no newline)
- `assistantEnd` → `render.assistantEnd()` (`"\n"` + line)
- `assistantCancelled` → `render.info("Turn cancelled")` then `render.assistantEnd()`
- `toolCall({name,args})` → `render.line()` then `render.toolCall(name,args)`
- `toolResult({result})` → `render.toolResult(result.ok, result.summary)`
- `error(m)` → `render.line()` then `render.error(m)`
- `info(m)` → `render.info(m)`

### 4.1 `render.ts` — ANSI helpers (dependency-free)

`useColor = !process.env.NO_COLOR && process.stdout.isTTY` — **NO_COLOR respected**; color stripped
when not a TTY. `wrap(code,s)` → `\x1b[<code>m<s>\x1b[0m` or bare `s`.

Color codes (`c.*`): dim=`2`, bold=`1`, red=`31`, green=`32`, yellow=`33`, blue=`34`,
magenta=`35`, cyan=`36`, gray=`90`.

`render` methods: `out(s)` raw; `line(s="")` `s+"\n"`; `streamToken(s)` raw;
`info` `cyan("ℹ ")+s`; `warn` `yellow("⚠ ")+s`; `error` `red("✗ ")+s`; `success` `green("✓ ")+s`;
`toolCall(name,args)` `gray("  ⚙ name(compactArgs)")`; `toolResult(ok,summary)`
`green/red("  ↳") + dim(truncate(summary,200))`; `banner(lines)` blank/lines/blank;
`assistantStart` blank line; `assistantEnd` `"\n"` line. `truncate`/`compactArgs` re-exported
from `../utils/text.js` (truncate limit **200** for tool results; arg preview).

---

## 5. Classic REPL (`src/cli/repl.ts`)

`startRepl(app)`: node `readline` loop. **DELETE readline in Go** → use `bufio.Scanner`/`golang.org/x/term`
or bubbline. Behavior to keep:

1. `ask(q)` = `rl.question`. Wire hooks: `agentEvents=createConsoleSink()`,
   `confirm` = print warn `<bold toolName> (<risk>) wants to run:\n     <summary>\n     args: <compactArgs>`,
   then `ask(yellow("   approve? [y/N] "))`, return `/^y(es)?$/i.test(trim)`.
   `log` = `render.line(gray("  · "+msg))`.
2. `await app.connectMcp()`; `st = mcp.status()`; `app.startScheduler(events => printAttention(events))`.
3. `banner(app)` (§5.1). If `!st.connected`: warn degraded-mode line.
4. Loop: prompt `cyan("\ndaintree ❯ ")`, trim. Empty → continue. Starts with `/` →
   `handleSlashCommand(line, app)`; if `.quit` → exit loop. Else `app.session.send(line)`
   (catch → `render.error`).
5. On quit: `rl.close(); app.shutdown(); render.line(gray("Goodbye."))`.

### 5.1 `banner(app)` lines

`Daintree Assistant  — local operations officer`, `project   <path>`,
`mcp       connected (<transport>) | degraded local mode`,
`models    large=<basename> · small=<basename>` (basename = `split("/").pop()`),
`tier      <tier>`, gray tip line about `/help` and "I never edit files directly."

### 5.2 `printAttention(events: QueueEvent[])`

Per event: blank, `magenta("◆ inbox") bold(title) gray("(severity)")`, `  <summary>`,
if `evidence?.length` → gray `  evidence: <ev.join(" | ")>`. Then re-print prompt
`cyan("\ndaintree ❯ ")` (out-of-band, restores the input line).

---

## 6. Slash command registry & handlers

### 6.1 `COMMAND_REGISTRY` (`src/commandRegistry.ts`) — single source of truth

Pure data, no `App`/UI imports (so `src/cli` and `src/ui` both import freely). `CommandMeta {
name, palette, syntax, help }`. Helpers: `paletteEntries()` → `["/name", palette]` (Ink composer
palette); `overlayEntries()` → `[syntax, help]`; `helpLines(pad=24)` → `syntax.padEnd(pad)+help`
(pad 24 clears widest syntax `/permissions [tier]`).

**Full command list (ordered, exact):**

| name | syntax | palette | help (one-line) |
|---|---|---|---|
| status | `/status` | connection and session | Daintree connection, project, models, tier |
| inbox | `/inbox [sev]` | items requiring attention | watcher/timer events (info\|attention\|urgent) |
| tools | `/tools [query]` | list / search tools | list/search available tools |
| timers | `/timers` | scheduled operations | scheduled timers |
| watchers | `/watchers` | supervised agents | active watchers |
| audit | `/audit [n]` | recent tool calls · export | recent calls (def 15); export <json\|csv> |
| explain | `/explain [runId]` | reconstruct a run's timeline | replay a run; no id lists recent runs |
| models | `/models` | model routing | model routing (large/medium/small tiers) |
| permissions | `/permissions [tier]` | supervisor \| operator \| system | show or set tier (supervisor\|operator\|system) |
| skills | `/skills [sub]` | loaded · find · load · clear | loaded \| find <query> \| load <id…> \| clear |
| compact | `/compact` | summarize the conversation | summarize + reset the conversation |
| clear | `/clear` | reset the conversation | drop the conversation — start fresh |
| doctor | `/doctor` | environment check | check MCP / config / project (with fixes) |
| reconnect | `/reconnect` | retry the Daintree connection | retry the Daintree MCP connection |
| help | `/help` | all commands and keys | this help |
| quit | `/quit` | exit | exit |

**Aliases (handled but NOT listed in registry):** `help` ⇐ `?`; `quit` ⇐ `exit`, `q`.

A registry test asserts BOTH switch statements (`commands.ts` REPL + `commandData.ts` UI) handle
every registry name — adding a command forces both surfaces in sync.

### 6.2 Parsing (both handlers identical)

`const [cmd, ...rest] = line.slice(1).trim().split(/\s+/); const arg = rest.join(" ")`.

### 6.3 Two handler surfaces

- `handleSlashCommand(line, app)` (`commands.ts`) → `CommandResult { handled, quit? }`. **Prints to stdout** via `render`.
- `handleUiCommand(line, app)` (`commandData.ts`) → `UiCommandResult { handled, quit?, title?, text?, switchPanel?, clearTranscript? }`. **Returns structured data**, no printing; the cockpit renders a card / switches a panel.

`PanelKey = "watchers" | "inbox" | "timers" | "audit" | "help"`. `switchPanel` set by inbox/timers/watchers/audit/help.
`clearTranscript: true` only on `/clear` (controller wipes in-flight transcript + remounts; committed
scrollback in host terminal stays — same as shell `clear`).

### 6.4 Per-command behavior (data source = `app.*`; both surfaces share accessors)

| cmd | Behavior / data accessors |
|---|---|
| quit/exit/q | `{handled:true, quit:true}` |
| help/? | REPL: print `HELP` blob. UI: `switchPanel:"help"`, text=`HELP_TEXT`. |
| status | `describeConfig(app.config)` + `mcp.status()`. Line `Daintree MCP: connected (transport, toolCount tools) | disconnected — <error|'no url/token'>` then each config k/v. |
| inbox | `sev = arg if ∈ {info,attention,urgent,blocked} else undefined`. `app.queue.digest({severityAtLeast:sev, maxItems:30})`; `app.queue.format(events)`. Title `Inbox (N)`. |
| tools | `app.registry.list()`; if arg → filter name/description `.includes(q.toLowerCase())`. Row `name.padEnd(26) [risk] description`. Title `Tools (M/N)`. |
| timers | `app.db.listTimers("scheduled")`. Row `id <locale fireAt> — title (payloadType)`. `(none)` if empty. switchPanel timers. |
| watchers | `app.db.listWatchers("active")`. Row `id title — goal [lastClassification|'pending']`. switchPanel watchers. |
| audit | if `rest[0]==="export"` → `parseAuditExportArgs(rest.slice(1))`; on `{error}` show it; else `app.db.queryAudit(filters)` + `serializeAudit(rows, format)` (format json\|csv). Else `n = Number(arg) || 15`; `app.db.listAudit(n)`. Row `<locale time> toolName.padEnd(22) <outcome> <durationMs>ms — summary`. **grant tag:** if `outcome==="grant_ok" && grantSource` → `grant_ok[<grantSource>]`. Color marks (REPL): ok=green, denied=yellow, else red. switchPanel audit. |
| explain | no arg → `app.db.listRuns(10)` + `formatRunList` + hint. With `runId` → `app.db.listRunEvents(runId)`; empty → "No events found…" warn; else `app.db.listAuditByRunId(runId)` + `formatRunTimeline(events, auditRows)`. (No switchPanel — routes through transcript card.) |
| models | `app.router.describe()` → `{large, medium, small}` model ids. Row `key.padEnd(7): value`. |
| permissions | no arg → show `Current tier: <tier>` + tier legend. With arg → `Tier.safeParse(arg)`; bad → "Unknown tier '<arg>'. Use supervisor \| operator \| system."; ok → `app.config.tier = parsed; session.refreshRuntimeContext(promptContext())`; "Tier set to <t>." |
| skills | sub=`rest[0]`. none → `app.skills.list()` rows `id [risk] title — summary` + usage. `loaded` → `session.describeSkills()`. `clear` → `session.setSkills([])`. `load` → ids=`rest.slice(1)`; partition known/unknown via `skills.has(id)`; if none known → unchanged warn; **>3 known → load first 3 (REPL warns)**; `session.setSkills(known)` + describe. `find` → query=`rest.slice(1).join(" ")`; empty → usage; else `session.findSkills(query)` → `!ok` selector-unavailable / `matched` "Loaded: <ids>" / else "No skill matched". |
| doctor | `runDoctor(app)` (§6.5). REPL prints `✓/✗ label.padEnd(16): detail  → fix`. UI: text=`formatDoctor(checks)`. |
| reconnect | REPL prints "Reconnecting…"; `app.reconnectMcp()`; then connected → "Reconnected (transport, toolCount tools)." / "Still not connected — <error>". |
| compact | transcript = non-system messages mapped `role: contentToText(content) || "[tool call]"`, joined, **`.slice(0,12000)`**. `app.router.chat("small", {messages:[sys, user], maxTokens:400})` with system prompt "Summarize this assistant session into a tight brief: goals, decisions, open watchers/timers, and next steps. <= 200 words." Then `app.session.compact(res.content)`. Catch → "Compaction failed: …". |
| clear | `app.session.clear()`. REPL additionally `clearHostTerminal(process.stdout)`. UI sets `clearTranscript:true`. "Conversation cleared — starting fresh." |
| default | "Unknown command /<cmd>. Try /help." |

**Magic numbers in commands:** inbox `maxItems:30`; audit default `n=15`; explain `listRuns(10)`;
skills load cap `3`; compact transcript slice `12000` chars, `maxTokens:400`, "<= 200 words".

### 6.5 `runDoctor(app)` (`commandData.ts`) → `DoctorCheck[]` (the rich `/doctor`)

`DoctorCheck { label, ok, detail, fix? }`. `withTimeout(p, ms, what)` races a `setTimeout(…).unref()`
rejection `"<what> timed out after <ms>ms"`.

Steps (order matters):
1. If `!mcp.isConnected() && cfg.mcpUrl && cfg.mcpToken` → try `app.reconnectMcp()` (swallow).
2. `st = mcp.status()`. `need(v, env) = v ? undefined : "set <env>"`.
3. Checks pushed in order:
   - `fireworks key` — ok=`!!cfg.fireworksApiKey`, detail present/MISSING, fix `set FIREWORKS_API_KEY in .env or the environment`.
   - `large model` / `small model` — ok=`!!cfg.*Model`, fix `set DAINTREE_LARGE_MODEL`/`DAINTREE_SMALL_MODEL`.
   - `mcp url` / `mcp token` — fix `set DAINTREE_MCP_URL to Daintree's MCP endpoint` / `set DAINTREE_MCP_TOKEN`.
   - `mcp connection` — ok=`st.connected`, detail `ok (<transport>)` | error, fix `start Daintree, then run /reconnect`.
   - `mcp tools` — ok=`connected && toolCount>0`, fix `connected but no tools listed; run /reconnect`.
   - **Live probe** (only if `st.connected`): probeTool = `actions.getContext` (workbench-tier, read-only, no confirm). `withTimeout(mcp.listTools(), 5000, "listTools")`; if probeTool not advertised → fail "…workbench tier may be unavailable", fix `verify the MCP token grants at least workbench tier`. Else `withTimeout(mcp.callTool(probeTool, {}), 5000, probeTool)`, measure ms; ok=`!res.isError`, detail `<tool> ok (<ms>ms)` | error text, fix `check Daintree tier/permissions; run /reconnect`. Throw → fail "probe failed: …", fix `connection may be stale; run /reconnect`.
   - `state writable` — write+unlink `<stateDir>/.doctor-probe`; ok=success, fix `ensure the state dir is writable or set DAINTREE_ASSISTANT_STATE_DIR`.
   - `project path` — ok=`statSync(projectPath).isDirectory()`, fix `pass --project <dir> or run from the project root`.
   - `mcp drift` (only if connected && driftToolNames) — ok=true, detail `<N> documented tool(s) not advertised at this tier/plugin config: <names>`.
   - `tier` — ok=true, detail=`cfg.tier`.
   - `tools loaded` — ok=`registry.list().length > 0`, detail=count.

`formatDoctor(checks)` → `✓|✗ label.padEnd(16): detail  → fix` per line.

**Probe timeout = 5000ms** (both `listTools` and the call). Probe tool name `actions.getContext`
is an external contract.

### 6.6 `HELP` (REPL) / `HELP_TEXT` (UI) blobs

REPL `HELP` = bold "Commands" + `helpLines().map("  "+l)` + "" + "Anything else is sent to the assistant."
UI `HELP_TEXT` = `helpLines()` + "" + "Keys: ? help · ^O toggle ops deck · ^C exit. Anything else goes to the assistant."

### 6.7 `formatRunTimeline` / `formatRunList` (shared, both surfaces)

`formatRunTimeline(events, auditRows)`: `auditById = Map(auditRows by id)`. Per event, parse
`payload` JSON defensively (`parseEventPayload` → `{}` on bad). Truncated wrapper `{truncated,bytes,preview}`
→ `… [truncated <type> — <bytes> bytes]` + indented preview. Else by `ev.type`:
`assistant:start`→`▸ assistant`; `assistant:content`→indent(content); `assistant:end`→optional
`  reasoning:`+indent(reasoning,6) then indent(content); `assistant:cancelled`→`■ cancelled[:]`+indent;
`tool:call`→`→ tool <name> <previewArgs>`; `tool:result`→`<✓|✗> tool <name> (<audit.outcome, durationMs>|ok|error)`
+indent(summary); `error`→`⚠ error: <message>`; `info`→`· <message>`; default→`· <type>`.
`previewArgs`: JSON-stringify, drop `{}`, cap **120** chars (slice 117 + `…`). `indent` pad default 4 spaces.

`formatRunList(runs)`: empty → "(no runs recorded yet …)". Else per run
`runId.padEnd(16) <locale firstTs>  <eventCount> event[s]`.

---

## 7. `/clear` host-terminal wipe (`src/cli/terminalClear.ts`)

`HOST_TERMINAL_CLEAR = "\x1b[2J\x1b[3J\x1b[H"` (erase viewport, erase scrollback, cursor home — same
3 escapes `clear` emits on xterm-class terminals, IN ORDER). `clearHostTerminal(stdout)`: only if
`stdout?.isTTY`; write in try/catch (a failed escape must never break `/clear`). Never touches the
alternate buffer (`\x1b[?1049h`) — that fights the inline render model and destroys native scrollback.

Go: write the byte sequence to `os.Stdout` when `term.IsTerminal`; ignore errors.

---

## 8. External contracts to PRESERVE

### 8.1 Env vars + precedence (`src/config.ts`)

**`loadConfig` snapshots the REAL process env (`trustedEnv`) BEFORE loading any `.env`.**
Security controls read from `trustedEnv` or explicit override ONLY — never from a bound project's `.env`
(untrusted code could escalate). `.env` load order: project-root `<projectPath>/.env` (dotenv, never
overrides set vars), then assistant's OWN package `.env` as lower-precedence fallback (skipped if same path).

| Var | Resolution (highest→lowest) | Trusted-only? |
|---|---|---|
| `FIREWORKS_API_KEY` | override → env → `""` | no (merged ok) |
| `FIREWORKS_BASE_URL` | env → default | no |
| `DAINTREE_LARGE_MODEL` | override → env → default | no |
| `DAINTREE_MEDIUM_MODEL` | env → default | no |
| `DAINTREE_SMALL_MODEL` | override → env → default | no |
| `DAINTREE_MCP_URL` | override → env | no |
| `DAINTREE_MCP_TOKEN` | override → env | no |
| `DAINTREE_PROJECT_ID` | override → env | no |
| `DAINTREE_WINDOW_ID` | override → env (env-only flag; never a CLI flag) | no |
| `DAINTREE_ASSISTANT_TIER` | override → **trustedEnv** → default `system` | **YES** |
| `DAINTREE_ASSISTANT_AUTO_APPROVE` (`==="1"`) | override → **trustedEnv** | **YES** |
| `DAINTREE_ASSISTANT_OFFLINE` (`==="1"`) | override → **trustedEnv** | **YES** |
| `DAINTREE_ASSISTANT_STATE_DIR` | override → **trustedEnv** | **YES** |
| `DAINTREE_ASSISTANT_LOG_DIR` | override → **trustedEnv** → `~/.daintree/logs` (path.resolve) | **YES** |
| `DAINTREE_ASSISTANT_DEBUG_LOG` (`==="1"`) | override → merged env | no (logs only to trusted logDir) |
| `DAINTREE_ASSISTANT_NO_SPLASH` (`!=="1"` → splash on) | override → merged env | no (cosmetic) |
| `DAINTREE_ASSISTANT_RESERVED_COLUMNS` | override → merged env → 2 if windowId else 1 | no (cosmetic) |
| `NO_COLOR` | render.ts: any value disables color | — |

**Tier fail-closed:** `Tier.safeParse(rawTier ?? "system")`; invalid explicit value → drop to
`"supervisor"` (least privilege), NOT system.

**DEFAULTS:** `fireworksBaseUrl="https://api.fireworks.ai/inference/v1"`,
`largeModel="accounts/fireworks/models/glm-5p2"`, `mediumModel="accounts/fireworks/models/glm-5p2"`,
`smallModel="accounts/fireworks/models/deepseek-v4-flash"`, `defaultMcpUrl="http://127.0.0.1:45454/mcp"`.
(Note: source DEFAULTS differ from CLAUDE.md prose names — trust the source values above.)

**State paths:** `stateRoot = ~/.daintree/assistant-cli`. `stateDir` = override/trustedEnv, else
`projectId ? <stateRoot>/<projectIdToDir(projectId)> : <stateRoot>`. `dbPath = <stateDir>/state.db`.
Per-project isolation via projectId (concurrent projects don't share `state.db`). windowId read but
does NOT yet affect the path (deferred). `fs.mkdirSync(stateDir, {recursive:true})` at load.

`firstString(...vals)` = first non-empty trimmed string (returns trimmed). `reservedColumns` parse:
`parseInt(raw,10)`, finite → `max(1, parsed)`, else `windowId ? 2 : 1`.

### 8.2 `describeConfig(cfg)` — `/status` view (secret-redacted)

`redact(s) = s ? "<first4>…<last2> (<len>)" : "(unset)"`. Keys (order): projectPath, stateDir, logDir,
projectId, windowId, largeModel, smallModel, fireworksApiKey(redacted), mcpUrl (`(unset → degraded local mode)`),
mcpToken(redacted), tier, splash, reservedColumns, autoApprove, offline, debugLog, projectInstructions
(`<bytes> bytes` | `(none)`).

### 8.3 `McpStatus` (`src/mcp/client.ts`) — consumed across CLI

`{ connected: boolean; transport?: "streamable-http"|"sse"|"injected"|"none"; toolCount?: number;
error?: string; driftToolNames?: string[]; serverInfo? }`.

### 8.4 `Severity` enum (`schemas.ts`): `debug, info, attention, urgent, blocked, done, error`.
(`/inbox [sev]` only accepts `info|attention|urgent|blocked` for `severityAtLeast`.)
`Tier` enum: `supervisor, operator, system`. `ModelTier`: `small, medium, large`.

### 8.5 Prompt-cache key & no-file-edit guard (referenced from CLI boot)
`registry.assertSafe()` runs at `App` construction — rejects forbidden tool name fragments
(`write_file`, `edit_file`, …). Keep this hard gate. `MAIN_PROMPT_CACHE_KEY="daintree-main"` (in
`agent/loop.ts`, not CLI) — a plain unversioned constant; preserve verbatim.

### 8.6 SQLite tables referenced by CLI commands (behavioral ref; Go schema may differ)
- `run_events` (RunEventRecord: type, ts, seq, payload[, runId]) — `listRunEvents(runId)`, RunEventSink writes.
- `audit_log` — `listAudit(n)`, `queryAudit(filters)`, `listAuditByRunId(runId)`; fields incl
  `id, ts, toolName, outcome (ok|denied|grant_ok|error|…), grantSource?, durationMs, summary`.
- timers — `listTimers("scheduled")` (id, fireAt, title, payloadType).
- watchers — `listWatchers("active")` (id, title, goal, lastClassification).
- runs — `listRuns(n)` (runId, firstTs, eventCount); conversation messages — `listMessages(sessionId)`.

---

## 9. Concrete Go mapping proposal

### 9.1 Packages

| Go package | Owns (from TS) |
|---|---|
| `cmd/daintree-assistant` | `main`, flag parsing, top-level routing (index.ts) |
| `internal/cli` | routing (`runOneShot`/`runInteractive`/`runDoctor`), console sink, render, REPL loop |
| `internal/cli/render` | ANSI helpers (render.ts), `NO_COLOR`, color codes |
| `internal/cli/jsonout` | JSONL sink + result envelope (jsonSink.ts) |
| `internal/commands` | slash registry (commandRegistry.ts) + both handlers (commands.ts/commandData.ts) merged |
| `internal/app` | `App` composition root (app.ts) |
| `internal/config` | `loadConfig`, `describeConfig`, env precedence, trustedEnv (config.ts) |
| `internal/schema` | enums + JSONL constants (schemas.ts subset) |
| `internal/tui` | Bubble Tea cockpit (replaces src/ui/OpenTUI — out of scope for this spec) |

### 9.2 Key types / interfaces

- `type AgentEventSink interface { AssistantStart(); AssistantToken(string); AssistantEnd(content, reasoning string); AssistantCancelled(content string); ToolCall(ToolCallEvent); ToolResult(ToolResultEvent); Error(string); Info(string); Usage(UsageEvent) }` — both ConsoleSink and JSONSink implement it.
- `type AppHooks struct { Confirm func(ConfirmRequest) (bool, error); Log func(string); AgentEvents AgentEventSink }`.
- `type CommandResult struct { Handled, Quit bool }` (REPL); `type UICommandResult struct { Handled, Quit bool; Title, Text string; SwitchPanel PanelKey; ClearTranscript bool }`.
- `type DoctorCheck struct { Label string; OK bool; Detail, Fix string }`.
- Result envelope as a struct with `json` tags matching §3.4 exactly; field order is irrelevant
  (JSON), but `error` must serialize `null` (pointer) vs object.

### 9.3 Go libs

| Need | Lib |
|---|---|
| flags/subcommands | `spf13/cobra` + `pflag` (mirrors commander option/argument/subcommand model) |
| TUI cockpit | `charmbracelet/bubbletea` + `bubbles` + `lipgloss` (color, replaces lipgloss for render too) |
| REPL line input | `bufio` or `charmbracelet/bubbline`; `golang.org/x/term` for isatty |
| `.env` | `joho/godotenv` (replicate non-override + trusted-env-snapshot precedence manually) |
| UUID | `google/uuid` (`"ses_"+NewString()[:8]`) |
| SQLite | `modernc.org/sqlite` (pure-Go, no cgo) or `mattn/go-sqlite3` |
| JSON | stdlib `encoding/json` (handle circular/unserializable via recover→degraded line) |
| color/NO_COLOR | `lipgloss` honors `NO_COLOR`; or hand-roll the 8 codes |

### 9.4 Behavioral notes for Go

- **stdout purity**: in `--json`, route *all* diagnostics to `os.Stderr`; never write a non-JSONL byte to stdout.
- **seq monotonicity**: single `int` counter incremented on every emit; degraded line on marshal error keeps the seq.
- **isatty**: `term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))` for both routing and `clearHostTerminal`.
- **trustedEnv**: snapshot `os.Environ()` into a map BEFORE `godotenv.Load`; read tier/auto-approve/offline/state-dir/log-dir from the snapshot only.
- **fail-closed tier**: explicit-but-invalid tier → `supervisor`.

---

## 10. What to DELETE (Node/Bun/React/OpenTUI-specific)

| Delete | Reason / Go replacement |
|---|---|
| `resolveBunPath()` + the Bun re-exec in `runInteractive` | single static Go binary; no runtime hop |
| `underBun` / `globalThis.Bun` checks | n/a |
| Lazy `import("../ui/runApp.js")` + OpenTUI degrade-to-REPL fallback | Bubble Tea is statically linked; no lazy import, no ESM-resolver workaround |
| `commander` | → cobra |
| node `readline` (repl.ts) | → bufio/bubbline |
| `react-reconciler/constants` ESM note, `@opentui/*` | TUI is Bubble Tea (separate spec) |
| `randomUUID` from `node:crypto`, `node:child_process spawnSync`, `node:fs existsSync` (Bun-locate) | google/uuid; os/exec only if ever needed; stdlib os |
| `dotenv` package | → godotenv (re-implement non-override + trusted snapshot) |
| `process.exitCode` / `process.exit` idiom | `os.Exit(code)` at the end of `main` |
| `paletteEntries()`/`overlayEntries()` (Ink-composer/overlay tuples) | re-derive in Bubble Tea views; keep `COMMAND_REGISTRY` data |
| `reservedColumns` rationale tied to xterm.js overlay scrollbar | keep the config knob but the TUI gutter logic is a TUI-spec concern |
| `splash` flag plumbing | optional; TUI-spec decision |

**KEEP (not delete):** `HOST_TERMINAL_CLEAR` escape sequence, `NO_COLOR` handling, every env var name
+ precedence, the JSONL `type` vocabulary + exit codes + `schemaVersion=1`, `ses_` id format, the
`COMMAND_REGISTRY` table verbatim, doctor probe tool `actions.getContext` + 5000ms timeout, the
trusted-env security boundary, fail-closed tier, the auto-decline-confirm one-shot behavior.
