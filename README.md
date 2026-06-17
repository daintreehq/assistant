# Daintree Assistant CLI

A local command-line **orchestration assistant for Daintree** — Daintree's "local
operations officer." It understands the current workspace, plans Daintree
operations, spawns and supervises agent terminals, watches them with cheap models,
schedules timers, and keeps the human's main conversation clean.

It is **not** a code editor. It never edits project files. When a change is needed
it spawns a *visible* agent terminal inside Daintree and supervises it.

Powered by **Fireworks AI** (OpenAI-compatible): a large model
(`minimax-m3`) runs the main thread; a small fast model
(`deepseek-v4-flash`) does watchers, summaries, and classification.

> This prototype ships with built-in system prompts and talks to Fireworks
> directly. In the final product these are replaced by the hosted backend.

## Quick start

```bash
npm install
cp .env.example .env      # then set FIREWORKS_API_KEY (already present in this repo's .env)

# Interactive Ink cockpit (full-screen TUI)
npm run dev

# Legacy readline REPL / non-TTY-safe
npm run dev -- --classic

# Ink without the alternate (full-screen) buffer — keeps scrollback
npm run dev -- --no-alt-screen

# One-shot (console output)
npm run dev -- "which worktrees are ready for review?"

# Environment check
npm run dev -- doctor
```

The default interactive experience is an **Ink operations cockpit**: a streaming
chat/decisions timeline on the left and a live **Operations Deck** on the right
(watchers, watched-terminal previews, attention inbox, scheduled timers, recent
audit). Risky actions raise an in-UI confirmation modal; `?` toggles help, `^O`
toggles the deck, `^C` shuts down the scheduler, MCP, and DB cleanly. One-shot
prompts and non-TTY invocations use the console renderer instead.

Build a standalone binary entry: `npm run build` → `dist/index.js` (exposed as the
`daintree-assistant` bin).

## How it connects to Daintree

Daintree launches the CLI and injects the MCP connection via environment:

```
DAINTREE_MCP_URL=http://127.0.0.1:45454/mcp
DAINTREE_MCP_TOKEN=<bearer>
DAINTREE_PROJECT_ID=<id>
```

The CLI connects over **Streamable HTTP** (falling back to legacy SSE) with the
bearer token. Without these it runs in **degraded local mode**: filesystem,
timer, watcher, and queue tools work; Daintree orchestration tools report a clean
"not connected" error. Pass them explicitly with `--mcp-url` / `--mcp-token`.

## Architecture

```
User ↔ Ink UI ↔ UiBridge ↔ AgentSession (large model)
       (events/confirm)  │  tools (function calling)
                         ▼
            ToolRegistry ── safety policy (tiers, confirm, NO file edits) ── audit
                  │
   ┌──────────────┼─────────────────────────────┐
   fs (read-only) Daintree MCP (raw + wrappers)  CLI tools (timer/watcher/queue)
                  │
            Scheduler (daemon) ── timers + terminal watchers (small model)
                  │
            Queue / inbox ──► main thread (digest only, never raw logs)
```

- **Three model tiers** (`small`/`medium`/`large`); v1 routes medium→large.
- **Durable state** in SQLite (`node:sqlite`, no native build) under
  `~/.daintree/assistant-cli/state.db` — timers, watchers, events, audit,
  conversation. Survives restarts; timers do sleep catch-up.
- **Terminal watchers** are small state machines: deterministic signals first,
  then the small model, then dedupe + publish only meaningful changes.
- **Permission tiers**: `supervisor` (read-only), `operator` (+spawn/create),
  `system` (+git/destructive). Mutating actions confirm; file edits are forbidden
  and delegated to a spawned agent (`agentTask.spawnForEdits`).

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md),
[`docs/FIREWORKS.md`](docs/FIREWORKS.md), and
[`docs/DAINTREE_MCP.md`](docs/DAINTREE_MCP.md).

## Commands (cockpit or classic REPL)

```
/status  /inbox  /tools [q]  /timers  /watchers  /audit  /models
/permissions [tier]  /compact  /doctor  /help  /quit
```

In the Ink cockpit these render as command cards (and may focus a deck panel);
in `--classic` mode they print to the console.

## Tools the model can call

| Group        | Tools                                                                 |
| ------------ | -------------------------------------------------------------------- |
| Project read | `fs.list` `fs.read` `fs.search`                                       |
| Daintree     | `daintree.status` `daintree.listTools` `tool.search` `daintree.call` |
| Context      | `context.snapshot` `terminal.summarize`                              |
| Timers       | `timer.schedule` `timer.list` `timer.cancel`                         |
| Watchers     | `watcher.terminal.create` `watcher.list` `watcher.cancel`           |
| Queue        | `queue.publish` `queue.digest` `queue.resolve`                       |
| Agent tasks  | `agentTask.spawnForEdits` (the no-file-edit escape hatch)            |

## Testing

```bash
npm test          # vitest, no network — fakes for MCP + models
npm run typecheck
```

## Notes / roadmap

- The daemon runs **in-process** in this prototype. State lives in SQLite so it's
  ready to split into a detachable background process (spec §5.1).
- **UI boundary:** the runtime (App, AgentSession, ToolRegistry, Scheduler, Db,
  Queue, MCP, Router) emits structured events and exposes state; the Ink layer
  under `src/ui` is the only thing that imports Ink. Tools never call Ink, the
  watcher engine never renders, and the model loop never writes to stdout — it
  emits through an `AgentEventSink` consumed by either the Ink `UiBridge` or the
  legacy console sink (`src/cli/consoleSink.ts`).
- Workflow templates (start_issue, merge_supervision, …) and worktree watchers
  are the next phase on top of this foundation.
