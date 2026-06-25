# Daintree Assistant

A single native **Go** binary: a local command-line **orchestration assistant for
Daintree** — Daintree's "local operations officer." It understands the current
workspace, plans Daintree operations, spawns and supervises agent terminals, watches
them with cheap models, schedules timers, and keeps the human's main conversation clean.

It is **not** a code editor. It never edits project files. When a change is needed it
spawns a *visible* agent terminal inside Daintree and supervises it.

Powered by **DeepSeek AI** (OpenAI-compatible Chat Completions over `net/http`):
`deepseek-v4-flash` runs every tier — the main thread (orchestration) as well as
watchers, summaries, and classification. Flash is the validated orchestration model;
the loaded skills supply the playbooks, so a heavier main-thread model earned nothing.
A `medium` tier exists in the routing abstraction and resolves to `large` in v1; any
tier can be overridden with `DAINTREE_{LARGE,MEDIUM,SMALL}_MODEL`.

> This prototype ships with built-in system prompts and talks to DeepSeek directly.
> In the final product these are replaced by the hosted backend.

## Build & install

**Prerequisite:** Go **1.25.8 or newer** (`go version`). Nothing else — SQLite is the
pure-Go `modernc.org/sqlite` driver, so `CGO_ENABLED=0` builds work and there is no
native toolchain or `npm`/`bun`/`node` dependency.

```bash
# Install to your Go bin ($(go env GOBIN) or $(go env GOPATH)/bin)
go install github.com/daintreehq/daintree-assistant/cmd/daintree-assistant@latest

# …or from a checkout
git clone https://github.com/daintreehq/daintree-assistant
cd daintree-assistant
make build                                          # → ./bin/daintree-assistant (trimpath + version)
# or directly:
go build -o bin/daintree-assistant ./cmd/daintree-assistant
go install ./cmd/daintree-assistant                 # installs to $(go env GOPATH)/bin
```

Then configure the API key and run:

```bash
cp .env.example .env      # set DEEPSEEK_API_KEY
./bin/daintree-assistant            # interactive cockpit
./bin/daintree-assistant doctor     # environment check (MCP / DeepSeek key / project / tier)
```

`make` targets: `build` · `install` · `test` · `test-race` · `test-pty` · `vet` · `fmt` ·
`generate` · `run` · `clean` · `db-reset` (hard-reset the SQLite state dir).

## Running it

```bash
./bin/daintree-assistant                                  # interactive Bubble Tea cockpit (TTY)
./bin/daintree-assistant --classic                        # classic line REPL (also used for non-TTY)
./bin/daintree-assistant "which worktrees are ready for review?"   # one-shot, prints, exits
./bin/daintree-assistant --json "…"                       # one-shot, JSONL event stream to stdout
./bin/daintree-assistant doctor                           # environment check
./bin/daintree-assistant host --stdio                     # embedded host: stdio NDJSON, PROTOCOL_VERSION 2
```

## The cockpit (and why it's not a full-screen takeover)

The default interactive experience is the **Daintree cockpit**, built on **Bubble Tea
v2** and rendered **inline in the terminal's NORMAL screen buffer** — never the alternate
screen, never with the mouse captured. This is the deliberate anti-pattern-avoidance
versus Claude Code / Codex: those take over the whole screen and break native scrolling.

Here the **host terminal owns the scrollback**: the mouse wheel scrolls wherever it
hovers, the scrollbar works, and selection / copy-paste are native. Completed turns and
the masthead are committed **once** into that native scrollback (via Bubble Tea's
`tea.Println` print-above-program, a strict one-in-flight commit queue) and flow up like
ordinary terminal lines. Only a small **live footer** — the in-flight turn, a status
line, and the composer — repaints. The masthead is plain text with **no pinned full-width
rule** (a committed rule would wrap and break on a narrow resize). Content is inset one
column on each side; the same single surface renders at every width.

```
◆ DAINTREE  assistant-main           OPERATOR  ● CONNECTED

YOU
▏ Fix the watcher tests and tell me when the branch is clean.

◆ DAINTREE
I'll delegate the edit and supervise the result.
├─ ✓ Delegated   term_8 · repair watcher tests              38ms
╰─ ⠋ Watching    tests running · 42 passed                  18s
──────────────────────────────────────────────────────────
⠋ Integrating results · 0.3s              agents 1 · tmr 1
› Ask Daintree…
  / commands · ^O ops · ^X detail
```

The active turn is driven by an **explicit run phase** (Received → Analyzing →
Generating → Tool queued/running → Integrating → Complete), not guessed from emptiness,
so the liveness cue never vanishes mid-work. Operational detail is a purposeful **view**,
not a text dump: `^O` toggles the operations deck (NOW → NEEDS ATTENTION → AGENTS →
SCHEDULED → RECENT) and `/watchers`, `/inbox`, `/timers`, `/audit` open it focused on one
section. `^X` toggles raw tool args/results; `Esc` returns home; `^C` shuts down the
scheduler, MCP, and DB cleanly. These render *in place of the composer*, never as pinned
panels (a pinned panel is mutually exclusive with native scrollback). Risky actions raise
a full-width **approval sheet** above the composer that defaults visually to decline and
stays readable with color stripped. `DAINTREE_THEME=dark|light|ansi|none` themes it.

See [`docs/BUBBLE_TEA.md`](docs/BUBBLE_TEA.md) for the full cockpit architecture.

## Headless modes

- **One-shot** — pass a prompt argument; it runs a single turn, prints the result to the
  console, and exits.
- **`--json`** — one-shot that streams structured **JSONL** events to stdout (one event
  per line: tokens, tool calls, results, the final envelope). For scripting/automation.
- **`--classic`** — a plain line REPL (no cockpit). Also the automatic fallback on a
  non-TTY stdout.
- **`host --stdio`** — the embedded host: a stdio **NDJSON** request/response transport
  (`PROTOCOL_VERSION 2`) that Daintree drives the runtime through. Requires a piped
  command stream on stdin (it refuses a terminal).

## How it connects to Daintree

Daintree launches the CLI and injects the MCP connection via environment:

```
DAINTREE_MCP_URL=http://127.0.0.1:45454/mcp
DAINTREE_MCP_TOKEN=<bearer>
DAINTREE_PROJECT_ID=<id>
```

The CLI connects over **Streamable HTTP** (falling back to legacy SSE) with the bearer
token, using `github.com/modelcontextprotocol/go-sdk`. Without these it runs in
**degraded local mode**: filesystem, timer, watcher, and queue tools work; Daintree
orchestration tools report a clean "not connected" error. Pass them explicitly with
`--mcp-url` / `--mcp-token`.

## Architecture

```
User ↔ Bubble Tea cockpit ↔ event pump ↔ agent.Session (large model)
       (events/confirm)  │  tools (function calling)
                         ▼
            tools.Registry ── safety policy (tiers, confirm, NO file edits) ── audit
                  │
   ┌──────────────┼─────────────────────────────┐
   fsx (read-only) Daintree MCP (raw + wrappers)  CLI tools (timer/watcher/queue)
                  │
            daemon.Scheduler ── timers + terminal watchers (small model)
                  │
            queue.Queue / inbox ──► main thread (digest only, never raw logs)
```

- **Three model tiers** (`small` / `medium` / `large`); v1 routes medium → large.
- **Durable state** in SQLite (`modernc.org/sqlite`, pure Go, no CGO) under
  `~/.daintree/assistant-cli/state.db` — timers, watchers, events, audit, conversation,
  grants, memory. Survives restarts; timers do sleep catch-up. Single clean schema
  baseline (pre-release: hard-reset on schema changes).
- **Terminal watchers** are small state machines: deterministic signals first, then the
  small model, then dedupe + publish only meaningful changes.
- **Permission tiers**: `supervisor` (read-only), `operator` (+spawn/create), `system`
  (+git/destructive). Mutating actions confirm; file edits are forbidden and delegated to
  a spawned agent (`agentTask.spawnForEdits`).

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md),
[`docs/BUBBLE_TEA.md`](docs/BUBBLE_TEA.md), [`docs/DEEPSEEK.md`](docs/DEEPSEEK.md),
[`docs/DAINTREE_MCP.md`](docs/DAINTREE_MCP.md), [`docs/TOOLS.md`](docs/TOOLS.md)
(adding a tool), [`docs/SKILLS.md`](docs/SKILLS.md) (authoring skills), and
[`docs/RUNTIME.md`](docs/RUNTIME.md) (auto-compaction + model error behavior).

## Commands (cockpit or classic REPL)

```
/status  /inbox [sev]  /tools [query]  /timers  /watchers  /grants
/workflows [status]  /launches  /audit [n]  /explain [runId]  /models
/permissions [tier]  /approvals [clear]  /skills [loaded|find <query>|load <id…>|clear]
/memory [list|pin <id>|unpin <id>|forget <id>]  /compact  /clear  /doctor
/reconnect  /help  /quit
```

In the cockpit these render as command cards (and may focus a deck view); in `--classic`
mode they print to the console.

## Skill system

Behavior is steered by **skills** — short procedural runbooks injected into the main
model's context only when relevant, instead of fine-tuning. The base system prompt is
split into three stable control messages to preserve DeepSeek prompt caching:

1. **base** (`prompts.BaseSystemPrompt`) — the cached prefix, almost never changes
2. **runtime context** — tier, project, MCP status, model ids
3. **loaded skills** — the bodies of whatever skills are active

Skills are pulled on demand: the model calls `skill.find` with a short query and the
small model selects 0–3 skills from a metadata-only view of the library, validated and
injected into the loaded-skills message. The model can also pull a known skill directly
with `skill.load <id>`. Drive it manually with `/skills`. Skill bodies are **embedded**
into the binary via `go:embed` from `internal/skills/files/*.md`. See
[`docs/SKILLS.md`](docs/SKILLS.md).

## Tools the model can call

A current snapshot; the registry (`internal/tools`) is the source of truth, and
[`docs/TOOLS.md`](docs/TOOLS.md) is the contributor reference for adding one.

| Group             | Tools                                                                       |
| ----------------- | --------------------------------------------------------------------------- |
| Project read      | `fs.list` `fs.read` `fs.search`                                             |
| Daintree (raw)    | `daintree.status` `daintree.listTools` `tool.search` `daintree.call`       |
| Terminal control  | `terminal.focus` `terminal.sendCommand` `terminal.arm` `terminal.disarm` `terminal.disarmAll` |
| Focus (UI)        | `agent.focusNextWaiting` `agent.focusNextWorking` `agent.focusNextAgent` `agent.focusPreviousAgent` `workflow.focusNextAttention` |
| Context           | `context.snapshot` `terminal.read` `terminal.summarize`                    |
| Extraction        | `terminal.extract` `terminal.extract.async`                               |
| Timers            | `timer.schedule` `timer.list` `timer.cancel`                               |
| Watchers          | `watcher.terminal.create` `watcher.watchPR` `watcher.list` `watcher.cancel` |
| Queue             | `queue.publish` `queue.digest` `queue.resolve`                             |
| Workflows         | `workflow.create` `workflow.get` `workflow.list` `workflow.update` `workflow.startWorkOnIssue` `workflow.prepBranchForReview` |
| Recipes/worktrees | `recipe.list` `recipe.run` `worktree.list` `worktree.getCurrent` `worktree.createWithRecipe` |
| Git snapshots     | `git.snapshotRevert` `git.snapshotDelete`                                  |
| Context export    | `copyTree.generate` `copyTree.generateAndCopyFile` `copyTree.injectToTerminal` |
| Forge             | `forge.listIssues` `forge.getIssue` `forge.listPRs` `forge.getPR`         |
| Agent tasks       | `agentTask.spawnForEdits` (no-file-edit escape hatch) `agentTask.superviseTerminal` `agentTask.status` `agentTask.list` |
| Grants            | `grant.create` `grant.list` `grant.revoke`                                 |
| Skill runs        | `skill.find` `skill.load` `skill.run.get` `skill.step.advance`            |
| Audit             | `audit.export`                                                             |
| Memory            | `memory.recall` `memory.list` `memory.save` `memory.forget` `memory.pin` `memory.unpin` |
| Artifacts         | `artifact.read`                                                            |

## Environment variables

`DEEPSEEK_API_KEY` (required) · `DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` /
`DAINTREE_PROJECT_ID` / `DAINTREE_WINDOW_ID` (injected by Daintree) ·
`DAINTREE_ASSISTANT_TIER` (default `system`) · `DAINTREE_ASSISTANT_AUTO_APPROVE` ·
`DAINTREE_ASSISTANT_OFFLINE` · `DAINTREE_ASSISTANT_STATE_DIR` ·
`DAINTREE_ASSISTANT_DEBUG_LOG` / `DAINTREE_ASSISTANT_LOG_DIR` ·
`DAINTREE_{LARGE,MEDIUM,SMALL}_MODEL` · `DEEPSEEK_BASE_URL`.

Resolution order: CLI overrides → real process env → project `.env` → the assistant's own
`.env` → built-in defaults. State lives under `~/.daintree/assistant-cli/`.

## Debug logging

A full-fidelity trace for debugging the assistant itself. When enabled it appends
**everything** — every model request and response, every tool/function call with its
arguments and result, and the whole watcher lifecycle — to a single human-readable log.

**Enable it** with `DAINTREE_ASSISTANT_DEBUG_LOG=1` (read from the process env, the bound
project's `.env`, or the assistant's own `.env`). It writes to a **global** dir (default
`~/.daintree/logs`, override `DAINTREE_ASSISTANT_LOG_DIR`); each run gets its own
`<YYYY-MM-DD>-<sessionId>.log`, and logs older than 7 days are pruned at boot.

```bash
ls -t ~/.daintree/logs | head
tail -f ~/.daintree/logs/2026-06-21-ses_ab12cd34.log
```

## Testing

```bash
go test ./...        # all tests (980+ across 44 packages), no network — fakes for MCP + models
go test -race ./...
go vet ./...
gofmt -l .           # must print nothing
```

## Notes / roadmap

- The daemon runs **in-process** and **foreground-only** in this prototype. State lives
  in SQLite so it's ready to split into a detachable background process later (the
  scheduler decision record is in `docs/ARCHITECTURE.md`).
- **UI boundary:** the runtime (App, Session, Registry, Scheduler, Store, Queue, MCP,
  Router) emits structured events and exposes state; only `internal/ui` imports Bubble
  Tea. Tools never render, the watcher engine never paints, and the model loop never
  writes to stdout — it emits through an `agent.EventSink` consumed by the cockpit's
  event pump or the console / JSONL sink.
- Workflows, skills, and persistent memory are implemented tool surfaces (`workflow.*`,
  `skill.*`, `memory.*`). Future phases target Daintree-owned watch-sets over MCP, which
  would let supervision tick without the assistant open.
