# CLAUDE.md

Guidance for working in this repository.

## What this is

> **Active development, pre-release.** Nothing is shipped yet. Don't preserve
> backward compatibility or version stable surfaces for their own sake — prefer
> the simplest thing. We deliberately do NOT version the system prompt (the
> cache key is a plain, unversioned identifier); just edit the prompt directly.

`@daintreehq/daintree-assistant` — a local CLI **orchestration assistant for
Daintree** ("Daintree's local operations officer"). It plans Daintree operations,
spawns and supervises visible agent terminals, watches them with cheap models,
schedules timers, and keeps the human's main conversation clean.

**It is NOT a code editor and never edits project files.** When a change is needed
it spawns a *visible* Daintree agent in a worktree (`agentTask.spawnForEdits`) and
supervises it. This is a hard, enforced invariant (see below) — don't add a tool
that writes/edits files.

The **Daintree project itself** lives at `../daintree` (`~/Projects/Daintree/daintree`)
and on GitHub at <https://github.com/daintreehq/daintree>.

Powered by **Fireworks AI** (OpenAI-compatible). Three model tiers: `large`
(`minimax-m3`, main thread), `small` (`deepseek-v4-flash`, watchers/summaries/
classification), `medium` (routes to large in v1).

## Commands

```bash
npm run dev                 # interactive Ink cockpit (tsx, no build)
npm run dev -- --classic    # legacy readline REPL (also used for non-TTY)
npm run dev -- "prompt"     # one-shot, console output, exits
npm run dev -- doctor       # check MCP / Fireworks key / project / tier
npm run ui:gallery          # iterate on the UI with frozen fixtures, no model/MCP

npm test                    # vitest run (no network; MCP + models are faked)
npx vitest run tests/config.test.ts   # a single test file
npm run typecheck           # tsc --noEmit
npm run build               # tsup → dist/index.js (CLI) + dist/host.js (embedded host)
```

**There is no ESLint/Prettier/Biome.** The only gates are `npm run typecheck` and
`npm test` (this is also what CI runs — `.github/workflows/ci.yml`). Run both before
considering work done. `rtk` can mask the real `tsc` output, so verify a clean
typecheck with `./node_modules/.bin/tsc --noEmit` directly.

## Layout & architecture

ESM + `NodeNext` (`"type": "module"`, Node ≥22). **Import with `.js` extensions**
even from `.ts` files. `node:sqlite` is used directly (no native build) and is
marked `external` in the tsup build.

```
src/
  cli/        Entry (index.ts), App wiring (app.ts), REPL, console sink, command data
  agent/      AgentSession main loop (loop.ts) + AgentEventSink (events.ts)
  models/     ModelRouter (router.ts), FireworksClient (fireworks.ts), prompts/
  tools/      ToolRegistry (registry.ts), types.ts, and the tool families (*.ts)
  mcp/        DaintreeMcpClient (client.ts) — Streamable HTTP, SSE fallback
  safety/     policy.ts — tier gating, confirmation matrix, no-file-edit guard
  daemon/     scheduler.ts (3s tick) + watcherEngine.ts (terminal watcher FSM)
  storage/    db.ts — node:sqlite: timers, watchers, events, audit, conversation, grants
  recipes/    runbooks injected into the prompt when relevant (registry/selector/render)
  ui/         Ink TUI: ControlRoom.tsx + DaintreeInkApp.tsx + components/ (the ONLY Ink importers)
  host/       Embedded Electron utility-process host (protocol.ts, index.ts)
  config.ts schemas.ts queue.ts debugLog.ts watcherCadence.ts
```

**Data flow:** `App.create()` (cli/app.ts) builds every dependency once and exposes
a `ToolContext` factory. `AgentSession.send()` runs a turn: optional auto-compact →
recipe re-selection → project tools → `router.stream("large", …)` → for each tool
call, `registry.dispatch()` → feed results back (≤12 iterations). `dispatch` =
validate args (Zod) → tier gate (`decide`) → confirmation/grant → run handler →
audit. The daemon `Scheduler` ticks every 3s firing due timers and watcher checks.
All sub-threads publish to the **attention queue** instead of interrupting the main
thread.

## Invariants & conventions

- **No file edits.** `ToolRegistry.assertSafe()` rejects any tool whose name matches
  forbidden fragments (`write_file`, `edit_file`, `fs.write`, …) at startup. Edits go
  through `agentTask.spawnForEdits` (mode `edit`|`explore`).
- **Tool results use the `ToolResult` envelope** via `ok(summary, result?)` /
  `fail(code, message, opts)` from `tools/types.ts`. Handlers never throw to the
  caller — the registry catches and returns `fail`. Side-channels (audit, debug log)
  must never break a tool call (wrap in try/catch).
- **Risk classes & tiers** (`safety/policy.ts`): risk ∈ read, local, ui, terminal,
  project, external, git, system. Tiers: `supervisor` (read/local/ui), `operator`
  (+terminal/project/external), `system` (+git/system). Mutating classes
  (`ALWAYS_CONFIRM`) need confirmation for the interactive `main` actor; non-interactive
  actors (watcher/timer/workflow) need a scoped **automation grant**.
- **Prompt-cache stability.** The base system prompt (`models/prompts/base.ts`) is
  the cached prefix — keep it byte-stable; dynamic facts live in later control
  messages. The Fireworks `prompt_cache_key` is a plain, unversioned constant
  (`MAIN_PROMPT_CACHE_KEY = "daintree-main"` in `agent/loop.ts`); it only groups
  requests onto a cache node. Editing the prefix just misses on the changed tokens
  — it never serves stale content — so there's no version to bump.
- **Foreground-only daemon.** Watchers/timers tick only while the assistant is open.
  Timers persist in SQLite and resume next launch. Watchers are **session-scoped**:
  they supervise terminals that live only for the session, so any left non-terminal
  are cancelled on the next `Db` construction (`cancelStaleWatchers`) — a new session
  never inherits a prior session's watchers. Never imply background supervision.
- **UI boundary.** Only `src/ui` imports Ink. The runtime emits structured events via
  `AgentEventSink`, consumed by the Ink `UiBridge` or the console sink.
- **Inline cockpit, native scrollback (Claude Code model).** The cockpit renders into the
  terminal's **main** screen buffer (NOT the alternate buffer — `runInkApp.tsx` no longer
  passes `alternateScreen`), so the host terminal's own scrollback / mouse wheel / selection
  work natively. `ControlRoom.tsx` splits the transcript at the trailing **active** turn:
  completed cells (immutable per `transcriptReducer`) are committed once via Ink **`<Static>`**
  — they flow into native scrollback and never repaint — while the in-flight turn, status line
  and composer are the repainting region pinned at the bottom. The header (`Header`) is printed
  ONCE as Static item 0 and is allowed to scroll away; it is NOT sticky. This supersedes the
  earlier "full-screen multi-layout is canonical" decision: the pinned sidebar/standard/wide
  banding and `SidebarHome`/`OpsRail`/`AttentionBanner` are gone from the live shell. Operations
  and help are **on-demand** views (`^O` / `/panel`, Esc returns) rendered in place of the
  composer — never pinned, since a pinned full-screen panel is mutually exclusive with native
  scrollback. Do NOT reintroduce the alternate buffer or raw-parse SGR mouse mode (it fights
  Ink's stdin; see `useAttentionSignal.ts`). "DEC 2026" = synchronized output (flicker), not
  scrolling. `<Static>` requires committed cells stay append-only — keep the reducer mutating
  only the trailing active turn.
- **Watcher engine is a state machine, not a poller** (`daemon/watcherEngine.ts`):
  deterministic signals (agentState, exit code, tail regex, timeout) first, the small
  model only when needed, dedupe, publish only meaningful changes; completion is gated
  on a read-only git-cleanliness check before any irreversible action is suggested.
- **Comment style:** dense, "why"-focused block comments on non-obvious logic. Match
  it. Tests use vitest globals (no imports needed) and `ink-testing-library` for UI.

## Debug logging

Set `DAINTREE_ASSISTANT_DEBUG_LOG=1` to append a full-fidelity trace (every model
request/response, every tool call with args+result, the watcher lifecycle) to a
**global** dir (default `~/.daintree/logs`, override `DAINTREE_ASSISTANT_LOG_DIR`).
The flag is read from the process env, the bound project's `.env`, or the assistant's
own `.env` fallback. `startDebugLog(cfg, sessionId)` runs once per process at boot: it
deletes logs older than 7 days, opens a **per-session** `<date>-<sessionId>.log` (never
clobbering a prior run), writes a `session.start` header, and returns the path so the
caller can print `logging to <file>`. The cockpit shows a `◌ LOG` badge + the path
when active. The logger (`src/debugLog.ts`) is a no-op when disabled and never throws.
`vitest.config.ts` pins `DAINTREE_ASSISTANT_DEBUG_LOG=0` so tests never write logs;
debug-log unit tests pass an explicit `logDir`.

## Key environment variables

`FIREWORKS_API_KEY` (required) · `DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` /
`DAINTREE_PROJECT_ID` / `DAINTREE_WINDOW_ID` (injected by Daintree) ·
`DAINTREE_ASSISTANT_TIER` (default `system`) · `DAINTREE_ASSISTANT_AUTO_APPROVE` ·
`DAINTREE_ASSISTANT_OFFLINE` · `DAINTREE_ASSISTANT_DEBUG_LOG` /
`DAINTREE_ASSISTANT_LOG_DIR` · `DAINTREE_{LARGE,MEDIUM,SMALL}_MODEL`. Resolution
order: CLI overrides → env → project `.env` → assistant's own `.env` → defaults.
All loaded in `src/config.ts`. State lives under `~/.daintree/assistant-cli/`.

## More docs

`README.md` (full overview), `docs/ARCHITECTURE.md`, `docs/DAINTREE_MCP.md`,
`docs/FIREWORKS.md`.
