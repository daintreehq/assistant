# Headless operation

How to drive the assistant from a script, a test harness, or another agent — without
a terminal, without the cockpit, and without rewriting the process environment to say
what argv says perfectly well.

There are four headless surfaces. Pick by how many turns you need, and by whether the
caller is a script or another agent.

| Surface | Turns | Output | Use it when |
|---|---|---|---|
| `mcp --stdio` | many | MCP tools | **another agent drives the assistant as a sub-agent** |
| `--json <prompt>` | one | JSONL on stdout | scripting, CI gates, one-shot queries |
| `--classic` + piped stdin | many | human-rendered | a multi-turn exchange in one process |
| `host --stdio` | many | NDJSON, protocol v2 | you are Daintree, or reimplementing it |

If the caller is itself an agent — Claude Code, most immediately — reach for
`mcp --stdio` first. `host --stdio` is Daintree's own contract — the first stdin line must be a valid
`SessionDescriptor` and the surrounding app owns the lifecycle. It is the wrong shape
for a test harness. See `docs/DAINTREE_HOST.md`.


## `mcp --stdio` — the assistant as an MCP server

```json
{ "mcpServers": { "daintree": {
    "command": "/path/to/bin/daintree-assistant",
    "args": ["mcp", "--stdio"],
    "env": { "DAINTREE_API_KEY": "sk-or-v1-…" } } } }
```

Two decisions shape this surface, both forced by what an MCP client is.

**It is async-first, because a turn takes minutes.** `daintree.ask` returns a run handle
*immediately* and `daintree.poll` reads it incrementally. A synchronous ask would be
unusable for exactly the work this assistant exists to do — spawning a cohort of agents
and supervising them. It is the same shape the assistant already uses internally for its
own long work (`terminal.run.async` returns a handle a coordinator settles later).

**The server holds no configuration.** A client launches this process once and keeps the
pipe for its whole session; it cannot restart it when you want a different project or a
locally-rebuilt backend. So project, endpoint, tier, MCP credentials and state dir are
all arguments to `daintree.session.open`, and the process env supplies defaults only.
Repointing is a close/open pair, never a reconnect.

### The tools

| Tool | What it does |
|---|---|
| `daintree.session.open` | bind a session to a project; returns `sessionId` + warnings |
| `daintree.session.list` | open sessions, and whether the binary went stale |
| `daintree.session.close` | cancel any turn, tear down, **release the project lease** |
| `daintree.ask` | start a turn; returns `runId` immediately |
| `daintree.poll` | read a run: status, event window, answer, stats |
| `daintree.inject` | fold a message into the **running** turn |
| `daintree.interrupt` | cancel the running turn, keep the session |
| `daintree.attention` | read what settled in the background |
| `daintree.approvals` | list confirmations the session is **parked** on |
| `daintree.approve` | answer one, releasing or refusing the blocked call |

Every tool has a generated input *and* output schema, so a caller discovers the exact
argument shape rather than guessing it.

Things worth knowing before you drive it:

- **One turn at a time per session.** A second `ask` is rejected — the assistant's turn
  loop is single-flight and a concurrent turn would corrupt the conversation. To steer
  work already running, use `inject`; to abandon it, `interrupt`.
- **`poll` returns a window.** Pass the previous response's `nextSeq` as `sinceSeq` to
  read only what is new. When the window truncates, `withheldEvents` says by how much —
  it never silently hands you a partial timeline as a complete one.
- **Background work reports through `attention`, never as a late run event.** A tool that
  accepted async work shows up as `pendingAsync` on the run; the completion arrives in
  the inbox, carrying `asyncId` so you can match the two. `attention` acknowledges by
  default so a polling caller sees each item once; pass `acknowledge:false` to peek.
- **Always close what you open.** A session holds the project's owner lease for its whole
  life, and a leaked one blocks every other process from opening that project.
- **Mutating tools need approval,** and the mode is per session:
  - `decline` (default) — refuse immediately and carry on. Safe for an unattended
    caller, but the session can never actually change anything.
  - `ask` — park the call and surface it with its risk, consequence and redacted args.
    A parked call **blocks the whole turn**, so only choose this if you will poll. It
    fails closed on a timer (`approvalTimeoutMs`, default 5 minutes), and interrupt or
    close releases everything outstanding. Cancellation always wins: a call approved
    after you interrupted the turn does not run.
  - `auto` — never ask. Equivalent to `--auto-approve`.

  A blocked run is reported as blocked: pending approvals ride the run's `poll` response
  and its `nextAction` says so, because "still running" would send you polling harder at
  something that will never move on its own.
- Unlike a one-shot, an MCP session **does** run the scheduler, so watchers, timers and
  async futures actually settle while it is open.

### Diagnosing a run

Two resources exist for when the poll digest is not enough. They are resources rather
than tool results so their cost is paid once, when diagnosing, instead of on every poll:

| URI | What it is |
|---|---|
| `daintree://session/{sessionId}/run/{runId}` | the **complete** event timeline `poll` truncates |
| `daintree://session/{sessionId}/log` | the tail of the structured debug trace |

The log is **per process, not per session** — `debuglog` keeps one active file, so a
per-session log would silently redirect earlier sessions' writes into the newest
session's file. Every session in this server therefore reports the same path; grep it by
`sessionId` to separate them.

Read the transcript when a poll reported a non-zero `withheldEvents`. Read the log when
you need what actually happened rather than what the answer claims — it is bounded to
the last 256 KB (the tail, because that is where a failure is), passed through the
redactor, and only exists if the session was opened with `debugLog:true`.

### When the binary changes underneath it

This is the one thing a session argument cannot fix. `make build` replaces the
executable, but the client is still holding a pipe to the old image, and MCP has no way
for a server to say "I have been replaced".

So the server reports it instead. Both `session.open` and `session.list` carry a
structured `server` block with `staleBinary` and the on-disk build time, and `open` adds
a warning naming the remedy — reconnect the MCP server. That turns "why is my fix not taking effect" from a mystery
into a stated fact. Everything *else* you might want to change needs no reconnect at
all, which is the whole reason configuration lives on the session.

## `--json` — the scriptable path

```bash
daintree-assistant --json "which worktrees are ready?"
```

**stdout carries ONLY JSONL.** Every human line — warnings, the debug-log path, tool
skip notices — goes to stderr. That separation is the whole contract: a caller can
capture stdout and parse it without filtering.

Exit codes (`domain.OneShotExitCode`): `0` success, `1` error, `2` cancelled. `3` is
reserved and never emitted.

`--json` requires a prompt — there is no JSONL interactive mode. A prompt that begins
with a dash needs `--` first (`daintree-assistant --json -- "--summarize this"`), or the
parser reads it as an option.

### Configuration

Every knob is a flag, and every flag shadows a trusted env var and wins over it.

| Flag | Env | Notes |
|---|---|---|
| `--backend-url URL` | `DAINTREE_BACKEND_URL` | overriding this keeps the stored key |
| `--api-key-file PATH` | `DAINTREE_API_KEY` | there is deliberately **no `--api-key`** |
| `--state-dir PATH` | `DAINTREE_ASSISTANT_STATE_DIR` | also relocates `credentials.json` |
| `--log-dir PATH` | `DAINTREE_ASSISTANT_LOG_DIR` | |
| `--debug-log` | `DAINTREE_ASSISTANT_DEBUG_LOG=1` | writes the session trace |
| `--auto-approve` | `DAINTREE_ASSISTANT_AUTO_APPROVE=1` | see the warning below |
| `--tier TIER` | `DAINTREE_ASSISTANT_TIER` | `supervisor`\|`operator`\|`system` |
| `--mcp-url` / `--mcp-token` | `DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` | injected by Daintree |
| `--project PATH` | — | default: the current directory |
| `--timeout DURATION` | — | one-shot only; `0` means no limit |

`--timeout` is silently ignored on every other route (interactive, `login`, `daemon`,
`doctor`). Only `RunOneShot` consults it.

Three rules worth knowing before you script against them:

- **The key never rides argv.** `ps` is world-readable, so `--api-key-file` takes a
  path. The file is read with a bounded read (a FIFO would otherwise defeat
  `--timeout`) and checked with the same shape rule the login prompt applies, so a
  stray newline fails locally instead of as an opaque 401.
- **An explicitly empty value is an error, not a fallback.** `--api-key-file=` — what a
  harness produces when it expands an unset variable — fails at the argument boundary
  rather than quietly running against a different key.
- **An explicitly false boolean wins.** `--auto-approve=false` beats
  `DAINTREE_ASSISTANT_AUTO_APPROVE=1`. An *absent* flag leaves the env in charge.

### Isolation

A harness should never touch the developer's real state. `--state-dir` relocates the
database *and* the sign-in, which means it also hides any stored credentials — so an
isolated run must supply its own key:

```bash
daintree-assistant \
  --state-dir /tmp/harness/state \
  --log-dir /tmp/harness/logs --debug-log \
  --backend-url http://127.0.0.1:8473 \
  --api-key-file /run/secrets/openrouter \
  --timeout 10m \
  --json "your prompt"
```

Exactly one process may own a project's `state.db` at a time. A one-shot run takes the
lease briefly and never spawns a supervisor daemon, so it cannot litter the machine —
but it will fail rather than double-open if a cockpit already owns that project. A
distinct `--state-dir` is the way to run alongside one.

`--auto-approve` makes tier-allowed mutating tools run with nothing on screen to say
so. Without it, one-shot auto-**declines** every confirmation, so mutating work is
skipped and reported on stderr. Runs that use it emit a `warning` line and set
`session.autoApprove`.

### The event stream

Every line is `{"type": …, "ts": <epoch-ms>, "seq": <int>, …}` with `seq` monotonic
from 0. Types:

| Type | Payload | Meaning |
|---|---|---|
| `session` | see below | one-time header; first line whenever it appears |
| `assistant:start` | — | a round is about to stream |
| `assistant:content` | `content` | buffered prose, flushed before a tool call |
| `assistant:end` | `content` | final answer (authoritative) |
| `assistant:cancelled` | `content` | aborted; the streamed buffer is dropped |
| `user:interjection` | `text` | a prompt folded into the running turn |
| `skill:loaded` | `titles[]` | the backend's selector loaded runbooks |
| `tool:call` | `id`, `name`, `args` | a call is starting |
| `tool:result` | `id`, `name`, `ok`, `summary`, `error`, `auditId?`, `async?` | settled |
| `info` / `warning` | `message` | non-fatal; **neither changes the exit code** |
| `error` | `message` | fatal for this turn |
| `result` | see below | terminal envelope; always last |

**Do not treat a `warning` as failure, and do not decide the outcome from an `error`
line.** Gate on the terminal `result` envelope — it is the only authority.

`session` answers "where do I look when this goes wrong". It is emitted once the
runtime exists, so it precedes every assistant and tool event — but a failure *before*
that point (bad flags, signed out, the project lease held by a live cockpit) produces
only `error` + `result` and **no session line at all**. Handle its absence.

```json
{"type":"session","ts":…,"seq":0,
 "sessionId":"ses_4f6eecc5","project":"/repo","tier":"system",
 "backendUrl":"http://127.0.0.1:8473","logPath":"/logs/2026-08-21-ses_4f6eecc5.log",
 "version":"1.2.0","autoApprove":false,
 "mcpConnected":true,"mcpTransport":"streamable-http"}
```

`mcpConnected: false` means the run started in **degraded local mode**, where every
orchestration tool reports "not connected". That is invisible in the answer text and is
the commonest cause of a confusing run — check it before diagnosing anything else. It is
sampled once, right after connect: MCP can still degrade mid-run, so read it as a
starting condition rather than a whole-run guarantee. Daintree's MCP tokens also expire
roughly 12 minutes after minting, so a long harness run can begin connected and end
otherwise.

`logPath` is present-but-empty when debug logging is off. `backendUrl` is stripped of
userinfo and query string before it is emitted (`mcp.SanitizeURL`, which fails closed to
`""` on anything it cannot parse) — the header names the endpoint and carries no
credential. `project` and `logPath` are ordinary filesystem paths and are not sanitized.

`result` answers "did it work, and what did it cost":

```json
{"type":"result","ts":…,"seq":42,"schemaVersion":1,
 "status":"success","exitCode":0,"content":"…","error":null,
 "stats":{"durationMs":48210,"rounds":4,"toolCalls":11,"toolErrors":1,
          "promptTokens":92100,"completionTokens":1840,"totalTokens":93940,
          "contextTokens":31200}}
```

`rounds` counts rounds *started* — one per `assistant:start` — not tool calls, so a run
cancelled before its first backend request still reports 1. `toolErrors` is worth gating
on separately: a failed tool is recoverable model context and never changes the exit
code, but "the answer arrived after six tool failures" is usually not the answer you
wanted.

Read the token counts as a **lower bound on spend, not a bill.** They are summed from
the usage the backend reports on a *successful* response, so an attempt that was billed
and then failed into a retry contributes nothing, and the separate model calls behind
auto-compaction and the utility tasks are not counted at all. `promptTokens` is input
*volume* — every round re-sends the conversation — while `contextTokens` is the last
round's prompt size, which is the figure that actually drives compaction.

### Background work does not run in a one-shot

A one-shot never starts the scheduler and never spawns a supervisor daemon. So if the
turn kicks off asynchronous work — `terminal.run.async`, a watcher, a timer — the tool
returns its `asy_…` handle, the row is written durably, and then **nothing polls it**.
That work is adopted by the next process to take the project lease (a cockpit, or
`daintree-assistant daemon`), not by the run that started it.

For a harness this means a one-shot is the wrong shape for "spawn agents and wait". Use
an in-turn wait (`terminal.awaitAll`) so the work completes inside the turn, give the
run a `--timeout` generous enough to cover it, or drive a longer-lived session.

## Multi-turn

`--json` is one turn per process, and each process mints a fresh session. Project
state — watchers, the attention inbox, async futures — persists in the state dir and is
adopted by the next owner, but the *conversation* does not carry over.

For a real multi-turn exchange, pipe stdin to the classic REPL, which reads one prompt
per line in a single process:

```bash
printf '%s\n' "list the worktrees" "now check the first one" \
  | daintree-assistant --classic --state-dir /tmp/harness/state
```

Slash commands work on that stdin too — `/clear`, `/compact` and the rest of the
catalog are handled before the line reaches the model — so a harness can reset state
between prompts. Output is human-rendered, not JSONL: you cannot have both today.

## Diagnosing a run

`--debug-log` writes a structured trace to `<log-dir>/<date>-<sessionId>.log`, and the
`session` line tells you exactly which file. It is the ground truth for what the model
and tools actually did; grep it by `runId`/`turnId`/`round`. See `docs/LOGGING.md` for
the event reference and the fix philosophy (a model mistake is usually a *documentation*
bug in the prompt, skill, or tool schema — not a dumb model).
