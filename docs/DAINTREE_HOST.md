# Daintree host — the `host --stdio` embedding contract (protocol v4)

This is the **host-side companion** to [`DAINTREE_MCP.md`](DAINTREE_MCP.md). That doc
describes the protocol this CLI speaks *to* Daintree (the tool catalog, tiers, call shapes).
This one describes the protocol Daintree speaks *to* the CLI: how the desktop app launches
the binary, hands it a session, drives turns, answers approvals, and tears it down.

> **The binary is headless.** There is no terminal UI package and none may be added. Daintree
> owns rendering and draws the conversation in React; this process emits structured events and
> nothing else. If a turn needs to *say* something new, that is a new **event on this
> protocol** — never a new thing drawn here. The retired PTY/cockpit embedding is archived at
> [`history/PTY_HOST_V1.md`](history/PTY_HOST_V1.md); nothing in it is a live contract.

> **Source of truth.** The wire contract is
> [`internal/host/wire.go`](../internal/host/wire.go) (version, vocabularies, command
> parsing) and [`internal/host/events.go`](../internal/host/events.go) (every event's exact
> JSON shape). Those files win over this document. The env contract the CLI actually consumes
> is [`internal/config/config.go`](../internal/config/config.go). Daintree internals named
> here can drift; the **observable contract** — frame shapes, event names, env var names,
> error codes — is what to rely on.

---

## `host:ready` — the session facts a host renders (protocol v4)

The first frame of a session is `host:ready`. Beyond `protocolVersion`, it carries the
facts a host needs to state what this session *is* — the same set the CLI's own masthead
stated, and for the same reason it exists at all: **a protocol-only consumer never reads
stderr**, so anything mentioned only there is invisible to an embedding host.

| Field | Meaning | Absent when |
|---|---|---|
| `version` | Engine build string (distinct from `protocolVersion`) | never sent empty |
| `autoApprove` | This session runs mutating tools with **no** confirmation | always present (boolean) |
| `tier` | Permission tier in force (`supervisor` / `operator` / `system`) | unset |
| `tierGloss` | Plain-language reading of `tier` | unknown tier |
| `backend` | The backend endpoint this session talks to, named and sanitized | only when it cannot be safely rendered (see below) |
| `routing` | A **non-default** endpoint-routing policy, as one line | the default policy |
| `logFile` | Absolute path of this session's debug log | debug logging is off |

Two rules hold for all of them:

- **The engine resolves them, not the host.** Each is a policy judgement that depends on
  constants this module owns — which backend URL is "the deployed one", what the local
  endpoint is called, which routing policy is default, what a tier permits. A host that
  re-derived them would need a second copy of all of that, wrong the first time any of it
  changed. See [`internal/host/masthead.go`](../internal/host/masthead.go).
- **Absent means "the default, which needs no announcement"** for `tier`, `tierGloss` and
  `routing`: only a *deviation* is reported, so a host can render these unconditionally
  and stay quiet in the common case. `logFile` is different — absent means logging is off.

  **`backend` is different again, and a host must not read its presence as "a custom
  endpoint".** It names the endpoint on every session, deviation or not. It once followed
  the deviation rule, on the reasoning that the deployed backend is what every install
  talks to; that inverted when the endpoint became the session's own, remembered across
  restarts and switchable with `/backend`. The deployed one is now what an unconfigured
  install *arrives at*, and it is the one that sends the conversation, the project context
  and every tool result off the machine — so announcing only the exception made those two
  cases identical on screen, and the silent one was the one that left the box.

  It can still be absent, in one case: an endpoint that cannot be safely rendered
  sanitizes to empty and is omitted rather than shown. That is a misconfiguration (a
  value with no host, for instance), not a default — read an absent `backend` as
  "unknown", never as "the deployed one".

`logFile` in particular cannot be worked out from outside: the engine picks the filename
(`<date>-<sessionId>.log`), so a host that guessed it would show a path that does not
exist the first time that format changes.

## TL;DR

- Daintree spawns `daintree-assistant host --stdio` and talks **NDJSON over stdio**: one JSON
  object per line.
- **stdin** carries the inbound stream: the first line is a `SessionDescriptor`, every
  subsequent line is a `HostCommand`. **stdout** carries `HostEvent` frames and *nothing*
  else. **stderr** carries human diagnostics and *never* protocol JSON.
- Every outbound event carries `type`, `sessionId`, and a **monotonic `seq` starting at 1**.
- `PROTOCOL_VERSION` is **3**, exported as `host.ProtocolVersion`. It is the single source of
  truth: `docs/generated/COMPATIBILITY.md` is projected from it, and a test
  (`TestDocsNameTheCurrentHostProtocolVersion`) fails the build if prose disagrees.
- **No secret crosses stdin.** The MCP bearer and endpoint arrive via environment only, so a
  leaked descriptor line cannot re-bind the session or carry the token.

---

## 1. Why v3 is a break, not an increment

v2 described a **terminal session** for a parent that drew a thin activity strip beside an
xterm. v3 describes a **conversation** for a parent that renders the whole thing. Three
changes make it breaking:

1. **Every event carries `seq`.** v2 silently dropped frames when the writer queue filled,
   with no way for a consumer to notice — unusable once the transcript *is* the product. v3
   applies backpressure to stream traffic instead, and `seq` makes any residual gap
   detectable rather than invisible.
2. **`turn:end` carries the authoritative final `content`.** v2 carried only an outcome
   class, so a lost token frame corrupted the conversation forever with no way to repair it.
3. **The event set covers what the runtime actually produces** — phase, reasoning,
   interjections, the whole tool batch, tool state and progress, usage, cost, notices —
   instead of the subset a status strip needed.

---

## 2. Launch and handshake

### The command line

```
daintree-assistant host --stdio
```

cwd is the **project root**. Connection wiring is entirely environment; there are no
connection flags on the launch line.

### Environment Daintree injects

These are read at the **trusted-env boundary** in `internal/config/config.go`: taken from the
real process environment (or the assistant's own `.env`), **never** from the bound project's
`.env`, so a checked-out repository cannot spoof the link or the identity.

| Variable | Value | Read? |
| --- | --- | --- |
| `DAINTREE_MCP_URL` | `http://127.0.0.1:<port>/mcp` — Streamable HTTP. `<port>` is the *actually bound* port (default 45454, walks up to 10 on conflict). **Never hard-code the port.** | **Yes** → `cfg.McpURL` |
| `DAINTREE_MCP_TOKEN` | Per-session bearer, sent as `Authorization: Bearer <token>`. Long-lived once bound to this session (no fixed TTL) — Daintree's `HelpSessionService` only reaps an *unbound* provisional bearer that never got attached, after ~30 minutes. A bound bearer instead gets REVOKED on specific events (e.g. Daintree closing); the supervisor distinguishes that from a transient outage and waits for a fresh credential rather than retrying blindly. | **Yes** → `cfg.McpToken` |
| `DAINTREE_PROJECT_ID` | The bound project's id. Scopes `StateDir` to `~/.daintree/assistant-cli/<project>/state.db`, and is stable across launches — safe to key per-project memory on. | **Yes** → `cfg.ProjectID` |
| `DAINTREE_WINDOW_ID` | Launching window id. **Informational**; the enforceable binding is server-side. | **Yes** → `cfg.WindowID` |
| `DAINTREE_ASSISTANT_AUTO_APPROVE` | `"1"` when the user turned off permission prompts. Reported back on `host:ready`. | **Yes** → `cfg.AutoApprove` |
| `DAINTREE_ASSISTANT_DEBUG_LOG` | `"1"` when the user enabled debug logging. | **Yes** → `cfg.DebugLog` |
| `DAINTREE_ASSISTANT_SCRATCH_DIR`, `DAINTREE_PANE_ID`, `DAINTREE_CWD`, `DAINTREE_WORKTREE_ID` | Host-provided metadata. | **No** — injected but not consumed. `DAINTREE_WORKTREE_ID` deliberately so: it is the worktree the assistant PANEL was created in, and the panel outlives any number of worktree switches, so by the time a turn runs it may name a worktree the user left hours ago. The live answer comes from MCP (below). |

Daintree strips inherited `DAINTREE_*` variables from the shell environment before injecting
its own, so a stale token in the user's OS environment cannot leak in.

### The first line: `SessionDescriptor`

```json
{"sessionId":"ses_…","windowId":7,"projectId":"proj_…","cwd":"/path","tier":"system","protocolVersion":3,"resumeSessionId":"ses_prev"}
```

`sessionId`, `projectId`, `cwd` and `tier` must be strings; `windowId` and `protocolVersion`
must be **numbers**. A quoted number is a string and is rejected. `protocolVersion` must be
integral — a fractional `2.9` is refused rather than truncated past the `== ProtocolVersion`
check. `resumeSessionId` is optional but **typed**: absent or `null` starts a fresh session, a
non-string is rejected, and it is capped at 256 **bytes**. It used to swallow a type
mismatch, which silently turned a resume request into a fresh session with no indication
the request had been discarded.

**The descriptor carries no secret, deliberately.** `windowId` / `projectId` / `tier` are
validated here *and* cross-checked against the environment, so a descriptor that disagrees
with the live binding is refused rather than honoured — and it can never hand the session
a token, because tokens only arrive by environment.

**`cwd` is the exception, and it is worth being exact about it.** It is not merely
declared: it *becomes* the runtime's project path. So a descriptor written on this pipe
does choose which directory the session operates in, and there is no second source to
check it against. That is bounded by who can write the pipe — stdin of a child process,
which only its parent holds — and a writer that can forge a descriptor already controls
the process. But the honest statement is that `cwd` is descriptor-controlled, not that the
descriptor cannot re-point the session.

A malformed descriptor yields `host:error` with code `bad-descriptor`, followed by teardown.
A version mismatch yields code `protocol-mismatch` naming both versions, then teardown. A
descriptor whose `projectId`, `windowId` or `tier` disagrees with the effective
environment yields `binding-mismatch` — the descriptor is what Daintree *believes* it
opened and the environment is what the runtime actually *uses*, and nothing compared them
before, so the two could disagree while both sides reported success. All three errors are
emitted **synchronously and flushed**, so the specific one reaches the parent before the
`host:shutdown` reason does.

`cwd` is deliberately *not* cross-checked. There is no independent second source for it:
the descriptor's `cwd` **is** the project binding this process resolves from, so comparing
the two would compare a value against itself. A check that can never fire is worse than no
check, because it makes the handshake look more self-auditing than it is. The other three
fields are stated twice — once in the descriptor, once in the environment — which is what
makes them checkable.

### The reply: `host:ready`

```json
{"type":"host:ready","sessionId":"ses_…","seq":1,"protocolVersion":3,"autoApprove":false,"version":"1.4.2","backend":"https://assistant.daintree.org","resumedSessionId":"ses_prev"}
```

`version` is the engine build string, so a host no longer has to shell out to `--version`
separately. `autoApprove` reports that this session runs mutating tools with **no**
confirmation — previously that state was mentioned only on stderr, which a protocol-only
consumer never reads, leaving a user unable to see that approvals were switched off.

---

## 3. Inbound commands

Every command is an object with a string `sessionId` and a string `type`.

| `type` | Extra fields | Meaning |
| --- | --- | --- |
| `prompt` | `text` | Start a user turn. Sent while a turn is running, it is **folded into** the in-flight turn as an interjection rather than rejected. |
| `approval:decide` | `approvalId`, `decision` | Answer a parked confirmation. |
| `question:answer` | `questionId`, `choiceIndex` | Answer a multiple-choice question. `choiceIndex` is 0-based; **negative means dismissed**, which fails the tool call as declined rather than answering it — distinct from the turn being cancelled underneath it, see §8. Required and must be a number — a missing one would default to 0 and silently answer "the first option" for a user who never chose. |
| `command` | `line` | Run a slash command the host's own composer resolved — the raw line, e.g. `"/account"`. An accepted command produces exactly one `command:result`; a REFUSED one produces a `host:error` and no result (see below). Commands are not conversation: sending `/status` as a `prompt` produces an answer about the *word* status, spends a turn doing it, and leaves the user believing they ran something. |
| `operations` | — | Ask for the current operations reading. Answered with one `operations:snapshot`. A **poll, not a subscription** — pushing every store change to a host that may not be showing the deck is a great deal of traffic for a view nobody is looking at. |
| `timers` | — | Ask for the scheduled-timer list alone. Answered with one `timers:snapshot`. Also a poll. Separate from `operations` because a timer manager refreshes on its own cadence and must not drag six unrelated deck sections along with it; the rows are identical, so the two surfaces cannot disagree. **v4.** |
| `timer:cancel` | `timerId` | Retire one timer **on the user's behalf** and revoke the automation grants scoped to it. Answered with exactly one `timer:cancelled`, success or failure. The engine does NOT ask for confirmation — see [who confirms a timer cancel](#who-confirms-a-timer-cancel). A blank `timerId` is refused as unparseable (dropped). **v4.** |
| `interject:retract` | — | Take back the most recent buffered interjection (LIFO). Answered with `interject:retracted`, which reports whether anything was actually taken back. |
| `interrupt` | — | Cancel the running turn. Leaves autonomous wake work alone. |
| `hibernate` | — | Graceful teardown, reason `hibernate`. |
| `shutdown` | — | Graceful teardown, reason `exit`. |

### Who confirms a timer cancel

Nobody, on this side. `timer:cancel` does **not** raise an `approval:requested`, and a
host must not wait for one.

The approval channel exists for a specific situation: the *model* has decided to do
something and a human is asked to allow it. A timer cancel is the other way round. The
human is looking at a list the host drew, has picked one row out of it, and the host has
already put a confirmation in front of them naming that timer and what cancelling it
withdraws. Routing it back through an approval would ask the same person the same
question a second time, about an object the engine would have to describe worse — it has
the id, the host has the row on screen.

So the split is: **the host owns the confirmation, the engine owns the operation.** The
engine's half is the part a host cannot do — retiring the schedule row and revoking the
automation grants scoped to that timer as one operation, and recording it in the audit log
under actor `user`, which is what keeps "the person cancelled this" distinguishable from
"the assistant decided to" when someone reads the session back.

The mutation is idempotent enough to be safe to retry: cancelling an already-retired timer
reports `alreadyInactive` with `cancelled: false` rather than pretending, and the grant
sweep runs either way (a fire that died between claiming its timer and revoking its grant
leaves live authority with nothing left to spend it).

`decision` is coerced to the closed vocabulary `approved | rejected | timeout`. **An
unrecognized value becomes `rejected`** — an unparseable decision must never be treated as an
approval, and the parked dispatch unblocks declined rather than emitting an off-contract
decision.

A line that is not a recognizable command — unknown `type`, missing required field, garbled
JSON — is **silently dropped**, never turned into an error. A foreign writer on the pipe
cannot make the session emit protocol errors.

---

## 4. Outbound events

Every frame carries `type`, `sessionId` and `seq`. `seq` is monotonic from 1 across the whole
session; a consumer that sees a gap knows it lost something.

| Event | Payload |
| --- | --- |
| `host:ready` | `protocolVersion`, `autoApprove`, `version?`, `backend?`, `tier?`, `tierGloss?`, `routing?`, `logFile?`, `resumedSessionId?` — see the masthead table above for what each absence means |
| `host:error` | `code`, `message` |
| `host:shutdown` | `reason` (`hibernate`/`revoke`/`error`/`exit`), `resumeSessionId?` |
| `turn:start` | `turnId`, `role` (`user`/`assistant`), `startedAt` |
| `turn:phase` | `phase` — the canonical `domain.RunPhase` |
| `turn:token` | `turnId`, `chunk` — streamed prose, for liveness only |
| `turn:reasoning` | `turnId`, the round's thinking |
| `turn:interjection` | `text`, `turnId?` — a message typed *while* the turn ran, emitted at the moment the loop folded it into history |
| `interject:retracted` | `retracted`, `text?` — the answer to `interject:retract`. `text` is present only when `retracted` is true, echoed verbatim so the composer gets back exactly what was typed |
| `turn:end` | `turnId`, `endedAt`, `outcome?`, `content?` |
| `tool:batch` | `calls` — the whole batch announced as queued, before sequential dispatch |
| `tool:started` | `toolCallId`, `toolId`, `argsSummary`, `startedAt`, `danger`, `turnId?` |
| `tool:state` | `toolCallId`, `state` — promotes one announced call through its lifecycle, `waiting` included: a mutating call parks there for the whole time it is blocked on the user's approval. It returns to `active` when the user APPROVES; a decline or a confirm error settles it `failed`, and an interrupt settles it `cancelled` |
| `tool:progress` | `toolCallId`, `message` — an in-tool substep, and ONLY that. The engine's own lifecycle (validating, parked for approval, running) never comes down this channel: it is `tool:state`, and a host that drew a progress beat as a caption under a row it had already labelled was printing the row's status twice |
| `tool:settled` | result classification, severity, `turnId?`, `asyncId?` |
| `approval:requested` | `approvalId`, `toolId`, `summary`, `requestedAt`, `needsTypedConfirm`, `riskClass?`, `consequence?`, `argsSummary?`, `turnId?` |
| `approval:decided` | `approvalId`, `decision`, `decidedAt` |
| `question:requested` | `questionId`, `question`, `options[{label,text}]`, `default`, `requestedAt`, `turnId?`, `toolCallId?` |
| `question:answered` | `questionId`, `choiceIndex`, `cancelled`, `answeredAt`, `label?`, `text?` |
| `command:result` | `command`, `text`, `conversationCleared` (**always present**), `quit?`, `unknown?`, `turnId?` — the answer to one inbound `command`. See [the clear rule](#a-host-must-never-infer-the-clear-from-the-command-text) |
| `operations:snapshot` | `inbox[]`, `workflows[]`, `agents[]`, `async[]`, `timers[]`, `audit[]` — the answer to one inbound `operations`. Row shapes below |
| `timers:snapshot` | `timers[]` (the same row shape as `operations:snapshot`), `takenAt`, `readFailed` — the answer to one inbound `timers`. **`readFailed` is not cosmetic**: the operations deck is best-effort and drops a section it could not load, but a timer manager must never render a failed read as "nothing scheduled", because that is a claim a user acts on by walking away from work that is still queued. An empty `timers[]` means nothing while `readFailed` is true |
| `timer:cancelled` | `timerId`, `cancelled`, `alreadyInactive`, `priorStatus`, `revokedGrants`, `grantRevokeFailed`, `error` — the answer to one inbound `timer:cancel` |
| `mcp:status` | `connected`, `toolCount?`, `error?` — whether the Daintree control plane is reachable and how many tools it offers |
| `usage` | per-round token accounting |
| `cost` | `total`, `complete` |
| `notice` | `level`, `message` |
| `model:rate-limited` | emitted when the provider throttled us after the retry budget |

### A slash command can be refused, and then there is no `command:result`

`/login`, `/logout` and `/account` are SLOW — `/login` opens a browser and then waits up to
five minutes for a loopback callback — so they run on a worker instead of blocking the
single-threaded command loop, which would otherwise accept no interrupt, no approval and no
quit for those five minutes. Only one runs at a time: a second slow command arriving while
one is still deciding is **rejected**, answered with a `host:error` carrying code
`command-busy` and no `command:result`. Queuing them instead would settle the two in
whichever order the network returned rather than the order they were typed, and a `/logout`
sent after a `/login` landing first leaves the session signed in. A command arriving during
teardown is refused the same way.

So a host must clear its pending-command state on `command:result` **or** a `host:error` —
binding it to the result alone leaves a refused command spinning forever.

### `mcp:status` is a different question from "is the engine up"

`host:ready` says this process is running; it says nothing about whether it can *do*
anything. A session that answers questions but cannot spawn an agent, under a status line
reading "Connected", is the most misleading state this protocol can produce — so control-plane
reachability is carried separately. `toolCount` is **omitted** until the catalog has actually
been fetched, which is not the same as a catalog of zero tools, and `error` names the reason
when `connected` is false.

It is emitted at boot and again after every completed slash command — `/reconnect` exists
precisely to change this, so re-reporting beats leaving a stale reading on screen. It is
**not** pushed by the engine's own mid-session reconnect, so a host holding a
`connected:false` that wants a current answer should send a command rather than wait.

### `operations:snapshot` row shapes

Six arrays. Every `at` / `startedAt` / `dueAt` is a Unix millisecond timestamp. The arrays
are always present, empty rather than omitted, and every field of a row is always present —
a row is a fixed shape, so an empty string means "the engine does not have this", never "an
engine too old to say".

| Array | Row fields | Contents |
| --- | --- | --- |
| `inbox` | `id`, `severity`, `source`, `summary`, `at` | the attention queue, severity `attention` and above |
| `workflows` | `id`, `goal`, `status`, `progress`, `next`, `blocked` | open execution graphs — **the engine populates none today**; the shape is fixed so a host can bind to it before the producer lands |
| `agents` | `id`, `title`, `goal`, `badge`, `agentState`, `preview`, `startedAt`, `needsAttention` | live watchers. `badge` and `preview` arrive empty: the cockpit merged a watcher with its terminal's tail, but that needs a live MCP read, so it is left to the host, which already has the terminal on screen |
| `async` | `id`, `title`, `tool`, `startedAt` | live async futures |
| `timers` | `id`, `label`, `dueAt`, `createdAt`, `payloadKind`, `toolName`, `runCount`, `repeatEveryMs`, `repeatMaxRuns`, `repeatUntilAt`, `targetWorktreeId`, `targetTerminalId`, `liveGrants`, `grantsUnknown` | scheduled timers. `payloadKind` is `reminder` \| `tool_call` \| `legacy`; `toolName` is set only for `tool_call`. `repeatEveryMs` 0 means one-shot, and `repeatMaxRuns` / `repeatUntilAt` 0 mean unbounded on that axis. `liveGrants` is how many automation grants this timer can still spend — what lets a cancel confirmation state its real consequence — and is meaningful only while `grantsUnknown` is false. `grantsUnknown` exists because a count of 0 cannot say whether a timer holds no authority or whether the engine failed to find out, and that difference is quoted on a destructive confirmation: a host must say it does not know rather than assert there is nothing to revoke. The stored payload's reminder text and tool ARGUMENTS are deliberately absent: neither is needed to decide whether to cancel a timer, and both are model-written free text that would otherwise cross into a renderer |
| `audit` | `tool`, `outcome`, `durationMs`, `at` | the 8 most recent tool calls, newest first |

Free text is redacted (`redact.String`) on the fields most likely to carry a credential —
`summary`, `goal`, `title`, `label`, and above all the agent `preview`, which is whatever the
agent last printed. It is **not** blanket coverage: `progress` and `next` go out raw, so do
not treat a snapshot as a scrubbed document.

These are data, not drawn lines: the engine stays the single source of the facts and the host
decides presentation.

### `turn:end` is the authority, not the token stream

A consumer accumulates `turn:token` for liveness, then **replaces its buffer** with
`turn:end.content`. That is what makes the transcript repairable: a token frame lost to a
wedged stdout, a mid-stream reconnect, or the consumer's own dropped update self-heals rather
than leaving mangled prose on screen forever.

`content` is **omitted**, not empty, when the turn produced no visible text — a cancel before
the first token, or a tool-only round — so a host can tell "nothing was said" from "the answer
was empty".

### `needsTypedConfirm` is always present

It is the safety layer's own verdict that an action is irreversible and must not be
approvable by a single click. It is carried explicitly rather than left for the host to
re-derive from `riskClass`: a host that reimplements "which risk classes are irreversible"
has forked a security rule into a second codebase, where it can drift silently and in the
permissive direction. It is emitted even when false, so a host can tell "does not need typed
confirmation" from "this peer is too old to say".

---

## 5. Framing, backpressure and ordering

- **One serialized writer.** A single dedicated goroutine drains the outbound queue, so
  frames can never interleave.
- **Frame cap: 4 MiB** inbound and outbound (`maxFrameBytes`). An oversize inbound line is
  refused rather than buffered without bound.
- **Backpressure, not silent drops.** Stream events (tokens, tool progress) *wait* when the
  queue is above the high-water mark, and a wedged consumer eventually sheds them — with
  the sequence number still burned, so the hole is detectable. Terminal and decision
  frames — `turn:end`, `approval:requested`, `host:shutdown`, command acks — **never
  wait**: they take the whole queue depth, which is the reserve that stops progress
  traffic from starving a result.

  They are not promised to *never drop*, because with a finite queue and an unread pipe
  that is not implementable — something has to give. The honest guarantee is stronger
  than the old wording anyway: **a critical frame is never silently discarded.** If one
  cannot be delivered, the session FAILS — the host treats the parent as gone, tears down,
  and reports `host:shutdown` with reason `error` rather than a clean `exit` — instead of
  continuing with a hole where a turn outcome or an approval request used to be.

  "Critical" means *a host cannot make progress without it*: `turn:end`, `host:ready`,
  `host:error`, `host:shutdown`, and the approval/question request and decision frames.
  Telemetry — usage, cost, notices, reasoning, rate-limit — is not worth a session, so a
  congested consumer drops those with the sequence gap left visible rather than killing a
  healthy run over a stale meter.
- **Outbound frames that would exceed 4 MiB are cut, not dropped.** This only arises for
  `turn:end`, whose `content` is unbounded, and it matters because `turn:end` is the frame
  that says the turn is over: refusing it left the host showing a turn running forever
  over a conversation that had finished. The content is truncated with a visible marker in
  the rendered text, and if even that will not fit, the frame goes out with the marker
  alone — a recoverable loss, unlike never learning the answer exists. The cut is chosen
  by measuring the *encoded* frame, not the raw string, because JSON escaping can inflate
  a payload several times over.

  **Open cross-repository question.** `turn:end.content` is documented as authoritative —
  a consumer replaces its accumulated token buffer with it. When the content was cut, that
  rule makes the transcript *worse* for a host that received every token: a complete
  buffer is replaced by a truncated one. Resolving it properly needs a decision on the
  Daintree side — treat marked truncation as a fallback rather than an unconditional
  replacement, or carry the authoritative content in chunks. Until then, truncation is the
  better default of the two available: a host that lost tokens gets something, and the
  marker tells the reader what happened.
- **`seq` is assigned during encode, under the same lock as the enqueue.** Assigning it
  outside would let two producers interleave — A takes N, is descheduled, B takes N+1 and
  enqueues first — so a consumer would see frames out of order and reasonably conclude one
  was lost.
- A broken stdout means the parent is gone: the host cancels and tears down.

---

## 6. Lifecycle, EOF and shutdown

| Trigger | Reason on `host:shutdown` |
| --- | --- |
| `shutdown` command | `exit` |
| `hibernate` command | `hibernate` |
| stdin EOF / read failure / oversize line | `exit` |
| fatal protocol error | `error` |

**`host:shutdown` is TERMINAL: no frame ever follows it.** Teardown seals the stream
before writing it — the writer stops accepting new frames, whatever is already in flight
is drained in order, and only then is the shutdown frame stamped, so its `seq` is
genuinely the highest of the session. A consumer may stop reading at that line and know
it has the whole turn. (The drain is bounded; a wedged stdout cannot hold teardown open
forever, and the frame still carries its sequence number so a truncated shutdown is
distinguishable from a clean one.)

**`host:shutdown` is emitted FIRST**, before the app tears down, so Daintree learns the reason
even if teardown itself hangs. Teardown then drains pending approvals (rejecting them so no
dispatch is left parked), cancels in-flight turns, and **joins them under a bounded timeout**
(`defaultTurnJoinTimeout`, 5s). A tool that ignores cancellation cannot wedge shutdown — the
host proceeds. An unexpected EOF is therefore *not* ambiguous: it cancels interactive waits,
persists what it safely can, and exits under a deadline.

Ownership then passes to the persistent supervisor where one is available — see
[`SUPERVISOR.md`](SUPERVISOR.md). Watchers, async futures, timers and the attention inbox are
project-scoped and survive; they are adopted by the next owner rather than torn down.

---

## 7. Identity, binding and the two terminal errors

Project scope is carried by `DAINTREE_PROJECT_ID` and, more importantly, by the per-session
bearer bound to that project. **Worktree identity is not in env or cwd** — query it over MCP
(`actions.getContext` / `worktree.getCurrent`).

It is **pinned per TURN, not per launch**. Daintree resolves an omitted `worktreeId` on
`agent.launch` against its LIVE active-worktree selection, read at the instant the call
lands — right for a palette pick, wrong for a turn: a fan-out of spawns dispatches as a
concurrent cohort, so a human switching worktrees mid-batch would split that cohort across
two worktrees, and a spawn late in a long turn would land wherever they had wandered to.
So the CLI binds the worktree its first round observed and forwards that id EXPLICITLY on
every spawn for the rest of the turn (`internal/tools/worktreepin`). A switch arriving
mid-turn still updates what the model SEES — the runtime block carries the live snapshot
every round — it just no longer moves where that turn's agents LAND. The next turn rebinds.

`SESSION_BINDING_GONE` and `BINDING_STALE` from the MCP side are **terminal**: the bound
window or project is gone. Stop retrying that session and surface it. They are not transient
and no amount of reconnecting fixes them.

The session tier on the wire is **`system`** — Daintree will not tier-block the assistant, so
**this CLI owns confirmation of dangerous operations**. That is what the approval events above
are for.

---

## 8. Decisions: who answers what, on which surface

The runtime has two decision kinds — an **approval** (allow or decline an action) and a
**question** (`user.askMultipleChoice`, choose an answer needed for planning). They are not
available on every surface, and the honest table is:

| Surface | Approval | Multiple-choice question |
| --- | --- | --- |
| Native host (`host --stdio`) | `approval:requested` → `approval:decide` | `question:requested` → `question:answer` |
| MCP (`mcp --stdio`) | `daintree.approvals` / `daintree.approve`, or MCP elicitation | `daintree.questions` / `daintree.question.answer` |
| JSONL one-shot (`--json`) | none — the tool is declined and the turn continues | **not implemented** — `QUESTION_UNAVAILABLE` |
| Line REPL | terminal prompt | terminal prompt |

### Why they are separate frames

An approval is *"may I do this?"* and has one safe default — no. A question is *"which of
these did you mean?"* and has **no safe default at all**: inventing one puts words in the
user's mouth and then acts on them. A host given only the approval frames would have to
render a yes/no sheet for a question with four answers.

That asymmetry drives the rest of the contract: an unanswered approval times out to
*rejected*, while an unanswered question times out to **cancelled**. An out-of-range
`choiceIndex` cancels rather than clamping, for the same reason.

The two MCP cells are also not the same KIND of decision as their native-host neighbours,
and the table should not be read as though they were. On the native host a person sees the
sheet and answers it. On MCP the request goes to the model driving the session, which
answers its own request — delegation, not authorization — and both are available only when
the server was launched to permit it. See
[HEADLESS.md](HEADLESS.md#delegate-is-delegation-not-human-authorization).

MCP questions follow the same rules the native host does, because they are the same
asymmetry: no default answer, a timeout that CANCELS rather than defaults, an out-of-range
index that cancels rather than clamping, questions tied to their `runId` and `toolCallId`,
interrupt and close releasing them, and a parked question waking a long poll. Approvals and
questions stay distinct tools throughout.

### The question channel is not only the model's

`/backend` with no argument reuses it. A slash command whose whole job is "pick one of these" is the same interaction the model gets, and rendering it instead as a numbered block of text is how a host with a real picker ended up showing a terminal menu inside a GUI panel. It goes out as an ordinary `question:requested` — the host needs to know nothing about where a question came from — and the bare form is registered **slow** (`CommandMeta.SlowWithoutArgument`), because the answer arrives as a command on the very loop the command would otherwise be blocking. Inline, that is not a freeze, it is a deadlock. `/backend <target>` is deliberately NOT slow: it resolves and returns, and it reconfigures the endpoint, which has to stay ordered against the turns using it.

The picker RESERVES the session before it opens (`Session.ReserveEndpoint`), so no turn can RUN while the sheet is up: an autonomous wake is deferred rather than attempted (its burst stays queued and the command's own unwind re-drives it), and a typed prompt cannot be submitted because the composer stays disabled through the command's result. Asking first and checking afterwards produces the one outcome a question must never produce: the user makes the decision it asked for and is then told it could not be applied. A picker opened during a running turn is refused before anyone looks at it.

The picker does **not** open when the endpoint was pinned by `DAINTREE_BACKEND_URL` / `--backend-url`: every option would be refused the moment it was chosen, so the command prints the listing that explains the pin instead. Daintree deliberately does not set that variable (it strips an inherited one), so the panel gets the picker.

A local question carries **no `turnId`**. It belongs to no turn, and stamping whatever turn happened to be running would record the answer inside a turn that never asked — a transcript claiming the model was told something it was not.

### One outstanding question at a time

The host draws ONE sheet, so a second `question:requested` would replace the first rather than joining it, leaving whoever asked first parked on a question nobody can see. A second request is therefore **refused** while one is outstanding: the model is told it could not ask and decides without asking, and a command reports that something else is already waiting. Queueing is deliberately not offered — it would hold a dispatch open for an unbounded time behind a decision the user has not been shown yet.

### Dismissed is not cancelled

`question:answered.cancelled` covers both, because the host only needs to know the sheet is gone. The ENGINE distinguishes them, and the model is told which:

| What happened | Reaches the model as |
| --- | --- |
| `question:answer` with a negative `choiceIndex` | `QUESTION_DISMISSED` — the user declined to answer; decide without asking |
| `interrupt`, teardown, or the answer timeout | `QUESTION_CANCELLED` — the turn went away underneath the sheet |

Collapsing the two tells a model that a user who closed a sheet had cancelled the turn, which invites a retry through a route that will be refused the same way.

| | **Terminal transcript** (host scrollback) | **Conversation** (`state.db` history) | **Project state** (memory, workflows, audit, inbox) | **Background supervision** (watchers, async, timers) |
| --- | --- | --- | --- | --- |
| Panel hidden / project switch | survives | survives | survives | runs (attached session owns the lease) |
| Cockpit exits normally (`^C`, `/quit`) | cleared by the host | survives | survives | **continues** — the daemon re-acquires the lease and adopts the live rows |
| Cockpit crashes / PTY killed | cleared by the host | survives | survives | **continues** — flock is kernel-released, so handover needs no cleanup |
| Host **"+ New session"** | dropped deliberately | new conversation; the old one stays in `state.db` | survives | **continues**, and completions land in the attention inbox |
| Daintree app quits | gone | survives | survives | **stops** — the daemon loses the MCP token, so supervision *pauses* with a blocked inbox item rather than fabricating outcomes; it resumes on the next launch |
| Machine sleeps | survives | survives | survives | pauses, then does timer catch-up on wake |
| Machine restarts | gone | survives | survives | **stops**; the next launch adopts the persisted rows |
| `/clear` | wiped (the only scrollback wipe path) — **but only when it actually cleared**, see below | cleared | survives | **cancelled** — `/clear` is the one wholesale teardown |
| `reset project-state` | untouched | cleared | cleared | cancelled |
| CLI upgrade with a schema bump | untouched | moved aside to a timestamped backup, then recreated | same | cancelled with the old DB |
| **Windows** | as above | as above | as above | **never survives attached session exit** — no supervisor on this platform |

#### A host must never infer the clear from the command text

`/clear` is REFUSED while a turn is in flight — clearing history mid-stream would corrupt
the snapshot the turn is still writing into — so the engine answers with a note and keeps
the conversation. The `command:result` event therefore carries the authoritative outcome:

```json
{"type":"command:result","command":"/clear","text":"…","conversationCleared":false}
```

**`conversationCleared` is the only trustworthy signal, and it is always present** (unlike
`quit` and `unknown`, which are omitted when false — an absent field here means an engine
older than this contract, and nothing else). Gate the scrollback wipe on it being `true`.

A host that instead matches the command line wipes its transcript, tool rows and live
state on a clear the engine refused, while the engine goes on working in the conversation
it kept — leaving the user talking to a model whose context they can no longer see, with
the two sides disagreeing about what was said. That is worse than the refusal it misread.
It is the same field and the same JSON key the `--multi-turn` surface uses (`docs/HEADLESS.md`).

The one-time **"While you were away"** notice (`App.AttachSummaryLines`, consumed on read)
is how the second and third rows become visible: a fresh attached session starts with a clean
transcript, but it tells you what the supervisor did while you were detached. It never
repeats.

So the honest promise to a tester is: **"this survives closing the Assistant panel"** — not
"this survives closing Daintree", and never "this runs overnight" unless Daintree stays up
on a Unix machine.

---

## 9. Contract summary — what each side may rely on

**Daintree may rely on:**

- stdout is machine-pure NDJSON; diagnostics only ever go to stderr.
- `seq` is monotonic from 1; a gap means a real loss, and stream frames apply backpressure
  rather than dropping.
- `turn:end.content` is authoritative and repairs the token stream.
- `approval:requested.needsTypedConfirm` is always present.
- `host:shutdown` is emitted before teardown work begins, and teardown is bounded.
- An unparseable approval decision fails **closed**; an unanswered question **cancels**
  rather than picking an answer.

**This CLI may rely on:**

- The MCP connection arrives **only** via `DAINTREE_MCP_URL` + `DAINTREE_MCP_TOKEN`, fresh per
  launch, localhost-only. Use the URL's port verbatim.
- `DAINTREE_PROJECT_ID` is stable across launches for a given project.
- The descriptor is the first line and carries no secret.
- The wire tier is `system`; the host will not gate dangerous calls on our behalf.

---

## Cross-repo maintenance

The wire contract must stay byte-for-byte with Daintree's
`ASSISTANT_HOST_PROTOCOL_VERSION` and its Zod schemas — Daintree validates stdout
line-by-line and rejects unknown shapes. A change here is a change in both repositories and a
bump of `host.ProtocolVersion`, which regenerates
[`generated/COMPATIBILITY.md`](generated/COMPATIBILITY.md):

- The MCP connection arrives **only** via `DAINTREE_MCP_URL` + `DAINTREE_MCP_TOKEN` env,
  fresh per launch, `/mcp` Streamable HTTP, localhost-only. Use the URL's port verbatim.
- `DAINTREE_PROJECT_ID` is a **stable identity** across launches/resumes for a given project
  — safe to key `StateDir` and per-project memory on.
- `DAINTREE_WINDOW_ID` is informational; the real binding is server-side.
- The session tier on the wire is **`system`**; the host will not tier-block the assistant,
  so **the CLI owns confirmation of dangerous operations.**
- `SESSION_BINDING_GONE` / `BINDING_STALE` are **terminal** — stop retrying that session and
  surface it to the user; they mean the bound window/project is gone.
- Worktree identity is **not** in env or cwd — query it over MCP (`actions.getContext` /
  `worktree.getCurrent`). The CLI pins it per TURN and forwards it explicitly on every
  spawn, so a mid-turn worktree switch changes what the model sees without moving where
  its agents land. `DAINTREE_WORKTREE_ID` is not that value and is not read.
- `/clear` can be **refused** (a turn in flight). Gate any transcript wipe on
  `command:result.conversationCleared === true`, never on the command text.
- Hiding keeps the CLI alive; **New session** kills it and drops the host-side transcript;
  eviction/close/crash kill it (with a resume capture that, for this CLI, currently
  translates to a fresh launch because there's no resume command).

```bash
go test ./internal/app -run TestGeneratedDocsAreCurrent -update
```

Protocol behaviour is tested by driving NDJSON frames through `internal/host` and asserting on
the emitted event stream — see `host_test.go`, `wire_test.go`, `transport_test.go`,
`bridge_test.go`, `interrupt_test.go`, `wake_shutdown_test.go`.
