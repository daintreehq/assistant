# Headless operation

How to drive the assistant from a script, a test harness, or another agent — without
a terminal, and without rewriting the process environment to say what argv says
perfectly well.

**The whole binary is headless now.** There is no terminal UI: Daintree embeds it over
`host --stdio` and renders the conversation natively. What follows is the full list of
ways in; pick by how many turns you need, and by whether the caller is a script, an
agent, or a person at a shell.

| Surface | Turns | Output | Use it when |
|---|---|---|---|
| `mcp --stdio` | many | MCP tools | **another agent drives the assistant as a sub-agent** |
| `--json <prompt>` | one | JSONL on stdout | scripting, CI gates, one-shot queries |
| `--json --multi-turn` | many | JSONL on stdout | **testing a runbook that needs a short conversation** |
| the line REPL | many | plain lines | a person at a shell or over SSH (`--classic` is a deprecated no-op) |
| `host --stdio` | many | NDJSON, protocol v3 | you are Daintree, or reimplementing it |

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

**The server holds no configuration of its own.** A client launches this process once
and keeps the pipe for its whole session; it cannot restart it when you want a different
project. So project, tier, state dir and identity are all arguments to
`daintree.session.open`, and the process env supplies defaults only — repointing those is
a close/open pair, never a reconnect.

Endpoints and credentials are the exception, and they are **pinned at launch**. See
[the process policy](#the-process-policy-is-the-authority-ceiling) below: they decide
where the conversation goes and whose credential pays for it, which makes them the
operator's call rather than an argument the caller can reach.

**No secret is a tool argument.** The backend key is named by `apiKeyFile` and the Daintree
MCP bearer by `mcpTokenFile` — paths, never values. Both are chosen by a *model* on this
surface, and that bearer authorises system-tier Daintree actions for its whole validity
window: inline, it could be echoed back by a prompt injection, logged by your MCP client, or
captured by traces outside this repository. Omit `mcpTokenFile` to inherit
`DAINTREE_MCP_TOKEN` from the server process.

### The process policy is the authority ceiling

Every field above is chosen by a **model** whose context can be steered by repository
text, tool output, or anything else it reads. A session argument that changes a
filesystem root, a network origin, a credential, a permission tier, or approval
behaviour is therefore part of the security boundary — and prose in a prompt saying
"don't do that" is not a control. `internal/mcpserver/policy.go` is.

The rule is one-directional: **a session may narrow what the operator launched this
process with, and can never widen it.** The policy is fixed at launch, where the operator
decides it, and there is deliberately no tool that reaches it.

What `mcp --stdio` pins by default:

| Dimension | Default | Why |
|---|---|---|
| `backendUrl` | **pinned** — an override is refused | it decides where the whole conversation, project context and every tool result are posted; an unbounded one is both an SSRF primitive and an exfiltration route |
| `mcpUrl` | **pinned** | it decides which server advertises the tools the assistant believes and calls |
| `apiKeyFile` / `mcpTokenFile` | **pinned** | a path keeps the *value* out of model context but still lets a model *select* a credential — spending another account, or acquiring a system-tier Daintree bearer |
| `project` / `stateDir` / `logDir` | confined to the directories the process was launched against | a prompt injection in one repository must not open a system-tier session on another one, or on `$HOME` |
| `tier` | at most the process tier | a request *above* the ceiling is refused, never quietly downgraded — a caller told it has system tier would read every later refusal as a bug |
| `approvals: "auto"` | refused unless the process itself was launched with auto-approve | a session cannot grant itself unattended mutation |
| `approvals: "delegate"` | refused unless launched with `--allow-delegated-approvals` (or with `--auto-approve`, which is the broader grant and implies it) | the caller agent settling its own requests is delegation, not authorization — see below |

Path confinement compares **resolved** paths: both the allowlisted root and the requested
path go through `filepath.EvalSymlinks` first, so `/allowed/link -> /etc` is outside the
root even though it reads as inside it. A path that does not exist yet — the usual case
for a state directory the open is about to create — resolves through its nearest existing
ancestor, which is where any escaping symlink would have to live.

A harness that genuinely needs a different endpoint or credential launches a **second
server** against it. That is a decision made at a shell, by a human, once — not an
argument a model can reach.

Embedding the server in a trusted host, where the operator *is* the caller, is the one
case with no ceiling. It requires naming `Serve` with `TrustedUnconfined` explicitly:
`ServeModelFacing` refuses that marker outright, so the unconfined configuration takes
more code than the safe one rather than less.

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
| `daintree.attention` | read what settled in the background (peeks — does not consume) |
| `daintree.attention.ack` | acknowledge the items you have processed |
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
- **`poll`'s `waitMs` is a real long poll.** It wakes on any *change* — new content, a
  tool starting or finishing, the turn becoming blocked on an approval — not only on the
  run finishing. Waiting for completion alone meant a 60s poll could sit through all of
  that and report a blocked decision only when the budget expired.
- **`inject` and `interrupt` take the `runId` you meant.** It is optional but strongly
  recommended: without it, a request written for one turn acts on whichever turn happens
  to be current when it lands, which over a slow pipe is a different turn. A stale
  `runId` is rejected with an error naming the live run, rather than silently steering
  the wrong work.
- **Background work reports through `attention`, never as a late run event.** A tool that
  accepted async work is recorded in the run's `asyncOperations` ledger — from the run
  itself, so the handles survive you advancing `sinceSeq` — and the completion arrives in
  the inbox carrying `asyncId` so you can match the two. Their status is `accepted`, never
  `finished`: the run saw the handle issued and will never see it settle.
- **`attention` peeks; `attention.ack` consumes.** Acknowledging inside the read is
  at-most-once delivery — the rows are marked before the response is known to have reached
  you, so a dropped connection loses them permanently, and an attention row is the *only*
  report background work ever makes. Read, act, then `attention.ack` the ids. A retry after
  an ambiguous failure is idempotent: already-acknowledged ids come back under `unknown`
  rather than erroring. (`acknowledge:true` is still available if you accept the risk.)
- **Always close what you open.** A session holds the project's owner lease for its whole
  life, and a leaked one blocks every other process from opening that project. Close is
  **safe to retry**: an already-closed session reports `acted:false, state:"already-closed"`
  rather than erroring, so a lost response costs a duplicate call and not a stuck lease.

  Teardown runs on the server, not inside your call. A close that takes longer than ten
  seconds returns `state:"closing"` and keeps going — the session stays **listed** in that
  state until it finishes, and its lease is released then. A teardown that genuinely
  **fails** is terminal: the session stays listed as `state:"close-failed"` with its lease
  believed still held, and retrying does not tear it down again, because running
  `Runtime.Close` over a half-closed App is not a retry. Restarting the MCP server is what
  releases it; the OS drops the flock on exit.

  Both states count against `MaxSessions`, since their runtime may still hold the lease —
  which is exactly why they are listed rather than hidden.
- **Recovering from a lost response.** Every session reports `currentRunId` *and*
  `recentRuns` (newest first, each with a short echo of its prompt), and `ask`'s busy
  refusal names the live run. So an `ask` whose response never arrived is recoverable
  either way: while the turn is still going, `currentRunId` hands the handle back; once it
  has finished — the case a fast run lands in — `recentRuns` does. That second half
  matters more than it sounds, because a retried `ask` on an idle session is *accepted*,
  and simply does the work twice.
- **A run is bounded.** `timeoutMs` on `ask` caps the RUN (default 30 minutes, server
  capped); `waitMs` only caps how long the *call* blocks. Letting a wait expire leaves the
  turn going; letting the deadline expire cancels it, and the outcome says
  `RUN_DEADLINE_EXCEEDED` rather than the bare `cancelled` you would get from your own
  interrupt. There is no unbounded option: a run holds the session, and the session holds
  the project lease. The bound is cooperative — a tool that ignores cancellation is
  reclaimed at shutdown, not by this.
- **A turn that records no terminal event FAILS.** It does not report an empty success.
  That shape is what a runtime with an unwired event sink produces — a bug this server has
  shipped once — and calling it success is exactly why it went unnoticed: the caller was
  told the run completed, so nothing looked wrong except that nothing had happened. The
  outcome is `error` with `RUN_EVENT_STREAM_INCOMPLETE`, and any content is diagnostic
  rather than an answer.
- **Interrupted prose is kept.** A turn cancelled or failed mid-sentence reports what it
  had streamed rather than only a sentinel — including the shape the runtime actually
  produces, where the cancellation itself carries no content.
- **Mutating tools need approval,** and the mode is per session:
  - `decline` — refuse immediately and carry on. Safe for an unattended caller, but the
    session can never actually change anything. This is what an omitted `approvals`
    resolves to *unless* the process was launched with auto-approve; see below.
  - `delegate` — park the call and hand it to **the calling agent** with its risk,
    consequence and redacted args. A parked call **blocks the whole turn**, so only
    choose this if you will poll. It fails closed on a timer (`approvalTimeoutMs`,
    default 5 minutes), and interrupt or close releases everything outstanding.
    Cancellation always wins: a call approved after you interrupted the turn does not
    run. A decision may name the `runId` you were watching, and one that arrives after
    that turn ended is rejected rather than applied to its successor. Each parked call
    carries `needsTypedConfirm` — the safety layer's own verdict that the action is
    irreversible and deserves more than a click — and `decisionAuthority`, which says
    who is actually deciding. Requires `--allow-delegated-approvals` at launch — or
    `--auto-approve`, which is strictly broader and therefore implies it.
  - **An omitted `approvals` inherits the launch configuration**, not a fixed default: it
    resolves to `auto` on a server launched with auto-approve, and `decline` otherwise.
    Pass `decline` explicitly if you need fail-closed behaviour whatever the server was
    launched with.
  - `auto` — never ask. Equivalent to `--auto-approve`, and permitted only if the
    process was launched with it.

  A blocked run is reported as blocked: pending approvals ride the run's `poll` response
  and its `nextAction` says so, because "still running" would send you polling harder at
  something that will never move on its own.

#### `delegate` is delegation, not human authorization

This mode used to be called `ask`, and the name was a lie by implication. **Nobody is
guaranteed to be asked.** The pending approval is handed to the same model that is driving
the session, which then calls `daintree.approve` — so a request the assistant made is
answered by the agent that prompted it, and any repository text able to steer that agent
can steer the answer too. Refusing `auto` while offering `ask` looked like a safety
posture and was not one: the same model reached the same outcome by a longer route.

(A client supporting MCP elicitation may put the request in front of a person instead —
but the protocol carries no attestation either way, so the server cannot tell and does not
claim to. `decisionAuthority` reports the guaranteed floor, not a hopeful reading.)

So the mode is named for what it does, every pending approval reports
`decisionAuthority: "caller-agent"`, and enabling it is a **launch** decision rather than
a session argument — because whether the caller agent is a person's terminal or an
unattended loop over an untrusted repository is something only the operator knows.

Two launch settings enable it, and it is worth being exact about the second:

- `--allow-delegated-approvals`, which enables it and nothing else. It has no dedicated
  environment variable, deliberately: the point is that a human at a shell decides whether
  the agent on the other end is one they trust, and an inherited variable is not that
  decision.
- `--auto-approve` (or `DAINTREE_ASSISTANT_AUTO_APPROVE=1`), which enables `auto` and
  therefore `delegate` too. Auto runs every tier-permitted mutating call with nothing
  consulted; delegate runs the subset the caller chooses to release. Refusing the narrower
  grant to an operator who allowed the broader one would only push a caller toward the
  mode that reviews nothing. So yes — the environment variable does indirectly permit
  delegation, by permitting more than it.

It is genuinely useful. A harness driving a controlled test project wants exactly this:
real mutating work, decided by the driving agent, with every request visible in the
timeline. What it is not is a boundary you can put an untrusted repository behind.

A **human** mode — where only an out-of-band decision the model cannot forge releases a
call — is not implemented. It needs a channel that does not exist on this surface yet:
client-certified elicitation, a native-host decision, or a signed capability minted by a
trusted host. Until one does, the honest answer is that this surface has no human
authorization, which is why it does not claim any.
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
| `daintree://session/{sessionId}/run/{runId}[?fromSeq=N&limit=M]` | the run's event timeline, in pages larger than `poll`'s window |
| `daintree://session/{sessionId}/log` | the tail of the **server process's** structured debug trace |

Read the transcript when a poll reported a non-zero `withheldEvents`. It is **paged**,
not unbounded — a resource that returned every retained event was the largest single
response this server could produce, reachable by a caller with no idea how long the run
was, and it had to be built and encoded in full before anyone could decide it was too
big. Pass `fromSeq` and `limit`; the response carries `nextSeq`, `remaining`, `complete`
and `totalEvents`, so you can size the job before you start and know when you have
reached the end. "Larger than a poll window" is the useful property here; "unbounded"
never was.

The log is **per process, not per session**, and the URI's session id addresses the
server rather than isolating anything. `debuglog` keeps one active file — a per-session
log would silently redirect earlier sessions' writes into the newest session's file — so
every session in this server reports the same path, and that file contains every
session's conversation and tool activity. Filter by the `sessionId` field on each line.
Treat that as a convention, not a boundary: if isolation matters, run one session per
server process, which is what the default `MaxSessions` is for. Real per-session logs
need an injected logger rather than a package-global singleton, and that has not been
built. The read is bounded to the last 256 KB (the tail, because that is where a failure
is), passed through the redactor, and only exists if the session was opened with
`debugLog:true`.

### Everything a caller can ask for by the page has a ceiling

A default is not a bound: it protects the caller that does not think about the size, not
the server from the caller that does. Every model-visible collection therefore has a
server maximum as well, and asking for more gets you the maximum plus the count you did
not receive — never an error, and never a silent truncation.

| Surface | Default | Server maximum |
|---|---|---|
| `poll` / `ask` events (`maxEvents`) | 40 | 500 |
| transcript resource page (`limit`) | 500 | 500 |
| `attention` items (`limit`) | 50 | 200 |
| pending approvals (per run, and per `approvals` listing) | — | 50 |
| approval argument preview | — | 4 KB |
| async operations on a run | — | 100 |
| text on one event (and one run error, attention title/summary) | — | 8 KB |
| all event text in one response | — | 256 KB |
| run content in a poll response | — | 64 KB |
| debug-log tail | — | 256 KB |

A page *count* is not a size bound — 500 events whose text is unbounded is unbounded — so
the per-field and aggregate byte budgets above matter as much as the item counts. A
truncation marker is counted *inside* the stated maximum rather than appended past it.

`attention` pages **inside the runtime**, not after the fetch, and that is load-bearing
rather than an optimisation: acknowledgement is version-conditional on the exact rows
read, so a handler that fetched everything and then acknowledged a page would either mark
rows it never delivered or consume a *newer* version than the one it showed. Because the
page and the acknowledgement now cover the same rows, `limit` and `acknowledge:true`
combine safely — only what the page actually carried is marked delivered. `more` says
another page is waiting.

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

Exit codes (`domain.OneShotExitCode`): `0` success, `1` error, `2` cancelled, `4` hard
timeout. `3` is reserved and never emitted.

Every JSONL frame carries `schemaVersion`, not just the session header and the terminal
`result` — a streaming consumer can reject an incompatible schema on the first line it
sees, including a setup `error` emitted before any session frame exists.

**`--timeout` is bounded in two stages.** The first cancels the run's context at the
deadline; that is cooperative, and a context only bounds code that *watches* it — a
syscall in flight, a tool that ignores cancellation, or a `-` stdin read that never sees
EOF is not preempted by one. So a watchdog arms for `--timeout` + a 30s grace
(`domain.HardTimeoutGrace`) and, if the process is still alive then, kills it with exit
`4` and a stderr diagnostic. A clean run disarms it long before; the grace is sized so
the process is never killed mid-flush, trading a hung job for a corrupted one. Exit `4`
therefore means "nothing unwound", and unlike `2` it has **no terminal `result` line** —
that is the signal that the stream ended abnormally.

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
| `--allow-delegated-approvals` | — | `mcp --stdio` only: lets a session choose `approvals:"delegate"`, where the CALLING AGENT settles each confirmation. No *dedicated* env counterpart, deliberately — a human at a shell should decide whether the agent on the other end is one they trust. `--auto-approve` (and its env var) permit delegation too, by permitting more than it. Rejected on any other subcommand rather than silently ignored |
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
attached session the developer has open. Nothing else needs supplying — there is no credential to
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
but it will fail rather than double-open if an attached session already owns that project. A
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
that point (bad flags, signed out, the project lease held by a live attached session) produces
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
the next prompt still runs, as it would in the line REPL — but the failure is latched:
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

## Diagnosing a run

`--debug-log` writes a structured trace to `<log-dir>/<date>-<sessionId>.log`, and the
`session` line tells you exactly which file. It is the ground truth for what the model
and tools actually did; grep it by `runId`/`turnId`/`round`. See `docs/LOGGING.md` for
the event reference and the fix philosophy (a model mistake is usually a *documentation*
bug in the prompt, skill, or tool schema — not a dumb model).
