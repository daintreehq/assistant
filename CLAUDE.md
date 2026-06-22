# CLAUDE.md

Guidance for working in this repository.

## What this is

> **Active development, pre-release.** Nothing is shipped yet. Don't preserve
> backward compatibility or version stable surfaces for their own sake — prefer
> the simplest thing. We deliberately do NOT version the system prompt (the
> cache key is a plain, unversioned identifier); just edit the prompt directly.
> The SQLite schema is a single clean baseline (`schemaUserVersion = 1`) — on a
> schema change, hard-reset the DB (`rm -rf ~/.daintree/assistant-cli`) rather than
> accumulate a migration chain.

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

Powered by **Fireworks AI** (OpenAI-compatible, plain `net/http`). Three model tiers:
`large` (`glm-5p2`, main thread), `small` (`deepseek-v4-flash`, watchers/summaries/
classification), `medium` (routes to large in v1).

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
./bin/daintree-assistant doctor              # check MCP / Fireworks key / project / tier
./bin/daintree-assistant host --stdio        # embedded host: stdio NDJSON, PROTOCOL_VERSION 2

# Gates (run both before considering work done)
go test ./...                # all tests (980+ across 44 packages), no network — fakes for MCP + models
go vet ./...                 # static checks
make test-race               # go test -race ./...
gofmt -l .                   # must print nothing (CI fails on unformatted files)
```

`make` targets: `build` · `install` · `test` · `test-race` · `vet` · `fmt` ·
`generate` (`go generate ./...`) · `run` · `clean`. `rtk` can mask the real `go`
output, so verify a clean build/test with the real binary if a result looks off.

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
  models/        Router (router.go), FireworksClient (fireworks.go, net/http SSE), pricing.go,
                 prompts/ (BaseSystemPrompt, BuildRuntimeContextMessage, BuildLoadedSkillsMessage)
  mcp/           Daintree MCP client over the go-sdk (Streamable HTTP, SSE fallback)
  skills/        embedded runbooks: go:embed files/*.md → SkillRegistry; SelectSkills (small model)
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
  e2e/           end-to-end tests only: built-binary, fake Fireworks/MCP, inline-contract, turn/race
```

**Data flow:** `app.App.Create()` builds every dependency once (Store, MCP, Queue,
Router, Registry, Skills, Session) and exposes a `ToolContext` factory. `agent.Session.Send()`
runs a turn: optional auto-compact → push user message → `router.Stream("large", …)`
with a token callback → on tool calls, announce the whole batch (`ToolBatch`) then
`registry.Dispatch()` each in the safe sequence, feed results back (≤ `MaxToolIterations`
= 12) → skill re-selection (`FindSkills`, small model, ≤3). `Dispatch` = validate args →
tier gate (`safety.Decide`) → confirmation/grant → run handler → audit. The daemon
`Scheduler` ticks every 3s, firing due timers and watcher checks. All sub-threads
publish to the **attention queue** instead of interrupting the main thread.

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
- **Prompt-cache stability.** `prompts.BaseSystemPrompt` is the cached prefix — keep it
  byte-stable; dynamic facts live in later control messages (runtime context, loaded
  skills). The Fireworks `prompt_cache_key` is a plain, unversioned constant
  (`domain.MainPromptCacheKey = "daintree-main"`); it only groups requests onto a cache
  node. Editing the prefix just misses on the changed tokens — never stale — so there's
  no version to bump.
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

## Key environment variables

`FIREWORKS_API_KEY` (required) · `DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` /
`DAINTREE_PROJECT_ID` / `DAINTREE_WINDOW_ID` (injected by Daintree) ·
`DAINTREE_ASSISTANT_TIER` (default `system`) · `DAINTREE_ASSISTANT_AUTO_APPROVE` ·
`DAINTREE_ASSISTANT_OFFLINE` · `DAINTREE_ASSISTANT_STATE_DIR` · `DAINTREE_ASSISTANT_DEBUG_LOG` /
`DAINTREE_ASSISTANT_LOG_DIR` · `DAINTREE_{LARGE,MEDIUM,SMALL}_MODEL` · `FIREWORKS_BASE_URL`.
Resolution order: CLI overrides → real process env (snapshotted **before** `.env` loads,
the trusted-env boundary) → project `.env` → assistant's own `.env` → `DEFAULTS`. All in
`internal/config`. State lives under `~/.daintree/assistant-cli/` (`state.db`; per-project
subdir when a project id is set).

## More docs

`README.md` (full overview), `docs/BUBBLE_TEA.md` (cockpit architecture),
`docs/ARCHITECTURE.md`, `docs/DAINTREE_MCP.md`, `docs/FIREWORKS.md`,
`docs/SKILLS.md` (how to author assistant skills).
