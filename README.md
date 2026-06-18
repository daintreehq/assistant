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

The default interactive experience is the **Daintree Control Room** — a living
orchestration surface, not a chat box. Delegated work forms a visible tree
(request → decision → agent → watcher → outcome), and it adapts to the terminal
width because Daintree usually hosts it in a narrow side panel:

```
◆ DAINTREE  assistant-main                         OPERATOR  ● CONNECTED
YOU
Fix the watcher tests and tell me when the branch is ready.
◆ DAINTREE
I'll delegate the edit and supervise the result.
├─ ✓ Inspected   tests/ui                                      180ms
├─ ✓ Delegated   term_8 · repair watcher tests
╰─ ◌ Watching    tests running · 42 passed                      18s
──────────────────────────────────────────────────────────────────────
› Ask Daintree to supervise, delegate, or inspect…
  / commands   ^O operations                    1 agent active · MCP
```

- **wide** (≥116 cols): the run-oriented transcript on the left, a quiet
  **operations rail** on the right (NOW / ATTENTION / NEXT).
- **standard** (72–115 cols): conversation-first, with a one-line current-operation
  strip below the header.
- **narrow** (<72 cols): a compact identity row, current-operation strip,
  transcript, and an attention line above the composer.

Operational detail is a purposeful **view**, not a text dump: `^O` opens the
operations surface (NOW → NEEDS ATTENTION → AGENTS → SCHEDULED → RECENT, with
watchers and terminals merged into single agent rows and recommended actions
exposed), and `/watchers`, `/inbox`, `/timers`, `/audit` open it directly. `^X`
reveals raw tool args/results in the transcript; `Esc` returns home; `^C` shuts
down the scheduler, MCP, and DB cleanly.

Risky actions raise a full-width **approval sheet** above the composer with a
risk-specific question (e.g. "Push branch to origin?") that defaults visually to
decline and stays readable with color stripped. One-shot prompts and non-TTY
invocations use the console renderer instead.

### UI gallery (visual development)

Iterate on the surface without a live model, scheduler, or MCP connection:

```bash
npm run ui:gallery   # 1 idle · 2 active · 3 attention · 4 approval · 5 degraded
                     # w cycle width (52/80/120) · o operations · x detail · q quit
```

Fixtures use a frozen clock, so screenshots and golden-frame tests stay stable.

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
/permissions [tier]  /recipes [loaded|reload|load <id…>|clear]
/compact  /doctor  /help  /quit
```

In the Ink cockpit these render as command cards (and may focus a deck panel);
in `--classic` mode they print to the console.

## Recipe system

Behavior is steered by **recipes** — short procedural runbooks injected into the
main model's context only when relevant, instead of fine-tuning. The base system
prompt is split into three stable control messages to preserve Fireworks prompt
caching:

1. **base** — the cached prefix, almost never changes
2. **runtime context** — tier, project, MCP status, model ids
3. **loaded recipes** — the bodies of whatever recipes are active

The small model (`deepseek-v4-flash`) selects 0–3 recipes from a metadata-only
view of the library via `router.json("small", …)`, validated against a Zod schema.
Selection is throttled (first turn, every 4th turn, or on a trigger term) so the
cached prefix doesn't churn each message. Drive it manually with `/recipes`
(`loaded` / `reload` / `load <id…>` / `clear`); decisions are written to a
`recipe_selection_log` table for later tuning. See [`src/recipes`](src/recipes).

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
