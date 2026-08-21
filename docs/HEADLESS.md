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
- Unlike a one-shot's default, an MCP session **does** run the scheduler, so watchers,
  timers and async futures actually settle while it is open. A one-shot can opt into the
  same shape with `--run-scheduler`.
- **`skills` pins runbooks for the session**, the MCP twin of `--skill` (see below for
  the full semantics — they are identical, deliberately: the two headless surfaces must
  not drift). Omitting it inherits whatever `--skill` this server process was launched
  with; passing an explicit `[]` clears those defaults for this session. An id the
  backend cannot accept fails the **open**, not the first turn, so the failure lands
  where the caller is looking — when there is a catalog to check it against. Against a
  backend that advertises none, the open succeeds with a warning and the bad id is
  reported on the first turn instead. `facts.pinnedSkills` reports what the session REQUESTS on
  every turn — the only way a caller that inherited a server-level default can see them.
  It is not a claim the backend honoured each one: an id can be in the catalog and still
  come back `pinned_skill_not_executable` or `pinned_skill_over_cap`.

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
parser reads it as an option. The two exceptions are `doctor --json` and
`--list-skills --json`: both are reads that answer with ONE document rather than a run,
and a gate or a catalog a script cannot parse is not one.

### Configuration

Every knob is a flag, and every flag shadows a trusted env var and wins over it.

| Flag | Env | Notes |
|---|---|---|
| `--backend-url URL` | `DAINTREE_BACKEND_URL` | outranks the endpoint stored by `/backend` |
| `--api-key-file PATH` | `DAINTREE_API_KEY` | OPTIONAL — see below. Deliberately **no `--api-key`** |
| `--prompt-file PATH` | — | one-shot only; `-` reads stdin. Capped at 1 MiB |
| `--state-dir PATH` | `DAINTREE_ASSISTANT_STATE_DIR` | the database, artifacts, and the owner lease |
| `--log-dir PATH` | `DAINTREE_ASSISTANT_LOG_DIR` | |
| `--debug-log` | `DAINTREE_ASSISTANT_DEBUG_LOG=1` | writes the session trace |
| `--auto-approve` | `DAINTREE_ASSISTANT_AUTO_APPROVE=1` | see the warning below |
| `--tier TIER` | `DAINTREE_ASSISTANT_TIER` | `supervisor`\|`operator`\|`system` |
| `--mcp-url` / `--mcp-token` | `DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` | injected by Daintree |
| `--project PATH` | — | default: the current directory |
| `--project-id ID` | `DAINTREE_PROJECT_ID` | scopes the DEFAULT state root into a per-project subdirectory |
| `--window-id ID` | `DAINTREE_WINDOW_ID` | identity only; no effect on where state is stored |
| `--project-instructions-file PATH` | — | the file's CONTENT becomes `DAINTREE.md`. Capped at 16 KiB |
| `--timeout DURATION` | — | one-shot only; `0` means no limit |
| `--run-scheduler` | — | one-shot only; run the scheduler and await this run's async work. Requires a positive `--timeout` |
| `--skill ID` | — | pin a backend runbook for every turn; repeatable. See below |
| `--list-skills` | — | print the runbooks this backend can load, then exit |

`--timeout` and `--run-scheduler` are only ever *read* on the one-shot route — the
interactive routes already run a scheduler of their own, and `daemon` / `doctor` have no
use for either. Their **validation**, though, is route-independent and happens at the
argument boundary: a negative `--timeout`, or `--run-scheduler` without a positive
`--timeout`, is rejected whatever follows it. Accepting a flag someone typed on purpose
and then doing nothing with it is the worse answer. See
[Background work in a one-shot is opt-in](#background-work-in-a-one-shot-is-opt-in).
`--prompt-file` follows the same route rule: a command word is chosen before the prompt
is, so `--prompt-file - mcp --stdio` serves MCP and never reads the stream carrying the
protocol.

Two of these flags have no env counterpart because they carry a file's CONTENT rather
than a setting:

- **`--prompt-file PATH`** supplies the one-shot prompt, so a long multi-line prompt can
  live in a file next to the runbook it exercises instead of being shell-quoted — and a
  prompt beginning with a dash no longer needs `--` first. `-` reads stdin. Passing it
  together with a positional prompt is an ERROR, not a precedence rule; a prompt supplied
  this way satisfies `--json`'s requirement that a run have one. The read is bounded at
  1 MiB and rejects rather than truncates, for the same reason `--api-key-file` is
  bounded, and a named path must be a REGULAR file — a FIFO blocks in `open` before any
  bound applies, and `-` is the spelling for streaming input. One caveat with `-`: an
  input that never sends EOF cannot be preempted by `--timeout` or by Ctrl-C, because
  neither interrupts a read already in flight. Close the stream to finish the prompt.
- **`--project-instructions-file PATH`** puts that file's content where the project's own
  `DAINTREE.md` would go, so a skill can be tested against a synthetic brief without
  writing one into the repo under test. It WINS over any discovered `DAINTREE.md`
  (including the one the embedded host loads per boot); without the flag, discovery is
  unchanged. Unlike discovery, a named file that is missing, empty, or oversized is
  FATAL — falling back to the repo's own brief would run the job against a different one
  than the caller named. Like `--api-key-file` and a named `--prompt-file`, it must be a
  REGULAR file (it has no `-` spelling, so a pipe has nowhere to go). It may be a symlink
  to one, though: discovery refuses a symlink because the bound
  project is untrusted, while argv carries the same trust as the environment it shadows.
  Two limits worth knowing: on `mcp --stdio` the file is validated when a session opens
  rather than at startup (there is no per-session override for it, so every open fails
  until the path is fixed), and the content does NOT travel to the supervisor daemon —
  a detached daemon rediscovers the project's own `DAINTREE.md`. Neither affects the
  one-shot harness runs the flag exists for, since one-shot never spawns a daemon.

`--project-id` is the load-bearing half of project identity: it scopes the default state
root into a per-project subdirectory, which is how a harness gets isolation without
hand-rolling state directories. An explicit `--state-dir` (or
`DAINTREE_ASSISTANT_STATE_DIR`) still wins outright, so two runs sharing one state dir
share its database and lease however they are named — give each its own `--state-dir`
when isolation has to be guaranteed. Both it
and `--window-id` are also accepted by `daintree.session.open` as `projectId`/`windowId`,
since that surface exists to repoint a process a client cannot restart.

### Naming a skill: `--skill` and `--list-skills`

The backend picks which runbooks a turn loads. That is right for ordinary use and wrong
for developing a runbook: when the turn goes badly you cannot tell whether the runbook is
bad or the selector simply did not pick it. `--skill` removes that ambiguity by naming
one.

```bash
daintree-assistant --list-skills                       # what can this backend load?
daintree-assistant --list-skills --json | jq -r '.skills[].id'
daintree-assistant --skill daintree.orchestration.multi-agent "spin up two reviewers"
daintree-assistant --skill a.one --skill b.two "..."   # repeat for more than one
```

`--list-skills` is the lightest route in the binary: one capability read against the
configured endpoint. It takes no project lease, opens no database and connects no MCP, so
it answers while another assistant owns the project. Text by default; `--json` writes one
indented document, `{"catalogRevision": "...", "skills": [{"id", "title"}]}`, sorted by
id. Exit `0` on a catalog read — **including an advertised empty one**, which is a real
answer — `1` on a config/fetch failure or a backend that advertises no catalog at all,
`2` on cancellation. A `--json` failure is still JSON:
`{"error": {"code": "...", "message": "..."}}`.

`--skill` is repeatable, one id per occurrence — commas are not a separator, because a
comma is legal inside an opaque backend id. Order matters: the backend admits pins in the
order given and budgets them against its active-skill cap. Exact repeats collapse; an
empty value (`--skill=`, which is what an unset shell variable expands to) is rejected
rather than run unpinned. Pins are rejected on routes that never run a turn
(`doctor`, `status`, `daemon`, `reset`, `support-bundle`) instead of being silently
ignored.

**Nothing about `--skill` is allowed to fail quietly**, because a run that silently did
not load the runbook looks exactly like one that did:

- The backend must advertise `skills.pinned_skill_ids`. The field is validated with
  `extra="forbid"`, so sending it to a deployment that predates it would 422 the whole
  turn — and withholding it would run unpinned. An unaware or unreachable backend
  therefore **fails the launch before a turn is spent**.
- An id that is not in the advertised catalog fails the launch too, with a near miss
  (`unknown skill id "daintree.foundatoin"; did you mean "daintree.foundation"?`).
- A backend that accepts pins but advertises no catalog cannot be checked locally. That
  is a `warning`, not a failure, and the server-side codes below are the backstop.
- A pin the backend accepted but could not honour arrives as an ordinary `warning` event
  carrying its code: `unknown_skill_id_ignored`, `pinned_skill_not_executable` (the id
  exists but is outside this profile's menu), or `pinned_skill_over_cap`. Each is
  reported once per session — the pin list cannot change mid-conversation, so repeating
  it every round would only bury the tool activity.

A pinned skill never narrows the tool inventory. It rides alongside the backend's own
selection rather than replacing it, so the turn still loads whatever else it needs.

Four rules worth knowing before you script against them:

- **You need no key.** There is no sign-in: the backend holds its own upstream
  credential, so a headless run works with nothing but a prompt. `--api-key-file` and
  `DAINTREE_API_KEY` are for the case where you want a SPECIFIC account to fund the run
  instead — the backend prefers a caller-supplied key over its own.
- **The key never rides argv.** `ps` is world-readable, so `--api-key-file` takes a
  path. The file is read with a bounded read (a FIFO would otherwise defeat
  `--timeout`) and checked with the same shape rule `DAINTREE_API_KEY` gets, so a stray
  newline fails locally instead of as an opaque header error every turn.
- **A named key that cannot be read is FATAL, never a fallback.** Both a missing file
  and `--api-key-file=` — what a harness produces when it expands an unset variable —
  fail at the argument boundary. Falling through to the backend's own credential would
  bill the wrong account and hide the mistake behind a successful-looking run.
- **An explicitly false boolean wins.** `--auto-approve=false` beats
  `DAINTREE_ASSISTANT_AUTO_APPROVE=1`. An *absent* flag leaves the env in charge.

### Isolation

A harness should never touch the developer's real state. `--state-dir` relocates the
database, the artifacts and the owner lease, so an isolated run shares nothing with a
cockpit the developer has open. Nothing else needs supplying — there is no credential to
carry across:

```bash
daintree-assistant \
  --state-dir /tmp/harness/state \
  --log-dir /tmp/harness/logs --debug-log \
  --backend-url http://127.0.0.1:8473 \
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
| `skill:loaded` | `titles[]` | the backend's selector loaded runbooks (early cue, not authoritative) |
| `skill:decision` | `active[]`, `newlyLoaded[]`, `selector` | the committed skill outcome for a round |
| `tool:call` | `id`, `name`, `args` | a call is starting |
| `tool:result` | `id`, `name`, `ok`, `summary`, `error`, `auditId?`, `async?` | settled |
| `info` / `warning` | `message` | non-fatal; **neither changes the exit code** |
| `error` | `message` | fatal for this turn |
| `result` | see below | terminal envelope; always last |

**Do not treat a `warning` as failure, and do not decide the outcome from an `error`
line.** Gate on the terminal `result` envelope — it is the only authority.

`skill:decision` is what a skill test asserts on. It is emitted once per round that
reaches committed metadata — **including rounds where nothing new loaded**, so the active
set is reported even when it did not change:

```json
{"type":"skill:decision","ts":1787300000002,"seq":9,
 "active":[{"id":"multi_agent","title":"Multi-agent orchestration"},
           {"id":"daintree_foundation","title":"Daintree orchestration foundation"}],
 "newlyLoaded":[{"id":"multi_agent","title":"Multi-agent orchestration"}],
 "selector":{"ran":true,"degraded":false,"taskType":"orchestration",
             "confidence":0.96,"reason":"coordinating multiple agents"}}
```

`active` and `newlyLoaded` are always arrays, never `null`, and every entry carries both
`id` and `title` exactly as the backend sent them. All five `selector` keys are always
present; `confidence` is `null` when the selector reported none, and `taskType`/`reason`
are `""` rather than absent.

**`selector.degraded` is the field to gate on.** A degraded selector fails open and reuses
the previous round's active set, so a run can carry precisely the right runbook for
entirely the wrong reason — `active` alone cannot tell you that happened.

`skill:loaded` still fires, unchanged, and remains titles-only. Its value is *timing*: it
is the only skill signal available before the upstream model connects. It is **not**
authoritative — it fires per attempt on a delta, so a retried round can report a load that
the committed round did not repeat. Never reconstruct the active set from it.

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

### Background work in a one-shot is opt-in

**By default a one-shot does not run the scheduler.** It takes the project lease briefly
and never spawns a supervisor daemon, so there is no poll loop for background work to
register with: the async tools fail closed with `ASYNC_UNAVAILABLE` rather than handing
back a handle nothing will ever settle, and the backend is told `scheduler_active: false`
so the model plans around the absence instead of into it. A short scripted query pays
nothing for machinery it will not use.

Pass **`--run-scheduler`** to opt in. It starts the scheduler and the async coordinator
for the life of the run — the same shape `mcp --stdio` already has — before the first
backend round, so `scheduler_active` is `true` on the round where the model decides
whether to start background work at all. After the turn, the run stays open until the
async work **this run started** has settled and its completion has been published, then
tears everything down and exits.

```bash
daintree-assistant --json --run-scheduler --timeout 15m \
  "spawn agents on the three ready worktrees and report when they finish"
```

`--run-scheduler` **requires a positive `--timeout`**. Settling is not guaranteed — an
invocation whose terminals stay unreadable does not advance toward expiry — so an
unbounded flagged run could wait forever. If the bound expires with work still live, the
run warns and leaves that work durably live for the next owner; nothing is cancelled or
abandoned. A run whose turn otherwise succeeded then reports `cancelled` and exits `2`,
with the answer still in `result.content`. A run that already **failed** keeps its
`error` status and exit `1` — an expired wait never downgrades a real failure into a
cancellation.

The bound is cooperative, not a hard kill, and it has no second deadline behind it.
Teardown joins any in-flight scheduler tick before closing the database, so a transport
that ignored cancellation entirely could hold the exit open indefinitely. That is the
deliberate trade — returning while a tick still held the store would be a data race — but
do not read `--timeout` as a guaranteed upper bound on process lifetime.

Four things are worth knowing before you reach for it:

- **It waits for *this* run's async work only.** The coordinator adopts every live async
  row in the project when it starts — whoever holds the lease supervises everything — but
  an inherited backlog never decides how long your script runs. Adopted work is polled
  opportunistically and stays live for the next owner if it outlasts the run.
- **Watcher and timer rows never hold the run open.** A watcher is long-lived and a
  timer can be scheduled arbitrarily far out, so neither has a "quiescent" state; if they
  gated the exit, `--timeout` would become the normal way a flagged run ends. Due work
  fires during the turn and during the wait; the rows persist either way. If a timer fires
  **during** the run and itself starts async work, that work carries this run's session
  and is normally waited on like any other — but the barrier reads a count rather than
  freezing registration, so work registered in the instant between the last read and
  teardown is not waited on. It stays durably live for the next owner, which is the same
  place every unfinished invocation ends up.
- **Completions do not come back as tool results.** They land in the durable attention
  inbox, which the next session reads. The attention callback is deliberately nil for the
  same reason it is under `mcp --stdio`: a callback with nowhere to render would mark
  those events delivered and consume them before anyone saw them.
- **It still spawns no daemon.** The scheduler is in-process and joined at exit; the
  lease is released as the process ends. A flagged one-shot cannot litter the machine.

Without the flag, a one-shot remains the wrong shape for "spawn agents and wait". Use an
in-turn wait (`terminal.awaitAll`) so the work completes inside the turn, or drive a
longer-lived session.

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
