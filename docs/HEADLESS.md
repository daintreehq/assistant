# Headless operation

How to drive the assistant from a script, a test harness, or another agent — without
a terminal, without the cockpit, and without rewriting the process environment to say
what argv says perfectly well.

There are five headless surfaces. Pick by how many turns you need, and by whether the
caller is a script or another agent.

| Surface | Turns | Output | Use it when |
|---|---|---|---|
| `mcp --stdio` | many | MCP tools | **another agent drives the assistant as a sub-agent** |
| `--json <prompt>` | one | JSONL on stdout | scripting, CI gates, one-shot queries |
| `--json --multi-turn` | many | JSONL on stdout | **testing a runbook that needs a short conversation** |
| `--classic` + piped stdin | many | human-rendered | a multi-turn exchange you intend to read yourself |
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
  with; passing an explicit `[]` clears those defaults for this session. An **unknown** id fails
  the **open**, not the first turn, so the failure lands where the caller is looking —
  when there is a catalog to check it against. A backend that accepts pins but advertises
  no catalog opens with a warning instead, and reports the bad id on the first turn; a
  backend that does not accept pins at all fails the open whatever the ids are. A
  catalogued id that this profile cannot execute also opens fine and warns per turn. `facts.pinnedSkills` reports what the session REQUESTS on
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

`--json` requires a prompt, or `--multi-turn` to read a whole conversation from stdin
(see [Multi-turn](#multi-turn)) — there is no JSONL *interactive* mode. A prompt that begins
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
| `--multi-turn` | — | one prompt per stdin line, one session, one transcript. Requires `--json`; each line capped at 1 MiB |
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
| `error` | `message` | fatal for the turn it falls in — or for the run, when it lands outside any turn (setup failure, empty `--multi-turn` script) |
| `turn:prompt` | `turn`, `prompt` | **`--multi-turn` only** — opens a turn |
| `turn:end` | `turn`, `status` | **`--multi-turn` only** — closes it with that turn's outcome |
| `command:result` | `command`, `handled`, `title`, `content`, `quit`, `conversationCleared` | **`--multi-turn` only** — a slash command ran between turns |
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

A bare `--json "prompt"` is one turn per process, and each process mints a fresh
session. Project state — watchers, the attention inbox, async futures — persists in the
state dir and is adopted by the next owner, but the *conversation* does not carry over.

`--multi-turn` is the plural of that: one process, one session, **one prompt per stdin
line**, and the whole conversation as a single JSONL transcript. It is what a runbook
test usually needs, since most orchestration runbooks only reach their interesting
behaviour on the second or third turn — after something has been spawned, or a result
has come back.

```bash
printf '%s\n' "list the worktrees" "now check the first one" \
  | daintree-assistant --json --multi-turn --state-dir /tmp/harness/state
```

A prompt file checked in beside the runbook it exercises works the same way, since the
prompts arrive on stdin either way:

```bash
daintree-assistant --json --multi-turn < testdata/worktree-review.prompts
```

It requires `--json` (without it, piping stdin to `--classic` already gives you
human-rendered multi-turn) and, like `--prompt-file`, refuses to share a run with another
prompt source: naming two is a mistake, not a precedence rule.

**One line is one prompt.** Blank lines are skipped, and a final line without a trailing
newline still counts. A single line is capped at 1 MiB, the same bound `--prompt-file`
applies to its read and for the same reason — a line has no natural end, so input with
no newline in it would otherwise grow until the process died. An over-long line is
**refused**, never truncated: the run fails rather than silently asking a different
question. A prompt that must span several lines is `--prompt-file`'s job — that flag's
`-` spelling deliberately still means *all of stdin is one prompt*, newlines included,
and `--multi-turn` does not re-cut it.

**Slash commands stay available**, exactly as on `--classic` stdin — `/clear` most of
all, so a script can reset between prompts. In this mode they reach the stream as a
`command:result` line rather than as rendered text, since stdout carries only JSONL. A
command line the catalog does not recognise still produces a `command:result`, with
`handled: false` — which is how a script catches its own typos, since a mistyped
command otherwise looks exactly like one that ran and said nothing. `/quit` stops
consuming input.

`/clear` resets **session state**, not just the conversation: it also drops watchers,
async operations, the attention inbox and the cost ledger, which is worth knowing when
it shares a run with `--run-scheduler`. What it never touches is the transcript —
already-emitted lines stand, `seq` keeps climbing, `stats` keep accumulating, and an
earlier failed turn stays failed. A script cannot launder its own failure by clearing.

### What the transcript looks like

The stream is one ordered transcript of the whole process, not several concatenated
runs:

- **one `session` header**, first, because it is one session;
- **`seq` monotonic from 0 across every line**, so the transcript has a single total
  order;
- **one terminal `result`**, last, whose `stats` accumulate over every round of every
  turn and whose `content` is the final turn's answer;
- **a `turn:prompt` / `turn:end` bracket per turn.**

The bracket is the unit to slice on. A turn spans one whole exchange — however many
model rounds and tool calls it took — so `assistant:start` marks a *round*, never a
turn, and everything between a matching `turn:prompt` and `turn:end` belongs to one
prompt. Turn numbers are zero-based, and a slash command does not consume one.

```json
{"type":"turn:prompt","ts":…,"seq":1,"turn":0,"prompt":"list the worktrees"}
{"type":"turn:end","ts":…,"seq":6,"turn":0,"status":"success"}
```

`turn:end` carries no exit code and no per-turn `stats`: an exit code is a property of
the *process*, and with the bracket in hand a consumer counts the lines between the
boundaries itself rather than trusting a second accounting block that could disagree
with the one on `result`.

`turn:prompt` **echoes the prompt**, because nothing else in the stream does and a
transcript that cannot say which question produced which answer is not one. It is your
own stdin, so it adds no secret you did not already supply — but it does put that text
on stdout, which is worth knowing before piping a prompt file into a CI log.

### Failure, cancellation and the exit code

**A run where turn two failed is a failed run.** A failed turn does not stop the script —
the next prompt still runs, as it would in the classic REPL — but the failure is latched:
a later success cannot clear it. The outcome is worst-wins across turns, `error` >
`cancelled` > `success`, and `result` reports the run's, while each `turn:end` reports
its own turn's. Gate on `result`; it remains the only authority for the process.

**`--timeout` bounds the whole process**, exactly as it does for a single-turn run —
setup, every turn, the wait for the next line of stdin, and `--run-scheduler`'s async
barrier. It is a wall-clock bound on the invocation, not a per-turn allowance; a
per-turn reading would let a ten-prompt script run for ten times the number you asked
for, which is the opposite of a bound. When it expires the current turn unwinds as
cancelled and the remaining prompts are not run. An otherwise-successful run then
reports `cancelled`; an earlier failed turn still outranks it, since worst-wins applies
to the expiry like any other outcome.

An empty script — stdin with no prompt on it at all, only blank lines or commands — is a
run **failure** (exit 1, reported on the stream), not a quiet success. It is nearly
always a harness mistake, and a transcript of nothing should not look like a clean run.

## Skill-authoring test loop

Developing a runbook has two halves that fail differently, and the loop below exists to
keep them apart. The **backend** owns skills — it loads them from disk, selects them, and
injects the body — so an authoring mistake shows up there as a catalog that will not load.
The **CLI** is what makes a run repeatable and readable: the same prompt every time, state
that shares nothing with your cockpit, and a JSONL transcript that says which runbook was
actually active. Without the second half you are reading prose and guessing.

**1. Start the backend with an EMPTY overlay directory.** The packaged catalog is
read-only in the sense that matters — a scratch edit must not land in the server's own
tree — so the backend merges a second directory on top of it. Point it there in
`../assistant-backend/.env`, where `.env.example` already carries the line commented out,
rather than as a command prefix: `./dev` sources `.env` with `set -a` and deliberately
overwrites the inherited environment, so a prefixed value loses to any line the file sets.
(A direct `DAINTREE_SKILLS_OVERLAY_DIR=… python -m daintree_assistant_server` does honour
the prefix — process environment outranks dotenv — but `./dev` is the usual way in.)

```ini
# ../assistant-backend/.env
DAINTREE_SKILLS_OVERLAY_DIR=/tmp/skill-harness/skills
```

Then, from a first terminal, create the directory and start the server — leave it running:

```bash
mkdir -p /tmp/skill-harness/skills
cd ../assistant-backend && ./dev
```

It binds the `HOST`/`PORT` from `.env`, which is `127.0.0.1:8473` only if you started from
the scaffold. Empty is the deliberate starting state: the overlay is loaded during startup,
so a skill that fails to parse there is a **readiness** failure rather than a reload
failure — the server boots but stays unready, and the reload route below is
readiness-gated, so it answers `503` and cannot dig you out. Boot empty and the first draft
is recoverable without a restart.

**2. Write the skill.** The filename stem MUST equal the frontmatter `id` — the loader
passes `path.stem` as the expected id and rejects the file otherwise — so this one goes to
`/tmp/skill-harness/skills/daintree.example.skill-under-test.md`. Only top-level `*.md`
files in the directory are read; subdirectories are ignored.

```markdown
---
id: daintree.example.skill-under-test
title: Skill under test
summary: One line the selector reads to decide whether this runbook fits.
whenToUse: The situations this runbook is for, and the ones it is not.
risk: read
requiredTools:
  - context.snapshot
---

## Procedure

1. …
```

`id`, `title`, `summary` and `whenToUse` are required (those are the canonical spellings);
`risk` defaults to `read`, `requiredTools` to empty, `foundation` to false. Frontmatter is
validated with `extra="forbid"`, so a key the schema does not know is a load failure rather
than an ignored line — the one authoring error that would otherwise look like the selector
misbehaving. `requiredTools` names dotted CLI tools, but the names are only syntax-checked
on load: a typo neither fails the reload nor creates a tool, so check them against the
inventory yourself.

An overlay skill whose `id` matches a packaged one **replaces** it; a new id is **added**.
That is what lets you iterate on a shipped runbook and develop a new one with the same
mechanism, without editing the server's tree either way. Give the overlay its own
directory: pointing it at the packaged one is a no-op, and a path that exists but is not a
directory fails the load outright.

**3. Reload, from a second terminal.**

```bash
curl --fail-with-body -sS -X POST http://127.0.0.1:8473/v1/daintree/skills/reload
```

The **path** is captured once at startup, so moving the overlay needs a restart; its
**contents** are re-read here, which is the whole point — edit, POST, run, without losing
the process. Once the server is ready this is validate-before-swap: the entire replacement
catalog is built and checked before it rebinds, so a skill caught half-written answers
`409` and leaves the previous catalog serving. Fix the file and POST again; from that
point on a malformed skill costs a request, not a restart.

Both the variable and the route are development-only by construction: the route is mounted
only when an overlay is configured on a non-hardened server, and a hardened deployment that
sets a non-empty overlay value fails its readiness check. There is no way to reach this
loop against production, which is why it can be this convenient.

**4. Run the CLI against it, pinned and isolated.** List first — a reload that did not
take, or an id that does not match its filename, is cheaper to find in a catalog read than
in a spent turn:

```bash
daintree-assistant --backend-url http://127.0.0.1:8473 --list-skills --json
```

Then pin the runbook. `--skill` is what removes the ambiguity that makes runbook
development frustrating: without it, a bad turn could mean a bad runbook *or* a selector
that never picked it. See [Naming a skill](#naming-a-skill---skill-and---list-skills) for
pin ordering, the preflight, and the warning codes a backend can answer with — a pin rides
alongside the backend's own selection and can still be refused, which is why step 5 reads
back what was active rather than assuming.

```bash
mkdir -p /tmp/skill-harness/case-001/run-01
echo "the prompt this case exercises" > /tmp/skill-harness/case-001/prompt.txt

daintree-assistant \
  --backend-url http://127.0.0.1:8473 \
  --skill daintree.example.skill-under-test \
  --prompt-file /tmp/skill-harness/case-001/prompt.txt \
  --state-dir /tmp/skill-harness/case-001/run-01/state \
  --project-id skill-harness-case-001 \
  --json > /tmp/skill-harness/case-001/run-01/run.jsonl
```

`--prompt-file` keeps the prompt in a file under the case directory rather than in shell
quoting, so the prompt and the runbook it exercises are edited and diffed as a pair.

`--state-dir` is what keeps the case out of the developer's own cockpit — no shared
database, artifacts or lease (see [Isolation](#isolation)) — but note that it **isolates
without resetting**: a re-run
against the same directory inherits whatever the last one left there. Give each iteration
an empty directory of its own, and reuse one only when the persistence is the thing under
test. `--project-id` here is identity rather than isolation, since an explicit
`--state-dir` outranks its scoping; it is also only the *fallback* project id — a connected
Daintree supplies its own.

**5. Read the transcript.** `skill:decision` is the committed answer to "which runbook was
active", and `selector.degraded` is the field to gate on alongside it — see
[The event stream](#the-event-stream) for why, and for the full shape.

```bash
cd /tmp/skill-harness/case-001/run-01

jq -c 'select(.type == "skill:decision")
       | {active: [.active[].id], degraded: .selector.degraded}' run.jsonl
jq -c 'select(.type == "tool:call")   | {name, args}'            run.jsonl
jq -c 'select(.type == "tool:result") | {name, ok, summary}'     run.jsonl
jq -c 'select(.type == "result")      | {status, stats}'         run.jsonl
```

The reads answer the three questions a runbook is judged on: did it load, did it drive the
tool calls it was written to drive, and what `result.stats` recorded — read as a lower
bound on spend, for the reasons in [The event stream](#the-event-stream). `ok` lives on
`tool:result` only; asking for it on a `tool:call` yields a silent `null`, not an error.

Two limits are worth knowing before you script a case against this loop:

- **The run above is ONE turn.** Each one-shot prompt run mints a fresh session, so a
  runbook whose procedure spans a conversation cannot be exercised by running the command
  twice — the second process shares the project state but not the conversation. Use
  [`--multi-turn`](#multi-turn) to drive the whole script through one session and one
  transcript.
- **Background work does not settle unless the scheduler runs.** By default a one-shot
  starts no scheduler, so `terminal.run.async` and `terminal.await.async` return
  `ASYNC_UNAVAILABLE` rather than a handle nothing will resolve — and the backend is told
  as much, so the model plans around it. A runbook that expects work to settle *after* the
  turn needs [`--run-scheduler`](#background-work-in-a-one-shot-is-opt-in) and the positive
  `--timeout` it requires. One that waits in-turn with `terminal.awaitAll` does not.

## Diagnosing a run

`--debug-log` writes a structured trace to `<log-dir>/<date>-<sessionId>.log`, and the
`session` line tells you exactly which file. It is the ground truth for what the model
and tools actually did; grep it by `runId`/`turnId`/`round`. See `docs/LOGGING.md` for
the event reference and the fix philosophy (a model mistake is usually a *documentation*
bug in the prompt, skill, or tool schema — not a dumb model).
