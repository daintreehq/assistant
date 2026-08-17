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
the system prompt, developer instructions, **skill/runbook selection**, model choice, and
prompt assembly. The CLI executes the local tool calls the backend asks for and streams
the reply. See [`docs/BACKEND.md`](docs/BACKEND.md).

**Every model call takes exactly one path:**

```
you → Daintree Assistant CLI → Daintree Assistant backend → OpenRouter → the selected model
                              (your bearer key)            (the same key, request-scoped)
```

The CLI has **no provider client and no provider credential of its own**. DeepSeek,
GPT, and anything else are *models the backend routes to through OpenRouter* — never a
direct transport from here. There is no `DEEPSEEK_API_KEY` in this process.

> **Sign-in:** the assistant talks to `https://assistant.daintree.org` by default and
> **authenticates every request** — there is no unauthenticated mode. Run
> `daintree-assistant login` to pick an endpoint (official, custom, or a local backend
> from `../assistant-backend`) and store your API key. During the internal-tester phase
> that key is **your own OpenRouter key**, and it is what funds every model call your
> turns make — including background watcher and async supervision work. Use a dedicated
> low-limit key, not your main one.

## Supported platforms

| Platform | Interactive cockpit | One-shot / `--json` | Persistent supervisor (daemon) |
| --- | --- | --- | --- |
| macOS (arm64, amd64) | supported | supported | supported |
| Linux (amd64, arm64) | supported | supported | supported |
| Windows | **unsupported** | **unsupported** | **unsupported** |

Windows is not "untested" — it does not run. Exactly one process at a time may own a
project's `state.db`, and that lease is an `flock` (`internal/ipc/lock_unix.go`), which has
no Windows port; the `!unix` build returns an error rather than silently running without
exclusion. Every stateful mode takes the lease before doing anything, so all of them fail
at that step. The daemon additionally needs `Setsid`. CI builds and tests macOS and Linux
(the PTY harness runs on macOS, the race detector on Linux); Windows is neither built nor
shipped. See [`docs/SUPERVISOR.md`](docs/SUPERVISOR.md).

## Build & install

**Prerequisite:** Go **1.25.8 or newer** (`go version`). Nothing else — SQLite is the
pure-Go `modernc.org/sqlite` driver, so `CGO_ENABLED=0` builds work and there is no
native toolchain or `npm`/`bun`/`node` dependency.

**Testers:** download a prebuilt archive from
[Releases](https://github.com/daintreehq/assistant/releases) — macOS (arm64/amd64) and
Linux (amd64/arm64), with `SHA256SUMS` to verify against. No Go required. The builds are
not yet code-signed, so macOS will warn about an unidentified developer; `make install`
from source avoids that.

**Contributors:**

```bash
# Install to your Go bin ($(go env GOBIN) or $(go env GOPATH)/bin)
go install github.com/daintreehq/assistant/cmd/daintree-assistant@latest

# …or from a checkout
git clone https://github.com/daintreehq/assistant
cd assistant
make build                                          # → ./bin/daintree-assistant (trimpath + version)
# or directly:
go build -o bin/daintree-assistant ./cmd/daintree-assistant
go install ./cmd/daintree-assistant                 # installs to $(go env GOPATH)/bin
```

Sign in once, then run it:

```bash
./bin/daintree-assistant login      # choose an endpoint, paste your API key (stored 0600)
./bin/daintree-assistant            # interactive cockpit
./bin/daintree-assistant doctor     # environment check — start here when something is wrong
./bin/daintree-assistant logout     # forget the stored endpoint and key
```

**`doctor` is the gate.** It diagnoses the install (which binary is on PATH, whether this
platform supports background supervision, whether the state dir is writable and private),
the sign-in (stored, structurally valid, and *actually accepted by the provider* — with
"no credit left" as its own distinct verdict), the backend (reachable, protocol, and every
task id this build will send), and both MCP connections. It exits non-zero **only** when
something genuinely failed, so it works as a CI or installer gate:

```bash
daintree-assistant doctor --json | jq '.checks[] | select(.status=="fail")'

# As a gate, note the pipe: jq's exit status is what the shell sees, so you need
# BOTH `set -o pipefail` (to see doctor's own non-zero) and `jq -e` (to fail on a
# false result). Without them the pipeline reports success no matter what.
set -o pipefail
daintree-assistant doctor --json | jq -e '.summary.healthy'
```

Every check has a stable id, a status, and one next action. When something is wrong and
you need a maintainer:

```bash
./bin/daintree-assistant support-bundle          # a redacted diagnostics archive
```

It collects versions, protocol/schema numbers, the environment settings that change
behaviour, the full doctor diagnosis, supervisor state, and a *listing* of your session
logs. It deliberately does **not** include your conversation, memories, terminal output,
file contents, or the logs themselves — those are what you cannot safely send. Every value
is redacted, the archive carries a scan of itself (`redaction-report.json`), and the whole
manifest is printed for you to approve **before** anything is written.

Signing in asks the provider whether the key works. A definite rejection fails; "no credit
remaining" and "could not reach the provider" warn and still save, since neither means the
key is wrong and refusing would leave you unable to configure the CLI. Inside the cockpit,
`/auth` shows the current sign-in and `/login` switches endpoint or key without a restart.

A first interactive launch runs the login flow for you if you skip it. To develop
against a backend of your own, start it (`cd ../assistant-backend &&
python -m daintree_assistant_server`) and either pick **Local** at login or export
`DAINTREE_BACKEND_URL=http://127.0.0.1:8473` — your stored key still applies.

`make` targets: `build` · `install` · `test` · `test-race` · `test-pty` · `vet` · `fmt` ·
`generate` · `run` · `clean` · `db-reset` (delegates to `reset project-state`).

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
./bin/daintree-assistant doctor --json                    # machine-readable environment check
./bin/daintree-assistant support-bundle --include-audit   # redacted diagnostics archive
./bin/daintree-assistant reset project-state              # safe local reset (keeps your sign-in)
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
**degraded local mode**, which is not a normal launch — the assistant's whole
orchestration role is offline.

What still works: filesystem reads, memory, timers, the attention queue, grants, the
audit trail, the async and workflow ledgers, and `daintree.status` and `context.snapshot`
(both report the outage as part of their answer). What does not: anything that reaches a
terminal, agent, or worktree.

**Watchers are the subtle case.** `watcher.terminal.create` will happily write a durable
row, but the engine polls through the Daintree control plane, so a watcher created while
disconnected observes nothing until the link returns. Creating one is bookkeeping, not
supervision. Every tool's dependency is listed in
[`docs/generated/TOOLS.md`](docs/generated/TOOLS.md)'s **Needs** column.

Pass the credentials explicitly with `--mcp-url` / `--mcp-token`.

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

- **No model routing here.** The backend picks the model (and the utility models behind
  summarize / extract / classify / checkpoint) and reaches all of them through
  OpenRouter. The CLI's `internal/models` package is conversation *wire vocabulary*
  only — there is no provider client, router, or pricing table in this binary.
- **Durable state** in SQLite (`modernc.org/sqlite`, pure Go, no CGO) under
  `~/.daintree/assistant-cli/state.db` — timers, watchers, events, audit, conversation,
  grants, memory. Survives restarts; timers do sleep catch-up. Single clean schema
  baseline (pre-release: hard-reset on schema changes).
- **Terminal watchers** are small state machines: deterministic signals first, then the
  small model, then dedupe + publish only meaningful changes.
- **Permission tiers**: `supervisor` (read-only), `operator` (+spawn/create), `system`
  (+git/destructive). Mutating actions confirm; file edits are forbidden and delegated to
  a spawned agent (`agentTask.spawnForEdits`).

## Internal beta

Running it as a tester rather than working on it? Start here:

| Document | Read it for |
| --- | --- |
| [`docs/beta/INTERNAL_BETA.md`](docs/beta/INTERNAL_BETA.md) | Scope, supported platforms, known limitations, what it costs you |
| [`docs/beta/FIRST_RUN.md`](docs/beta/FIRST_RUN.md) | Install → sign in → first useful result, in five minutes |
| [`docs/beta/PRIVACY_AND_DATA.md`](docs/beta/PRIVACY_AND_DATA.md) | What leaves your machine, what is stored, how to delete it |
| [`docs/beta/TROUBLESHOOTING.md`](docs/beta/TROUBLESHOOTING.md) | A decision tree keyed to `doctor`'s check ids |
| [`docs/beta/SUPPORT_BUNDLE.md`](docs/beta/SUPPORT_BUNDLE.md) | What a bundle contains, and what it deliberately omits |
| [`SECURITY.md`](SECURITY.md) | Reporting a leaked secret or an unsafe action |
| [`CHANGELOG.md`](CHANGELOG.md) | What changed |

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md),
[`docs/BACKEND.md`](docs/BACKEND.md) (the model/skill/prompt story — start here),
[`docs/BUBBLE_TEA.md`](docs/BUBBLE_TEA.md) (the cockpit contract),
[`docs/SUPERVISOR.md`](docs/SUPERVISOR.md) (the persistent daemon),
[`docs/DAINTREE_MCP.md`](docs/DAINTREE_MCP.md) (Daintree's MCP protocol),
[`docs/DAINTREE_HOST.md`](docs/DAINTREE_HOST.md) (how Daintree embeds this CLI),
[`docs/TOOLS.md`](docs/TOOLS.md)
(adding a tool), [`docs/SKILLS.md`](docs/SKILLS.md) (server-owned skills),
[`docs/LOGGING.md`](docs/LOGGING.md) (the debug-log event reference), and
[`docs/RUNTIME.md`](docs/RUNTIME.md) (auto-compaction + model error behavior).

## Commands (cockpit or classic REPL)

**→ [`docs/generated/COMMANDS.md`](docs/generated/COMMANDS.md)** — generated from
`COMMAND_REGISTRY`, the same table that drives the composer palette and `/help`.

The ones worth knowing on day one:

```
/doctor      environment check — start here when something is wrong
/auth        the active endpoint and API key (redacted)
/login       sign in again: official, custom, or a local backend — no restart
/status      backend, MCP, project, session, tier
/inbox       whatever needs your attention
/permissions supervisor | operator | system
/help        everything else
```

In the cockpit these render as command cards (and may focus a deck view); in `--classic`
mode they print to the console.

## Skill system

Behavior is steered by **skills** — short procedural runbooks — but selection and
injection are **server-owned**. The Daintree backend's selector classifies the
conversation, picks the relevant runbook(s) for the turn, and injects their bodies into
the prompt before generation. It returns a `skills` block + an opaque signed `state`
token in the first SSE `meta` event, flushed before the upstream model connects. The CLI
stores+replays the opaque state token (including on a full-request retry, so the same
selection is reused) and shows the user nothing: backend skill selection is prompt
assembly, not a decision they take, so it enters no conversation and has no `/skills`
command; the debug trace, run log and `--json` stream carry every load, and `/explain
<run>` replays them in that run's timeline. This is the entire
client-side "keep skills loaded" mechanism —
the backend is stateless and recovers the active set from the token, not from the message
history. The CLI also keeps two local run-tracking tools — `skill.run.get` and
`skill.step.advance`. There is **no** local skill catalog and no `skill.find`/`skill.load`.
Skills never narrow the toolset. Authoring lives in `../assistant-backend`. See
[`docs/SKILLS.md`](docs/SKILLS.md) and [`docs/BACKEND.md`](docs/BACKEND.md).

## Tools the model can call

**→ [`docs/generated/TOOLS.md`](docs/generated/TOOLS.md)** — every registered tool with
its risk class, minimum tier, confirmation behaviour, grantability, connection
dependency, parallel-safety class, and feature flag.

That file is **generated from the live registry** and diffed in CI, so it cannot drift
from the binary. This README deliberately no longer restates it: the hand-maintained
version had fallen to 67 tools while the registry held 86, and listed several that had
been deleted. [`docs/TOOLS.md`](docs/TOOLS.md) remains the contributor guide for *adding*
a tool; [`docs/generated/COMPATIBILITY.md`](docs/generated/COMPATIBILITY.md) pins the
protocol, schema, and backend-task versions a release negotiates on.

The shape worth knowing without reading the table:

| Group | What it covers |
| --- | --- |
| `fs.*` `artifact.*` `context.*` | read-only project and transcript access |
| `agentTask.*` | spawn and supervise **visible** agents — the only path to a code change |
| `terminal.*` | focus, rename, send, read, summarize, extract, wait, close, arm/disarm |
| `async.*` | durable background supervision that survives closing the cockpit |
| `watcher.*` `timer.*` `queue.*` `grant.*` | unattended supervision and the authority it needs |
| `workflow.*` | the durable work ledger (plus a flag-gated execution graph) |
| `worktree.*` `recipe.*` `forge.*` `git.*` `copyTree.*` | Daintree and repository operations |
| `memory.*` `scratch.*` `skill.*` `audit.*` | state that outlives, or is scoped inside, a turn |
| `daintree.*` `tool.search` | capability discovery and the raw MCP escape hatch |
| `user.askMultipleChoice` | one finite question, answered in place |

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

A trace for debugging the assistant itself. When enabled it appends every model request
and response, every tool call with its arguments and result, and the whole watcher
lifecycle to a single human-readable log.

**Secrets are redacted before anything is written** (`internal/redact`, applied at the
write boundary so no call site can opt out): credential shapes — bearer tokens, `sk-`
keys, PATs, JWTs, `export API_KEY=…`, URL userinfo, PEM blocks — plus this process's own
API key and Daintree MCP token, which are registered by exact value because neither is
guaranteed to match a shape. Oversized values are capped with a size and a content hash.

That still leaves your conversation, terminal output, file excerpts, issue bodies, and
memory contents in the file. It is an **owner-only local artifact**, not something to
paste into an issue — use `daintree-assistant support-bundle` for that.

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
go test ./...        # the whole suite, no network — fakes for MCP + backend
go test -race ./...
go vet ./...
gofmt -l .           # must print nothing
make test-pty        # real pseudoterminal render harness (not in the default suite)
```

CI runs all of the above on **macOS and Linux**, plus three gates that are easy to forget
locally: the generated capability reference is regenerated and diffed, `govulncheck` scans
the dependency graph, and the working tree is scanned for literal credentials — by
`gitleaks` and by a scan-grade subset of this project's own patterns
(`go test ./internal/redact -run TestRepositoryContainsNoCredentials`). The subset is
deliberately narrower than the runtime redactor: redaction errs toward masking, while a
scanner that fires on the repository's own documentation gets switched off.

## Notes / roadmap

- The persistent per-project supervisor is started automatically by an interactive launch.
  It keeps watchers, async operations, timers, and wake turns running after the UI exits,
  then yields ownership when an assistant attaches. Use `status` and `daemon stop` to
  inspect or stop it; see [`docs/SUPERVISOR.md`](docs/SUPERVISOR.md).
- **UI boundary:** the runtime (App, Session, Registry, Scheduler, Store, Queue, MCP,
  Backend) emits structured events and exposes state; only `internal/ui` imports Bubble
  Tea. Tools never render, the watcher engine never paints, and the model loop never
  writes to stdout — it emits through an `agent.EventSink` consumed by the cockpit's
  event pump or the console / JSONL sink.
- Workflows and persistent memory are implemented tool surfaces (`workflow.*`,
  `memory.*`); of the skill layer only run-tracking is local (`skill.run.get` /
  `skill.step.advance`) — selection and injection are the backend's.
  Future phases target Daintree-owned watch-sets over MCP, which
  would let supervision tick without the assistant open.

## License

Apache 2.0. See [LICENSE](LICENSE).
