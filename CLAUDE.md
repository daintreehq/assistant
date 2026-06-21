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
npm run dev                 # interactive OpenTUI cockpit (runs under Bun — see below)
npm run dev -- --classic    # legacy readline REPL (also used for non-TTY)
npm run dev -- "prompt"     # one-shot, console output, exits
npm run dev -- doctor       # check MCP / Fireworks key / project / tier
npm run ui:gallery          # iterate on the UI with frozen fixtures, no model/MCP

npm test                    # vitest run (non-UI, Node) THEN `bun test tests/ui` (UI)
npm run test:unit           # just the Node/vitest suite
npm run test:ui             # just the Bun cockpit-render suite
npm run typecheck           # tsc --noEmit
npm run build               # tsup → dist/index.js (CLI) + dist/host.js (embedded host)
npm run db:reset            # hard-reset state: rm -rf ~/.daintree/assistant-cli (dev policy
                            # for schema changes — a fresh DB rebuilds from SCHEMA on launch)
```

**Runtime: the cockpit runs under Bun.** OpenTUI's renderer is a native (FFI) core
that needs Bun (or Node ≥26.3.0 `--experimental-ffi`; we target Bun). `npm run dev`
and `ui:gallery` are `bun run …`. Install Bun via `curl -fsSL https://bun.sh/install | bash`.
The published bin (`daintree-assistant` → `dist/index.js`, `#!/usr/bin/env node`) and
Daintree itself launch us under **Node**; when a cockpit is wanted (TTY, not `--classic`)
and we're not under Bun, `runInteractive` **re-execs the process under Bun**
(`resolveBunPath` in `src/cli/index.ts`, `stdio:"inherit"`) so OpenTUI loads and the
cockpit renders in place — MCP env vars pass through, so it reconnects over HTTP. The
cockpit import is **lazy** (only the TTY branch loads `@opentui/*`), so Node-only paths
(`doctor`, `--classic`, one-shot, non-TTY) never touch it; if Bun is absent the cockpit
degrades to the classic REPL instead of crashing. (`@opentui/react`'s bare
`react-reconciler/constants` import fails Node's strict ESM resolver — another reason
Node-run paths must not import it.) SQLite is runtime-adaptive
(`src/storage/sqliteDriver.ts`): `bun:sqlite` under Bun, `node:sqlite` under Node —
never import a SQLite builtin directly.

**There is no ESLint/Prettier/Biome.** The only gates are `npm run typecheck` and
`npm test`. The UI tests render through OpenTUI's native renderer so they run under
**`bun test`** (`tests/ui/**`); everything else stays on vitest/Node (`vitest.config.ts`
excludes `tests/ui`). Run both before considering work done. `rtk` can mask the real
`tsc` output, so verify a clean typecheck with `./node_modules/.bin/tsc --noEmit`. The
full OpenTUI port contract is in `docs/OPENTUI_PORT.md`.

## Layout & architecture

ESM + `NodeNext` (`"type": "module"`, Node ≥22, cockpit under Bun). **Import with
`.js` extensions** even from `.ts` files. SQLite goes through the runtime-adaptive
`storage/sqliteDriver.ts` (`bun:sqlite` | `node:sqlite`), marked `external` in the
tsup build.

```
src/
  cli/        Entry (index.ts), App wiring (app.ts), REPL, console sink, command data
  agent/      AgentSession main loop (loop.ts) + AgentEventSink (events.ts)
  models/     ModelRouter (router.ts), FireworksClient (fireworks.ts), prompts/
  tools/      ToolRegistry (registry.ts), types.ts, and the tool families (*.ts)
  mcp/        DaintreeMcpClient (client.ts) — Streamable HTTP, SSE fallback
  safety/     policy.ts — tier gating, confirmation matrix, no-file-edit guard
  daemon/     scheduler.ts (3s tick) + watcherEngine.ts (terminal watcher FSM)
  storage/    db.ts + sqliteDriver.ts — timers, watchers, events, audit, conversation, grants
  skills/     runbooks: content files in repo-root `skills/*.md` (fileSource), loaded
              into the registry; the model pulls them on demand via `skill.find` (query →
              small-model selector → inject). See docs/SKILLS.md to author one.
  ui/         OpenTUI cockpit: runApp.tsx + DaintreeApp.tsx + ControlRoom.tsx + components/ (the ONLY @opentui importers)
  host/       Embedded Electron utility-process host (protocol.ts, index.ts)
  config.ts schemas.ts queue.ts debugLog.ts watcherCadence.ts
```

**Data flow:** `App.create()` (cli/app.ts) builds every dependency once and exposes
a `ToolContext` factory. `AgentSession.send()` runs a turn: optional auto-compact →
skill re-selection → project tools → `router.stream("large", …)` → for each tool
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
- **UI boundary.** Only `src/ui` imports `@opentui/*`. The runtime emits structured
  events via `AgentEventSink`, consumed by the `UiBridge` or the console sink.
- **Inline cockpit — NEVER the alternate screen (Claude Code model).** The cockpit renders
  on **OpenTUI** (`@opentui/react`, native Zig renderer) INLINE into the terminal's **main**
  screen buffer: `runApp.tsx` calls `createCliRenderer({ screenMode: "main-screen", useMouse: false })`.
  This must stay that way: **Daintree always runs the assistant inside xterm, and the host (xterm)
  must own scrolling** — native mouse-wheel-where-you-hover, scrollbar, selection and copy/paste.
  The alternate screen (and capturing the mouse) would disable all of that, so both are forbidden.
  In main-screen the whole cockpit tree renders inline (`ControlRoom.tsx`): masthead at top, the
  conversation beneath, the live region (status + composer) at the bottom; as content grows the
  older rows scroll into the host's native scrollback. On resize the native renderer reflows the
  whole tree cleanly — there is no Ink `<Static>` line-rewrite, so the old resize-duplication
  hazard is **gone by construction** (a full repaint on resize is intended). The **`Header` still
  has NO full-width rule** — a committed rule would be wrapped by the host on shrink. Content is
  inset one column each side (`LEFT_PAD` + the `reservedColumns` right gutter). Keys come from
  `useKeyboard` (global — gate by view/focus in-handler, since there's no Ink `isActive`); terminal
  size from `useTerminalDimensions()`. Do NOT raw-parse SGR mouse mode. Operations/help are
  **on-demand** views (`^O` / `/panel`, Esc returns) in place of the composer — single column.
  Follow-up (not yet done): `split-footer` + `ScrollbackSurface.commitRows()` is OpenTUI's true
  `<Static>` equivalent (commit finished turns, keep a small live footer) — needs real-terminal
  tuning. See `docs/OPENTUI_PORT.md`. **`OpenTUI <box> defaults to flexDirection:"column"`** (Ink
  `<Box>` was row) — set `flexDirection="row"` explicitly for horizontal layouts.
- **Watcher engine is a state machine, not a poller** (`daemon/watcherEngine.ts`):
  deterministic signals (agentState, exit code, tail regex, timeout) first, the small
  model only when needed, dedupe, publish only meaningful changes; completion is gated
  on a read-only git-cleanliness check before any irreversible action is suggested.
- **Comment style:** dense, "why"-focused block comments on non-obvious logic. Match
  it. Non-UI tests use vitest globals (Node). UI tests use `bun:test` + OpenTUI's
  `testRender`/`captureCharFrame` from `@opentui/react/test-utils` (run with `bun test`).

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
`docs/FIREWORKS.md`, `docs/SKILLS.md` (how to author assistant skills).
