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
    "env": {} } } }
```

The `env` block is empty on purpose: the backend funds every model call from its own
credential, so a headless MCP server needs no key at all. `DAINTREE_API_KEY` belongs there
only on a deployment with accounts, and then it is an ACCOUNT bearer naming which caller
the requests are from — never a provider key, and never a thing that pays.

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
where the conversation goes and which account it is made from, which makes them the
operator's call rather than an argument the caller can reach.

**No secret is a tool argument.** The backend account bearer is named by `apiKeyFile` and
the Daintree MCP bearer by `mcpTokenFile` — paths, never values. Both are chosen by a *model* on this
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
| `questions: "delegate"` | **permitted** by default | answering a question authorises nothing the assistant could not already do; it picks among options the assistant proposed, so gating it would only stop this surface reaching the branches the product reaches |

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
| `daintree.questions` | list multiple-choice questions the session is **parked** on |
| `daintree.question.answer` | answer one by the index of the option you choose |

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

- **Multiple-choice questions are answerable too,** and that is what makes this surface
  able to test the product rather than a variation on it. The assistant sometimes needs a
  planning decision — which worktree, which of three approaches — and MCP used to report
  `QUESTION_UNAVAILABLE`, so a turn that hit one took a *different path here than it takes
  in Daintree*. An end-to-end run that cannot reach the same branch is not testing the
  thing it claims to test. `daintree.questions` lists what is parked;
  `daintree.question.answer` answers one by the **index** of the option you choose.

  A question is not an approval, and the difference decides the defaults. An approval asks
  "may I do this?", which has one safe answer — no — so an unanswered one times out to
  *rejected* and the turn carries on having skipped the call. A question asks "which of
  these did you mean?", which has **no safe answer at all**: inventing one puts words in
  your mouth and then acts on them. So an unanswered question times out to **cancelled**,
  an out-of-range index **cancels rather than clamping** to the nearest option, and there
  is no default anywhere in the path. A parked question blocks the turn and wakes a long
  poll exactly as a parked approval does, and it rides the run's `pendingQuestions`.

  Questions are their **own** setting — `questions: "decline" | "delegate"`, defaulting to
  `decline`, with its own `questionTimeoutMs`. They were derived from the approval mode at
  first, and that defeated the case they were added for: a harness that wants planning
  questions while keeping mutations declined could not have them without also granting
  approval authority it did not want. Answering a question authorises nothing — it picks
  among options the assistant itself proposed — so `approvals:"decline"` with
  `questions:"delegate"` is a perfectly coherent session, and the common one for a
  read-mostly test.

  There is deliberately no auto-*answer*. Bypassing a confirmation is a decision an
  operator can make; answering "which of these did you mean?" on someone's behalf is not.

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
- **`runbooks` pins runbooks for the session**, the MCP twin of `--runbook` (see below for
  the full semantics — they are identical, deliberately: the two headless surfaces must
  not drift). Omitting it inherits whatever `--runbook` this server process was launched
  with; passing an explicit `[]` clears those defaults for this session. An **unknown** id fails
  the **open**, not the first turn, so the failure lands where the caller is looking —
  when there is a catalog to check it against. A backend that accepts pins but advertises
  no catalog opens with a warning instead, and reports the bad id on the first turn; a
  backend that does not accept pins at all fails the open whatever the ids are. A
  catalogued id that this profile cannot execute also opens fine and warns per turn. `facts.pinnedRunbooks` reports what the session REQUESTS on
  every turn — the only way a caller that inherited a server-level default can see them.
  It is not a claim the backend honoured each one: an id can be in the catalog and still
  come back `pinned_runbook_not_executable` or `pinned_runbook_over_cap`.

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
`--list-runbooks --json`: both are reads that answer with ONE document rather than a run,
and a gate or a catalog a script cannot parse is not one.

### Configuration

Every knob is a flag, and every flag shadows a trusted env var and wins over it.

| Flag | Env | Notes |
|---|---|---|
| `--backend-url URL` | `DAINTREE_BACKEND_URL` | outranks the endpoint stored by `/backend` |
| `--allow-insecure-backend` | `DAINTREE_ALLOW_INSECURE_BACKEND=1` | authorizes a non-loopback plaintext `http://` endpoint, which is otherwise refused |
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
| `--runbook ID` | — | pin a backend runbook for every turn; repeatable. See below |
| `--list-runbooks` | — | print the runbooks this backend can load, then exit |

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
  `DAINTREE.md` would go, so a runbook can be tested against a synthetic brief without
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

### Naming a runbook: `--runbook` and `--list-runbooks`

The backend picks which runbooks a turn loads. That is right for ordinary use and wrong
for developing a runbook: when the turn goes badly you cannot tell whether the runbook is
bad or the selector simply did not pick it. `--runbook` removes that ambiguity by naming
one.

```bash
daintree-assistant --list-runbooks                       # what can this backend load?
daintree-assistant --list-runbooks --json | jq -r '.runbooks[].id'
daintree-assistant --runbook daintree.orchestration.multi-agent "spin up two reviewers"
daintree-assistant --runbook a.one --runbook b.two "..."   # repeat for more than one
```

`--list-runbooks` is the lightest route in the binary: one capability read against the
configured endpoint. It takes no project lease, opens no database and connects no MCP, so
it answers while another assistant owns the project. Text by default; `--json` writes one
indented document, `{"catalogRevision": "...", "runbooks": [{"id", "title"}]}`, sorted by
id. Exit `0` on a catalog read — **including an advertised empty one**, which is a real
answer — `1` on a config/fetch failure or a backend that advertises no catalog at all,
`2` on cancellation. A `--json` failure is still JSON:
`{"error": {"code": "...", "message": "..."}}`.

`--runbook` is repeatable, one id per occurrence — commas are not a separator, because a
comma is legal inside an opaque backend id. Order matters: the backend admits pins in the
order given and budgets them against its active-runbook cap. Exact repeats collapse; an
empty value (`--runbook=`, which is what an unset shell variable expands to) is rejected
rather than run unpinned. Pins are rejected on routes that never run a turn
(`doctor`, `status`, `daemon`, `reset`, `support-bundle`) instead of being silently
ignored.

**Nothing about `--runbook` is allowed to fail quietly**, because a run that silently did
not load the runbook looks exactly like one that did:

- The backend must advertise `runbooks.pinned_runbook_ids`. The field is validated with
  `extra="forbid"`, so sending it to a deployment that predates it would 422 the whole
  turn — and withholding it would run unpinned. An unaware or unreachable backend
  therefore **fails the launch before a turn is spent**.
- An id that is not in the advertised catalog fails the launch too, with a near miss
  (`unknown runbook id "daintree.foundatoin"; did you mean "daintree.foundation"?`).
- A backend that accepts pins but advertises no catalog cannot be checked locally. That
  is a `warning`, not a failure, and the server-side codes below are the backstop.
- A pin the backend accepted but could not honour arrives as an ordinary `warning` event
  carrying its code: `unknown_runbook_id_ignored`, `pinned_runbook_not_executable` (the id
  exists but is outside this profile's menu), or `pinned_runbook_over_cap`. Each is
  reported once per session — the pin list cannot change mid-conversation, so repeating
  it every round would only bury the tool activity.

A pinned runbook never narrows the tool inventory. It rides alongside the backend's own
selection rather than replacing it, so the turn still loads whatever else it needs.

Four rules worth knowing before you script against them:

- **You need no key.** The backend holds its own upstream credential and funds every
  model call from it, so a headless run works with nothing but a prompt. `--api-key-file`
  and `DAINTREE_API_KEY` do NOT change that: they supply a bearer that says who is
  CALLING, which a deployment with accounts configured will verify and one without will
  ignore entirely. Neither pays for anything. If a deployment does require an account,
  sign in interactively once (`daintree-assistant auth login`) and headless runs on that
  machine pick the session up from the keychain — there is no headless login flow,
  because the authorization code arrives through a browser. See
  [`auth status --json`](#auth-status---json) for reading account state from a script.
- **The key never rides argv.** `ps` is world-readable, so `--api-key-file` takes a
  path. The file is read with a bounded read (a FIFO would otherwise defeat
  `--timeout`) and checked with the same shape rule `DAINTREE_API_KEY` gets, so a stray
  newline fails locally instead of as an opaque header error every turn.
- **A named key that cannot be read is FATAL, never a fallback.** Both a missing file
  and `--api-key-file=` — what a harness produces when it expands an unset variable —
  fail at the argument boundary. Falling through to an anonymous request would run the
  turn as a different principal and hide the mistake behind a successful-looking run.
- **An explicitly false boolean wins.** `--auto-approve=false` beats
  `DAINTREE_ASSISTANT_AUTO_APPROVE=1`. An *absent* flag leaves the env in charge.

### `auth status --json`

`auth status --json` writes exactly one NDJSON line to stdout and nothing else; human
text goes to stderr, so a stray sentence can never corrupt the stream. The line is a
versioned event whose `data` is the redacted account snapshot:

```json
{"v":1,"type":"auth:status","environment":"staging","data":{
  "state":"signed_in_active","authenticated":true,
  "environment":"staging","backendUrl":"https://assistant.daintree.org",
  "configured":true,"authRequired":false,
  "email":"person@example.com","subjectHash":"0123456789abcdef",
  "planId":"standard","entitlementSource":"polar",
  "entitlementCheckedAt":"2026-08-25T12:00:00Z",
  "lastVerifiedAt":"2026-08-25T12:04:11Z",
  "storageTier":"keychain",
  "links":{"account":"https://staging.daintree.org/account"},
  "authRevision":"3f9a1c02b7e45d18:7"}}
```

Most fields are `omitempty`, so read the example as "what a fully-populated line looks
like" rather than a fixed shape. `entitlementStale` is absent when false; `email`,
`planId` and `entitlementSource` are all optional in the backend contract and absent when
it does not report them; `links` is always present and may be `{}`.

Four properties a consumer has to respect:

- **`state` is an OPEN domain.** New values are added whenever the account layer learns
  to tell apart two situations it used to collapse, and two arrived at once for exactly
  that reason. A consumer that switches without a default, or validates `state` against a
  closed enum, breaks on the next one. Branch on the coarse fields — `authenticated`,
  `configured`, `authRequired` — and render `state` as an opaque string.
- **`configured` and `authRequired` are POINTERS, and absent means "we could not ask".**
  A bare `false` would decode as "this deployment has no accounts", which during an
  outage tells someone their sign-in is unnecessary. Absent is a third answer; treat it
  as unknown rather than as no.
- **The account fields are only populated after a live check.** They come from
  `--refresh`, which makes the one request to `/v1/daintree/account`. A plain read is
  offline-capable and reports what the process already knows, so a fresh process shows
  `state: signed_in_unverified` and no plan until something asks. Nothing about the plan
  is persisted — deliberately, since a plan on disk is a plan that can be wrong.
- **Nothing here is ever a credential.** There is no field that can hold an access token,
  a refresh token or an authorization URL, the subject appears only as a one-way hash,
  and a backend URL carrying userinfo is redacted before it reaches the line.

Two timestamps, answering different questions. `lastVerifiedAt` moves whenever ANY
protected request succeeds, so it says "this login still works". `entitlementCheckedAt`
is the backend's own `checked_at` for the billing answer and moves only when the account
endpoint is asked. A session confirmed a second ago can therefore sit beside an hour-old
plan, which is exactly the pair `entitlementStale` exists to qualify.

A retained snapshot survives a later failure on purpose: if a `--refresh` cannot reach the
backend, the previous answer's plan fields stay on the line rather than blanking, because
blanking would report a subscription as gone when the network was. Branch on `state` and
`lastErrorCode`, and read `entitlementCheckedAt` for age — never treat the presence of a
plan as proof it was just checked.

Exit codes: `0` for any answer the command could construct, including a missing plan, a
lapsed plan, a refused client and a dependency outage — none of those is fixed by signing
in. `3` means signed out or revoked, and is the only code a script should react to by
prompting for a login. `1` and `2` are still ordinary command failures (a manager that
could not be built, bad arguments or configuration) and carry no status line.

`auth login --json` emits a multi-line event stream — `auth:starting`, then
`auth:browser_opened` or `auth:manual_url_required`, `auth:waiting`, `auth:authenticated`
— and closes a successful sign-in with the same `auth:status` line, on every plan outcome
including a failed check. When the check failed, the event's top-level `code` names the
reason; the status payload itself carries no `lastErrorCode` for it, because the
post-login check is deliberately non-mutating and does not record anything against the
session. Cancellation, a deployment with no accounts, and a genuine sign-in failure end on
`auth:cancelled`, `auth:not_offered` and `auth:error` respectively, with no status line.

### Isolation

A harness should never touch the developer's real state. `--state-dir` relocates the
database, the artifacts and the owner lease, so an isolated run shares nothing with a
attached session the developer has open. Nothing else needs supplying against a backend
that asks for no account — which is every deployment today. Note that `--state-dir` is
also the account boundary: a login lives under the state ROOT, so an isolated run does
NOT inherit the developer's session, and against a deployment that required one it would
have to sign in under that directory itself.

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
| `runbook:loaded` | `titles[]` | the backend's selector loaded runbooks (early cue, not authoritative) |
| `runbook:decision` | `active[]`, `newlyLoaded[]`, `selector` | the committed runbook outcome for a round |
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

`runbook:decision` is what a runbook test asserts on. It is emitted once per round that
reaches committed metadata — **including rounds where nothing new loaded**, so the active
set is reported even when it did not change:

```json
{"type":"runbook:decision","ts":1787300000002,"seq":9,
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

`runbook:loaded` still fires, unchanged, and remains titles-only. Its value is *timing*: it
is the only runbook signal available before the upstream model connects. It is **not**
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
starting condition rather than a whole-run guarantee. A bound MCP token is long-lived
(no fixed TTL) but can still be REVOKED mid-run (e.g. Daintree closing), so
a long harness run can begin connected and end otherwise for that reason rather than
expiry on a clock.

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

The *first* stage of the bound is cooperative: teardown joins any in-flight scheduler
tick before closing the database, so a transport that ignored cancellation could hold the
graceful exit open. That is the deliberate trade — returning while a tick still held the
store would be a data race.

The second stage is not cooperative. The same watchdog described above applies here:
`--timeout` plus `domain.HardTimeoutGrace` (30s), armed before any setup and still armed
through teardown, kills the process with exit `4`. So `--timeout` **is** a guaranteed
upper bound on process lifetime, and the graceful path is what decides whether you exit
`0`/`1`/`2` with a terminal `result` line or exit `4` with none.

The precise ceiling is `timeout + 30s + 2s`: the watchdog gives its own stderr diagnostic
up to two seconds to write before calling `os.Exit`, because stderr can be a pipe nobody
is draining and a blocking write there would have turned the watchdog into one more way
to hang.

Durable async rows may remain live for the next owner in either case; the invocation
itself always ends.

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

## Runbook-authoring test loop

Developing a runbook has two halves that fail differently, and the loop below exists to
keep them apart. The **backend** owns runbooks — it loads them from disk, selects them, and
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
(A direct `DAINTREE_RUNBOOKS_OVERLAY_DIR=… python -m daintree_assistant_server` does honour
the prefix — process environment outranks dotenv — but `./dev` is the usual way in.)

```ini
# ../assistant-backend/.env
DAINTREE_RUNBOOKS_OVERLAY_DIR=/tmp/runbook-harness/runbooks
```

Then, from a first terminal, create the directory and start the server — leave it running:

```bash
mkdir -p /tmp/runbook-harness/runbooks
cd ../assistant-backend && ./dev
```

It binds the `HOST`/`PORT` from `.env`, which is `127.0.0.1:8473` only if you started from
the scaffold. Empty is the deliberate starting state: the overlay is loaded during startup,
so a runbook that fails to parse there is a **readiness** failure rather than a reload
failure — the server boots but stays unready, and the reload route below is
readiness-gated, so it answers `503` and cannot dig you out. Boot empty and the first draft
is recoverable without a restart.

**2. Write the runbook.** The filename stem MUST equal the frontmatter `id` — the loader
passes `path.stem` as the expected id and rejects the file otherwise — so this one goes to
`/tmp/runbook-harness/runbooks/daintree.example.runbook-under-test.md`. Only top-level `*.md`
files in the directory are read; subdirectories are ignored.

```markdown
---
id: daintree.example.runbook-under-test
title: Runbook under test
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

An overlay runbook whose `id` matches a packaged one **replaces** it; a new id is **added**.
That is what lets you iterate on a shipped runbook and develop a new one with the same
mechanism, without editing the server's tree either way. Give the overlay its own
directory: pointing it at the packaged one is a no-op, and a path that exists but is not a
directory fails the load outright.

**3. Reload, from a second terminal.**

```bash
curl --fail-with-body -sS -X POST http://127.0.0.1:8473/v1/daintree/runbooks/reload
```

The **path** is captured once at startup, so moving the overlay needs a restart; its
**contents** are re-read here, which is the whole point — edit, POST, run, without losing
the process. Once the server is ready this is validate-before-swap: the entire replacement
catalog is built and checked before it rebinds, so a runbook caught half-written answers
`409` and leaves the previous catalog serving. Fix the file and POST again; from that
point on a malformed runbook costs a request, not a restart.

Both the variable and the route are development-only by construction: the route is mounted
only when an overlay is configured on a non-hardened server, and a hardened deployment that
sets a non-empty overlay value fails its readiness check. There is no way to reach this
loop against production, which is why it can be this convenient.

**4. Run the CLI against it, pinned and isolated.** List first — a reload that did not
take, or an id that does not match its filename, is cheaper to find in a catalog read than
in a spent turn:

```bash
daintree-assistant --backend-url http://127.0.0.1:8473 --list-runbooks --json
```

Then pin the runbook. `--runbook` is what removes the ambiguity that makes runbook
development frustrating: without it, a bad turn could mean a bad runbook *or* a selector
that never picked it. See [Naming a runbook](#naming-a-runbook---runbook-and---list-runbooks) for
pin ordering, the preflight, and the warning codes a backend can answer with — a pin rides
alongside the backend's own selection and can still be refused, which is why step 5 reads
back what was active rather than assuming.

```bash
mkdir -p /tmp/runbook-harness/case-001/run-01
echo "the prompt this case exercises" > /tmp/runbook-harness/case-001/prompt.txt

daintree-assistant \
  --backend-url http://127.0.0.1:8473 \
  --runbook daintree.example.runbook-under-test \
  --prompt-file /tmp/runbook-harness/case-001/prompt.txt \
  --state-dir /tmp/runbook-harness/case-001/run-01/state \
  --project-id runbook-harness-case-001 \
  --json > /tmp/runbook-harness/case-001/run-01/run.jsonl
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

**5. Read the transcript.** `runbook:decision` is the committed answer to "which runbook was
active", and `selector.degraded` is the field to gate on alongside it — see
[The event stream](#the-event-stream) for why, and for the full shape.

```bash
cd /tmp/runbook-harness/case-001/run-01

jq -c 'select(.type == "runbook:decision")
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
bug in the prompt, runbook, or tool schema — not a dumb model).
