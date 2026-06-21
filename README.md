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

# Interactive cockpit (inline — renders in the terminal's main buffer, native scrollback)
npm run dev

# Legacy readline REPL / non-TTY-safe
npm run dev -- --classic

# One-shot (console output)
npm run dev -- "which worktrees are ready for review?"

# Environment check
npm run dev -- doctor
```

The default interactive experience is the **Daintree cockpit**, rendered *inline* in
the terminal's **main** screen buffer (the Claude Code model) — never the alternate
screen. This keeps scrolling where it belongs: the host terminal (xterm, in Daintree)
owns the scrollback, so the mouse wheel scrolls wherever it hovers, and selection and
copy/paste work natively. Completed turns are committed once (via Ink `<Static>`) and
flow into that native scrollback; the masthead is plain text that scrolls away with
them (no pinned full-width rule — a committed rule would wrap and break on resize). The
in-flight turn, a status line, and the composer are the small repainting region at the
bottom. On resize the host reflows the scrollback natively and Ink only repaints that
region — there is no monkeypatching of Ink's erase. Content is inset one column on each
side. There is no column-banded layout: the same single surface renders at every width.

```
◆ DAINTREE  assistant-main           OPERATOR  ● CONNECTED

YOU
╭────────────────────────────────────────────────────────╮
│ Fix the watcher tests and tell me when the branch is … │
╰────────────────────────────────────────────────────────╯
◆ DAINTREE
I'll delegate the edit and supervise the result.
├─ ✓ Delegated   term_8 · repair watcher tests
╰─ ◌ Watching    tests running · 42 passed              18s
──────────────────────────────────────────────────────────
◌ Watching term_8 · 18s                    agents 1 · tmr 1
› Ask Daintree…
  / commands · ^O ops · ^X detail
```

User messages render as a distinct, dimmer **boxed card** (theme-aware via
`DAINTREE_THEME=dark|light|ansi|none`, never a hard-coded bright block) while
Daintree's own prose stays unboxed under a `◆ DAINTREE` marker — so "who said
what" is unmistakable even with color stripped.

Operational detail is a purposeful **view**, not a text dump: `^O` toggles the
operations deck (NOW → NEEDS ATTENTION → AGENTS → SCHEDULED → RECENT, with
watchers and terminals merged into single agent rows and recommended actions
exposed), and `/watchers`, `/inbox`, `/timers`, `/audit` open it focused on one
section. `^X` toggles raw tool args/results in the transcript; `Esc` returns home
from any view; `^C` shuts down the scheduler, MCP, and DB cleanly. These on-demand
views render *in place of the composer* and never as pinned panels — a pinned
full-screen panel would be mutually exclusive with the native scrollback.

Risky actions raise a full-width **approval sheet** above the composer with a
risk-specific question (e.g. "Push branch to origin?") that defaults visually to
decline and stays readable with color stripped. One-shot prompts and non-TTY
invocations use the console renderer instead.

### UI gallery (visual development)

Iterate on the surface without a live model, scheduler, or MCP connection:

```bash
npm run ui:gallery   # number keys switch fixtures (idle · active · attention ·
                     # approval · degraded · timers · fleet · long message)
                     # w width (55/58/62/65/80/120) · o ops · x detail · q quit
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

| Group        | Tools                                                                       |
| ------------ | --------------------------------------------------------------------------- |
| Project read | `fs.list` `fs.read` `fs.search`                                             |
| Daintree     | `daintree.status` `daintree.listTools` `tool.search` `daintree.call`       |
| Context      | `context.snapshot` `terminal.summarize`                                    |
| Timers       | `timer.schedule` `timer.list` `timer.cancel`                               |
| Watchers     | `watcher.terminal.create` `watcher.list` `watcher.cancel`                 |
| Queue        | `queue.publish` `queue.digest` `queue.resolve`                             |
| Extraction   | `terminal.extract` `terminal.extract.async`                               |
| Agent tasks  | `agentTask.spawnForEdits` (the no-file-edit escape hatch)                  |
| Grants       | `grant.create` `grant.list` `grant.revoke`                                 |
| Workflows    | `workflow.create` `workflow.get` `workflow.list` `workflow.update`        |
| Recipe runs  | `recipe.step.advance` `recipe.run.get` `recipe.load`                       |
| Audit        | `audit.export`                                                             |
| Memory       | `memory.recall` `memory.list` `memory.save` `memory.forget` `memory.pin` `memory.unpin` |
| Artifacts    | `artifact.read`                                                            |

## Debug logging

A full-fidelity trace for debugging the assistant itself. When enabled, it appends
**everything** — every model request and response (full message arrays), every
tool/function call with its arguments and result, and the whole watcher lifecycle —
to a single human-readable log. These logs are intentionally large and untruncated.

**Enable it** by setting `DAINTREE_ASSISTANT_DEBUG_LOG=1`. The flag is read from the
process environment, the bound project's `.env`, **or the assistant's own `.env`**
(a low-precedence fallback), so it takes effect even when Daintree embeds the
assistant against another project. It's already set in this repo's `.env`.

**Where it writes:** a **global** directory (default `~/.daintree/logs`, override
with `DAINTREE_ASSISTANT_LOG_DIR`), so one place covers every session regardless of
which project it was bound to. Each run gets its **own** file named by session date
and id — `<YYYY-MM-DD>-<sessionId>.log` — so a new instance never clobbers a previous
run's log.

```bash
ls -t ~/.daintree/logs | head        # newest session logs
tail -f ~/.daintree/logs/2026-06-18-ses_ab12cd34.log
```

**On startup** (when logging is on) the assistant prints `logging to <file>` and
shows it in the cockpit header alongside a `◌ LOG` badge, the new file opens with a
`session.start` header (the project it was launched in, tier, models, MCP target),
and any log older than **7 days** is deleted as part of boot.

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
- Workflows, recipes, and persistent memory are implemented tool surfaces
  (`workflow.*`, `recipe.*`, `memory.*`). Future phases target Daintree-owned
  watch-sets over MCP (option C in the scheduler decision record), which would let
  supervision tick without the assistant open.
