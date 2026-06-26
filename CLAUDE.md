# CLAUDE.md

Guidance for working in this repository.

## What this is

> **Active development, pre-release.** Nothing is shipped yet. Don't preserve
> backward compatibility or version stable surfaces for their own sake — prefer
> the simplest thing. We deliberately do NOT version the system prompt (the
> cache key is a plain, unversioned identifier); just edit the prompt directly.
> The SQLite schema is a single clean baseline (`schemaUserVersion`, currently 7) — on a
> schema change, hard-reset the DB (`make db-reset`, which wipes the resolved
> state dir, honouring `DAINTREE_ASSISTANT_STATE_DIR`) rather than accumulate a
> migration chain.

`github.com/daintreehq/daintree-assistant` — a single native **Go** binary, a local
CLI **orchestration assistant for Daintree** ("Daintree's local operations officer").
It plans Daintree operations, spawns and supervises visible agent terminals, watches
them with cheap models, schedules timers, and keeps the human's main conversation clean.

**It is NOT a code editor and never edits project files.** When a change is needed
it spawns a *visible* Daintree agent in a worktree (`agentTask.spawnForEdits`) and
supervises it. This is a hard, enforced invariant (see below) — don't add a tool
that writes/edits files.

The **Daintree project itself** lives at `../daintree` (`~/Projects/Daintree/daintree`)
and on GitHub at <https://github.com/daintreehq/daintree>.

Powered by the **Daintree Assistant backend** (`../assistant-backend`), a Daintree-native
HTTP API — **not** OpenAI-compatible. The CLI is a thin local runtime: it sends only the
visible conversation + structured runtime/turn context + its tool inventory, and the
backend owns the system prompt, developer instructions, **skill/runbook selection**, model
choice, prompt assembly, the utility-model prompts, and the upstream model credentials
(DeepSeek, spoken internally behind a provider abstraction). The CLI executes the local
tool calls the backend asks for and streams the assistant's text. See `docs/BACKEND.md`.

> **You have standing permission to edit the backend at `../assistant-backend`.** Many
> fixes here are really backend changes — the base/system prompt, developer instructions,
> skill/runbook bodies, and the utility-model prompts all live in that repo
> (`src/daintree_assistant_server/prompts/` and `.../skills/files/*.md`). When a model
> behaviour, formatting, or skill bug traces to the prompt/skill rather than a local tool
> shape, fix it directly in `../assistant-backend` (prompt/skill changes land there; local
> tool-shape changes land here) — no need to ask first.

**Development endpoint:** hardcoded to `http://127.0.0.1:8473`, unauthenticated — the
assistant supports exactly this one endpoint for now (a later phase swaps in the
production URL + a real login flow). The only override is the dev/test env var
`DAINTREE_BACKEND_URL`; there is no product config knob. Run `../assistant-backend`
locally (`python -m daintree_assistant_server`). The legacy `internal/models` DeepSeek
client/Router is retained transitionally but no assistant turn or utility task uses it.

## Commands

Go ≥ 1.25.8. No npm / bun / node — this is a single static binary.

```bash
# Compile & install
go build -o bin/daintree-assistant ./cmd/daintree-assistant   # local binary → ./bin
go install ./cmd/daintree-assistant                           # → $(go env GOBIN) or $(go env GOPATH)/bin
make build                                                    # trimpath + version ldflags → ./bin
make install                                                  # go install with the same ldflags

# Run
./bin/daintree-assistant                     # interactive Bubble Tea cockpit (TTY, not --classic)
./bin/daintree-assistant --classic           # classic line REPL (also used for non-TTY)
./bin/daintree-assistant "which worktrees are ready?"   # one-shot, prints, exits
./bin/daintree-assistant --json "…"          # one-shot, JSONL events to stdout
./bin/daintree-assistant doctor              # check backend health / MCP / project / tier
./bin/daintree-assistant host --stdio        # embedded host: stdio NDJSON, PROTOCOL_VERSION 2

# Gates (run both before considering work done)
go test ./...                # all tests (1700+ across 45 packages), no network — fakes for MCP + backend
go vet ./...                 # static checks
make test-race               # go test -race ./...
gofmt -l .                   # must print nothing (CI fails on unformatted files)
```

`make` targets: `build` · `install` · `test` · `test-race` · `vet` · `fmt` ·
`generate` (`go generate ./...`) · `run` · `clean` · `db-reset` (hard-reset the
state dir). `rtk` can mask the real `go` output, so verify a clean build/test
with the real binary if a result looks off.

There is **no ESLint/Prettier/Biome equivalent**: the only gates are `go build`,
`go vet`, `go test`, and a `gofmt` check. The full cockpit architecture contract is
in `docs/BUBBLE_TEA.md`.

## Layout & architecture

Module `github.com/daintreehq/daintree-assistant`, `go 1.25.8`. **Import with full
module paths.** SQLite is `modernc.org/sqlite` (pure Go, **no CGO** — `CGO_ENABLED=0`
builds work). MCP is `github.com/modelcontextprotocol/go-sdk`. The cockpit is **Bubble
Tea v2** (`charm.land/bubbletea/v2`, with `bubbles/v2`, `lipgloss/v2`, `glamour/v2`).

```
cmd/daintree-assistant/   main.go — entrypoint: flags → one-shot | doctor | cockpit | classic; injects main.version
internal/
  domain/        pure vocabulary (imports only uuid + stdlib): RiskClass, Tier, ModelTier,
                 RunPhase, ToolResult (Ok/Fail), AgentEvent union, DB-row records, constants
                 (MainPromptCacheKey, MaxToolIterations), WatchCondition DSL, IDs
  config/        LoadConfig(ConfigOverrides) → AppConfig; trusted-env boundary; DEFAULTS
  ports/         interface seams: EventSink, Store, Router, ToolRegistry, MCPClient, Queue
  projectinstructions/  Load(projectPath) → DAINTREE.md (16 KiB cap)
  debuglog/      StartDebugLog / LogDebug / CurrentDebugLogPath (0700/0600, 7-day prune)
  storage/       Store (store.go) over modernc.org/sqlite — timers, watchers, events, audit,
                 conversation, grants, memory; cancels stale watchers on Open
  backend/       Daintree backend client — the CLI's ONLY model gateway. client.go (Respond/
                 RunTask/Health), contracts.go (strict wire envelope), sse.go (named-event
                 meta/delta/done/error parser), tasks.go (server-owned utility tasks). See docs/BACKEND.md
  models/        VESTIGIAL: legacy Router + DeepSeekClient + pricing, retained transitionally —
                 no assistant turn or utility task uses it. prompts/ now holds ONLY MainPromptContext
                 (the structured runtime facts the CLI collects); the prompt-TEXT builders were deleted
  mcp/           Daintree MCP client over the go-sdk (Streamable HTTP, SSE fallback)
  queue/         Queue — attention queue (Publish / Digest / Resolve)
  safety/        policy.go — Decide(risk, tier), tier gating, AlwaysConfirm, no-file-edit guard
  tools/         Registry (registry.go) + Dispatch (dispatch.go) + AssertSafe; tool families in
                 fsx/ mcpx/ mcpwrap/ contextx/ extractionx/ timer/ watcher/ queue/ grant/
                 workflow/ skill/ auditx/ memory/ artifactx/ agenttaskx/
  agent/         Session (session.go) main turn loop + EventSink (events.go)
  daemon/        scheduler.go (3s tick) + watcher.go (terminal watcher state machine)
  app/           App.Create(CreateOptions) — wires every dependency once, exposes the ToolContext factory
  commands/      slash-command catalog + handlers (shared by cockpit & classic REPL)
  cli/           Run(Options) entry, classic REPL (repl.go), CockpitRunner seam, render/, jsonout/
  ui/            Bubble Tea cockpit (the ONLY bubbletea importers): model/update/view, pump,
                 scrollback, splash, composer/ theme/ markdown/
  host/          embedded host (run.go) — stdio NDJSON transport, PROTOCOL_VERSION 2
  terminal/      TTY-gated raw escapes (clear.go) — the ONLY host-scrollback wipe path
  deps/          build-time blank-import anchor (deps.go) — pins go.mod modules; NO runtime effect
  e2e/           end-to-end tests only: built-binary, fake DeepSeek/MCP, inline-contract, turn/race
```

**Data flow:** `app.App.Create()` builds every dependency once (Store, MCP, Queue,
Backend client, Registry, Session) and exposes a `ToolContext` factory. `agent.Session.Send()`
runs a turn: optional auto-compact → push user message → `Backend.RespondStream(req, …)`
(sends the visible conversation + structured `request.runtime`/`request.turn` context + the
local tool inventory + the opaque backend `state` token) with a token callback → the FIRST
SSE `meta` event carries the refreshed state token + the server's `skills` block. **Skill
selection is server-owned**: the backend's selector picks/injects runbook bodies and a
synthetic `skill__load` exchange *before* it calls the upstream model, so the runbook is in
hand for that same generation; the CLI just stores the state token and surfaces newly-loaded
titles. On tool calls, announce the whole batch (`ToolBatch`) then `registry.Dispatch()` each
in the safe sequence, feed results back and re-`RespondStream` (replaying the state token).
`Dispatch` = validate args → tier gate (`safety.Decide`) → confirmation/grant → run handler →
audit. The daemon `Scheduler` ticks every 3s, firing due timers and watcher checks. All
sub-threads publish to the **attention queue** instead of interrupting the main thread.

## Invariants & conventions

- **No file edits.** `tools.Registry.AssertSafe()` (via `safety.AssertNoFileEditTools`)
  rejects, at startup, any tool whose name contains a forbidden fragment (`write_file`,
  `edit_file`, `fs.write`, `apply_patch`, `file.edit`, …). Edits go through
  `agentTask.spawnForEdits` (mode `edit` | `explore`) — the only agent-spawn path.
- **Tool results use the `ToolResult` envelope** via `domain.Ok(summary, result)` /
  `domain.Fail(code, message, opts…)`. Handlers never throw to the caller — `Dispatch`
  recovers panics and returns a `Fail`. Side-channels (audit, debug log) must never
  break a tool call (guard them).
- **Risk classes & tiers** (`safety/policy.go`): risk ∈ read, local, ui, terminal,
  project, external, git, system. Tiers: `supervisor` (read/local/ui), `operator`
  (+terminal/project/external), `system` (+git/system). Mutating classes (`AlwaysConfirm`:
  terminal/project/external/git/system) need confirmation for the interactive `main`
  actor; non-interactive actors (watcher/timer/workflow) need a scoped **automation grant**.
- **Prompt assembly + caching are the BACKEND's job now.** The CLI sends NO system/developer
  prompt; the backend owns the base prompt, skill bodies, prompt assembly, and the DeepSeek
  `prompt_cache_key`. The CLI's only contribution to cache stability is keeping the
  conversation prefix stable: no client-side control prefix
  (`domain.ControlMessageCount == 0`), only `user`/`assistant`/`tool` roles reach the wire,
  and every volatile per-turn fact rides the **structured** `request.runtime` / `request.turn`
  blocks (inert data the backend renders) rather than mutating an earlier message. See
  `docs/BACKEND.md`. (The backend assembles most-stable-first — base prompt → integrations →
  active skill bodies → conversation → synthetic skill-load → runtime/turn context LAST — so
  skill bodies sit in the cached prefix.)
- **Foreground-only daemon.** Watchers/timers tick only while the assistant is open
  (`daemon.Scheduler`, 3s tick, started on interactive paths). Timers persist in SQLite
  and resume next launch. Watchers are **session-scoped**: any left non-terminal are
  cancelled on the next `storage.Store` open — a new session never inherits a prior
  session's watchers. Never imply background supervision.
- **UI boundary.** Only `internal/ui` imports `charm.land/bubbletea/*` (+ bubbles /
  lipgloss / glamour). The runtime emits structured events via `agent.EventSink`,
  consumed by the cockpit's event pump or the console / JSONL sink. Tools never render;
  the model loop never writes to stdout.
- **Inline cockpit on the NORMAL screen buffer — NEVER the alternate screen (Claude
  Code model).** The cockpit is **Bubble Tea v2**. `internal/ui/run.go` builds the
  `tea.Program` with **no `AltScreen`, no mouse capture** (bracketed paste on). Daintree
  always runs the assistant inside xterm, and **the host terminal must own scrolling** —
  native mouse-wheel-where-you-hover, scrollbar, selection, copy/paste. The alt screen
  and mouse capture would disable all of that, so both are forbidden (enforced by
  `internal/ui/view_test.go`: `View()` must contain no `\x1b[?1049h`, and the program
  `View` must report `AltScreen == false` / `MouseMode == MouseModeNone`). A growing
  transcript lives in the host's **native scrollback**: finished turns + the masthead
  commit ONCE via `tea.Println` (a strict, one-in-flight commit queue, masthead-first,
  ack'd by `ScrollbackCommittedMsg`), and only a small **live footer** (in-flight turn +
  status + composer) repaints. NEVER render the whole transcript into the `View()` string
  — that garbles the layout the instant the transcript outgrows the terminal height. The
  **masthead has NO full-width rule** (a committed rule would wrap on host shrink).
  `/clear` is the ONLY scrollback wipe (`internal/terminal/clear.go`,
  `\x1b[2J\x1b[3J\x1b[H`, TTY-gated). See `docs/BUBBLE_TEA.md`.
- **Explicit liveness, ordered turn model.** The active turn is driven by a first-class
  `domain.RunPhase` (Received → Analyzing → Generating → ToolQueued/Running →
  Integrating → Complete/Failed/Cancelled), NOT inferred from "is the assistant text
  empty". A turn is an ordered `[]TurnStep` (prose / tool / status / note), not a flat
  string + a separate activities slice — so `preamble → tools → conclusion` renders in
  true chronological order. See `docs/BUBBLE_TEA.md`.
- **Watcher engine is a state machine, not a poller** (`daemon/watcher.go`):
  deterministic signals (agent state, exit code, tail regex, timeout) first, the small
  model only when needed, dedupe, publish only meaningful changes; completion is gated
  on a read-only git-cleanliness check before any irreversible action is suggested.
- **Fresh starts are fresh.** A new session's cockpit must start clean: no stale
  attention queue items, no old failed turns visible, no leftover watcher status.
  The masthead, status bar, and deck reflect only what happens in this session.
  (Timers persist across runs and resume; conversation events persist in storage
  but aren't surfaced in the UI unless explicitly requested. Only session-derived
  UI surfaces must be blank on startup — stale deck rows or inbox items are bugs.)
- **Comment style:** dense, "why"-focused block comments on non-obvious logic. Match it.
  Tests use Go's `testing` package; UI tests render through Bubble Tea's `View()` and
  assert on the string (no native renderer needed). `:memory:` SQLite and fakes for
  MCP/models — never the network.

## Debug logging

Set `DAINTREE_ASSISTANT_DEBUG_LOG=1` to append a full-fidelity trace (every model
request/response, every tool call with args+result, the watcher lifecycle) to a
**global** dir (default `~/.daintree/logs`, override `DAINTREE_ASSISTANT_LOG_DIR`).
The flag is read from the process env, the bound project's `.env`, or the assistant's
own `.env` fallback. `debuglog.StartDebugLog(cfg, sessionId)` runs once per process at
boot: it deletes logs older than 7 days, opens a **per-session** `<date>-<sessionId>.log`
(never clobbering a prior run, dir 0700 / file 0600), writes a `session.start` header,
and returns the path so the caller can print `logging to <file>`. The cockpit shows a
`◌ LOG` badge + the path when active. The logger is a no-op when disabled and never
throws. Tests pin `DAINTREE_ASSISTANT_DEBUG_LOG=0` / pass an explicit `logDir`.

### Replaying MCP calls by hand (live debugging)

When debug logging is enabled, the **raw Daintree MCP URL + bearer token** are written to
the top of every session log as an `mcp.credentials` line (right after `session.start`,
emitted on each connect/reconnect). With those you can hit the **same** MCP the assistant
uses and see the exact responses a tool got — invaluable when a tool (e.g.
`terminal.extract` / `terminal.awaitAll`) behaves wrong but the model loop only shows the
post-parse result. (This credential line is a TEMPORARY debug aid — see the `TODO: remove`
on `App.logMcpCredentials` in `internal/app/lifecycle.go`; it only writes under
`DAINTREE_ASSISTANT_DEBUG_LOG=1`, never in normal runs.)

Connecting (Streamable HTTP, `github.com/modelcontextprotocol/go-sdk`): POST JSON-RPC to
the URL with `Authorization: Bearer <token>`, `Content-Type: application/json`, and
`Accept: application/json, text/event-stream`. Handshake = `initialize` (capture the
`Mcp-Session-Id` response header) → `notifications/initialized` → then `tools/call`
(`terminal.list`, `terminal.getStatus`, `terminal.getOutput`, …) passing
`Mcp-Session-Id` on every follow-up. Responses come back as SSE `data:` lines.

To understand what a finish-wait actually *fetches* and how it decides "done", read
`internal/app/toolterminal.go` (`ReadStatuses` → `terminal.getStatus` with
`includeOutput`; `ReadOutput` → `terminal.getOutput`) alongside
`internal/tools/extractionx/awaitall.go` (the cohort poll loop) and
`internal/domain/finish.go` (`FinishPreFilter` — note an empty/whitespace tail is never
judged, so a status read that returns blank `recentOutput` strands a finished agent).

### Reading logs to improve the system (the core dev loop)

The session logs in `~/.daintree/logs/<date>-<sessionId>.log` are the **ground truth**
for how the model and tools actually behaved — not how we assume they behave. A
recurring, first-class development activity here is: the user hands you a real session
log (or a complaint about a session), you read it to find where the model misjudged,
misused a tool, or got confused, and you fix the **system** so the same mistake can't
recur. Treat every "the assistant did X wrong" report as a log-archaeology task, then a
prompt/schema/tool fix — not a one-off patch.

How to read a log fast (it is structured text — grep it, don't eyeball megabytes):
- `tool.call … ok=false` / `outcome=error` — every rejected or failed tool call, with
  the offending `args:` block and the `error:` envelope (code + message) right under it.
  This is the highest-signal entry point: it shows the EXACT arguments the model emitted.
- `model.request` / `model.response` — the full prompt sent and the assistant's reply
  (including `toolCalls` with raw `arguments` JSON). Use these to see what the model was
  *told* vs. what it *did*.
- `watcher.created` / `spawn.launched` / `watcher.*` — the watcher/agent lifecycle.

The fix philosophy — **fix the guidance, not just the symptom.** When the model misuses
a tool, the root cause is almost always ambiguous or misleading instruction, NOT a dumb
model. The model can only act on what the base prompt + skills (now **backend-owned**, in
`../assistant-backend/src/daintree_assistant_server/prompts/` and `.../skills/files/*.md`)
and the local tool `Description`/`Schema` told it. So a model mistake is usually a
*documentation* bug in one of those surfaces — and the durable fix updates them in lockstep
so the model can't repeat it. **A prompt/skill fix lands in the `../assistant-backend` repo;
a tool-shape fix lands here.** Prefer making the correct shape impossible to get wrong (show
literal argument shapes, not prose abstractions) over adding lenient parsing.

Worked example (2026-06-23): the model called `agentTask.spawnForEdits` with a flattened
key `"watcher<arg_key>create": true` and the strict decoder rejected it
(`json: unknown field`). Root cause: the prompt + skill described the arg in prose as the
dotted path `watcher.create: true`, but the schema is a **nested object**
`watcher: {create, goal, cadenceMs}`. The model encoded the dotted prose literally. Fix:
the playbook and skill now show `watcher: {"create": true, "goal": "..."}` explicitly and
warn against a dotted/flattened key — no code change, a prose fix at the source of the
confusion. (Editing the base prompt is free here: it just cache-misses on the changed
tokens, never goes stale — see the prompt-cache invariant above.)

## Key environment variables

`DAINTREE_BACKEND_URL` (dev/test override of the hardcoded `http://127.0.0.1:8473`; the
**backend** holds `DEEPSEEK_API_KEY`, not the CLI) · `DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` /
`DAINTREE_PROJECT_ID` / `DAINTREE_WINDOW_ID` (injected by Daintree) ·
`DAINTREE_ASSISTANT_TIER` (default `system`) · `DAINTREE_ASSISTANT_AUTO_APPROVE` ·
`DAINTREE_ASSISTANT_OFFLINE` · `DAINTREE_ASSISTANT_STATE_DIR` · `DAINTREE_ASSISTANT_DEBUG_LOG` /
`DAINTREE_ASSISTANT_LOG_DIR`. (The old `DEEPSEEK_API_KEY` / `DEEPSEEK_BASE_URL` /
`DAINTREE_{LARGE,MEDIUM,SMALL}_MODEL` knobs now configure the **backend** or feed the
vestigial `internal/models` Router; the CLI no longer requires a model key to start.)
Resolution order: CLI overrides → real process env (snapshotted **before** `.env` loads,
the trusted-env boundary) → project `.env` → assistant's own `.env` → `DEFAULTS`. All in
`internal/config`. State lives under `~/.daintree/assistant-cli/` (`state.db`; per-project
subdir when a project id is set).

## More docs

`docs/BACKEND.md` (**the backend integration — read this for the model / skill / prompt
story**), `docs/SKILLS.md` (how server-owned skills work + the local run-tracking tools),
`README.md` (full overview), `docs/BUBBLE_TEA.md` (cockpit architecture),
`docs/ARCHITECTURE.md`, `docs/DAINTREE_MCP.md`. **STALE — predates the backend migration,
do not trust:** `docs/DEEPSEEK.md` (the direct DeepSeek client — the CLI no longer talks to
DeepSeek; the backend does). Skill authoring + the model live in `../assistant-backend`
(its `skills/files/*.md` + `docs/DAINTREE_API.md`).
