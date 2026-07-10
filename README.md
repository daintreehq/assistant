# Daintree Assistant

A single native **Go** binary: a local command-line **orchestration assistant for
Daintree** — Daintree's "local operations officer." It understands the current
workspace, plans Daintree operations, spawns and supervises agent terminals, watches
them with cheap models, schedules timers, and keeps the human's main conversation clean.

It is **not** a code editor. It never edits project files. When a change is needed it
spawns a *visible* agent terminal inside Daintree and supervises it.

Powered by the **Daintree Assistant backend** — a Daintree-native HTTP API (not
OpenAI-compatible). The CLI is a thin local runtime: it sends only the visible
conversation, structured runtime/turn context, and its tool inventory; the backend owns
the system prompt, developer instructions, **skill/runbook selection**, model choice,
prompt assembly, and the upstream model credentials (DeepSeek, spoken internally). The
CLI executes the local tool calls the backend asks for and streams the reply. See
[`docs/BACKEND.md`](docs/BACKEND.md).

> **Development:** the backend endpoint is hardcoded to `http://127.0.0.1:8473` and runs
> unauthenticated. Run it locally from `../assistant-backend`
> (`python -m daintree_assistant_server`). The assistant supports exactly this one
> endpoint for now; a later phase swaps in the production URL and a real login flow.

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

Start the backend (`../assistant-backend`, on `127.0.0.1:8473`), then run:

```bash
# The CLI needs no model key — the backend owns the model credentials. Just run it:
./bin/daintree-assistant            # interactive cockpit (backend must be reachable)
./bin/daintree-assistant doctor     # environment check (backend / MCP / project / tier)
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
./bin/daintree-assistant daemon                           # run the persistent project supervisor
./bin/daintree-assistant daemon stop                      # stop the project supervisor
./bin/daintree-assistant status                           # supervisor health and live work
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
[`docs/DAINTREE_MCP.md`](docs/DAINTREE_MCP.md) (Daintree's MCP protocol),
[`docs/DAINTREE_HOST.md`](docs/DAINTREE_HOST.md) (how Daintree embeds this CLI),
[`docs/TOOLS.md`](docs/TOOLS.md)
(adding a tool), [`docs/SKILLS.md`](docs/SKILLS.md) (server-owned skills), and
[`docs/RUNTIME.md`](docs/RUNTIME.md) (auto-compaction + model error behavior).

## Commands (cockpit or classic REPL)

```
/status  /inbox [sev]  /tools [query]  /timers  /watchers  /grants
/workflows [status]  /workflow [id|sub]  /launches  /audit [n]  /explain [runId]  /models
/permissions [tier]  /approvals [clear]  /skills
/memory [list|pin <id>|unpin <id>|forget <id>]  /compact  /clear  /doctor
/reconnect  /help  /quit
```

In the cockpit these render as command cards (and may focus a deck view); in `--classic`
mode they print to the console.

## Skill system

Behavior is steered by **skills** — short procedural runbooks — but selection and
injection are **server-owned**. The Daintree backend's selector classifies the
conversation, picks the relevant runbook(s) for the turn, and injects their bodies into
the prompt before generation. It returns a `skills` block + an opaque signed `state`
token in the first SSE `meta` event, flushed before the upstream model connects. The CLI
immediately surfaces de-duplicated `newly_loaded` refs as skill cards and stores+replays
the opaque state token (including on a full-request retry, so the already-visible
selection is reused). This is the entire client-side "keep skills loaded" mechanism —
the backend is stateless and recovers the active set from the token, not from the message
history. The CLI also keeps two local run-tracking tools — `skill.run.get` and
`skill.step.advance`. There is **no** local skill catalog and no `skill.find`/`skill.load`.
Skills never narrow the toolset. Authoring lives in `../assistant-backend`. See
[`docs/SKILLS.md`](docs/SKILLS.md) and [`docs/BACKEND.md`](docs/BACKEND.md).

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
| Git               | `git.getProjectPulse`                                                       |
| Context export    | `copyTree.generate` `copyTree.generateAndCopyFile` `copyTree.injectToTerminal` |
| Forge             | `forge.listIssues` `forge.getIssue` `forge.listPRs` `forge.getPR`         |
| Agent tasks       | `agentTask.spawnForEdits` (no-file-edit escape hatch) `agentTask.superviseTerminal` `agentTask.status` `agentTask.list` |
| Grants            | `grant.create` `grant.list` `grant.revoke`                                 |
| Skill runs        | `skill.run.get` `skill.step.advance` (selection is server-owned)          |
| Audit             | `audit.export`                                                             |
| Memory            | `memory.recall` `memory.list` `memory.save` `memory.forget` `memory.pin` `memory.unpin` |
| Artifacts         | `artifact.read`                                                            |

## Environment variables

The CLI needs no model-provider API key or model-routing variables; the Daintree Assistant
backend owns both. Runtime settings are `DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` /
`DAINTREE_PROJECT_ID` / `DAINTREE_WINDOW_ID` (injected by Daintree) ·
`DAINTREE_ASSISTANT_TIER` (default `system`) · `DAINTREE_ASSISTANT_AUTO_APPROVE` ·
`DAINTREE_ASSISTANT_OFFLINE` · `DAINTREE_ASSISTANT_STATE_DIR` ·
`DAINTREE_ASSISTANT_DEBUG_LOG` / `DAINTREE_ASSISTANT_LOG_DIR`.

Resolution is setting-specific at the trust boundary: CLI overrides win; MCP credentials,
Daintree identity, and debug logging never come from a project's untrusted `.env`; tier,
offline mode, state dir, and log dir come only from CLI/real process environment. State
lives under `~/.daintree/assistant-cli/`.

## Debug logging

A full-fidelity trace for debugging the assistant itself. When enabled it appends
**everything** — every model request and response, every tool/function call with its
arguments and result, and the whole watcher lifecycle — to a single human-readable log.

**Enable it** with `DAINTREE_ASSISTANT_DEBUG_LOG=1` (read from the process environment or
the assistant's own `.env`, never the bound project's `.env`). It writes to a **global**
dir (default `~/.daintree/logs`, override `DAINTREE_ASSISTANT_LOG_DIR`); each run gets its own
`<YYYY-MM-DD>-<sessionId>.log`, and logs older than 7 days are pruned at boot.

```bash
ls -t ~/.daintree/logs | head
tail -f ~/.daintree/logs/2026-06-21-ses_ab12cd34.log
```

## Testing

```bash
go test ./...        # all tests (2200+ across 58 packages), no network — fakes for MCP + backend
go test -race ./...
go vet ./...
gofmt -l .           # must print nothing
```

## Notes / roadmap

- The persistent per-project supervisor is started automatically by an interactive launch.
  It keeps watchers, async operations, timers, and wake turns running after the UI exits,
  then yields ownership when an assistant attaches. Use `status` and `daemon stop` to
  inspect or stop it; see [`docs/SUPERVISOR.md`](docs/SUPERVISOR.md).
- **UI boundary:** the runtime (App, Session, Registry, Scheduler, Store, Queue, MCP,
  Router) emits structured events and exposes state; only `internal/ui` imports Bubble
  Tea. Tools never render, the watcher engine never paints, and the model loop never
  writes to stdout — it emits through an `agent.EventSink` consumed by the cockpit's
  event pump or the console / JSONL sink.
- Workflows, skills, and persistent memory are implemented tool surfaces (`workflow.*`,
  `skill.*`, `memory.*`). Future phases target Daintree-owned watch-sets over MCP, which
  would let supervision tick without the assistant open.
