# Daintree host — the `host --stdio` embedding contract (protocol v3)

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
| `DAINTREE_MCP_TOKEN` | Per-session bearer, sent as `Authorization: Bearer <token>`. Expires ~12 minutes after minting. | **Yes** → `cfg.McpToken` |
| `DAINTREE_PROJECT_ID` | The bound project's id. Scopes `StateDir` to `~/.daintree/assistant-cli/<project>/state.db`, and is stable across launches — safe to key per-project memory on. | **Yes** → `cfg.ProjectID` |
| `DAINTREE_WINDOW_ID` | Launching window id. **Informational**; the enforceable binding is server-side. | **Yes** → `cfg.WindowID` |
| `DAINTREE_ASSISTANT_AUTO_APPROVE` | `"1"` when the user turned off permission prompts. Reported back on `host:ready`. | **Yes** → `cfg.AutoApprove` |
| `DAINTREE_ASSISTANT_DEBUG_LOG` | `"1"` when the user enabled debug logging. | **Yes** → `cfg.DebugLog` |
| `DAINTREE_ASSISTANT_SCRATCH_DIR`, `DAINTREE_PANE_ID`, `DAINTREE_CWD`, `DAINTREE_WORKTREE_ID` | Host-provided metadata. | **No** — injected but not consumed. |

Daintree strips inherited `DAINTREE_*` variables from the shell environment before injecting
its own, so a stale token in the user's OS environment cannot leak in.

### The first line: `SessionDescriptor`

```json
{"sessionId":"ses_…","windowId":7,"projectId":"proj_…","cwd":"/path","tier":"system","protocolVersion":3,"resumeSessionId":"ses_prev"}
```

`sessionId`, `projectId`, `cwd` and `tier` must be strings; `windowId` and `protocolVersion`
must be **numbers**. A quoted number is a string and is rejected. `protocolVersion` must be
integral — a fractional `2.9` is refused rather than truncated past the `== ProtocolVersion`
check. `resumeSessionId` is optional and not type-checked.

**The descriptor carries no secret, deliberately.** `windowId` / `projectId` / `tier` are
validated here, but the *live* binding values come from the environment: a leaked or forged
descriptor line cannot re-point the session at another project or hand it a token.

A malformed descriptor yields `host:error` with code `bad-descriptor`, followed by teardown.
A version mismatch yields code `protocol-mismatch` naming both versions, then teardown. Both
are emitted **synchronously and flushed**, so the specific error reaches the parent before the
`host:shutdown` reason does.

### The reply: `host:ready`

```json
{"type":"host:ready","sessionId":"ses_…","seq":1,"protocolVersion":3,"autoApprove":false,"version":"1.4.2","resumedSessionId":"ses_prev"}
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
| `question:answer` | `questionId`, `choiceIndex` | Answer a multiple-choice question. `choiceIndex` is 0-based; **negative means dismissed**, which cancels the tool call rather than answering it. Required and must be a number — a missing one would default to 0 and silently answer "the first option" for a user who never chose. |
| `interrupt` | — | Cancel the running turn. Leaves autonomous wake work alone. |
| `hibernate` | — | Graceful teardown, reason `hibernate`. |
| `shutdown` | — | Graceful teardown, reason `exit`. |

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
| `host:ready` | `protocolVersion`, `autoApprove`, `version?`, `resumedSessionId?` |
| `host:error` | `code`, `message` |
| `host:shutdown` | `reason` (`hibernate`/`revoke`/`error`/`exit`), `resumeSessionId?` |
| `turn:start` | `turnId`, `role` (`user`/`assistant`), `startedAt` |
| `turn:phase` | `phase` — the canonical `domain.RunPhase` |
| `turn:token` | `turnId`, `chunk` — streamed prose, for liveness only |
| `turn:reasoning` | `turnId`, the round's thinking |
| `turn:interjection` | `text` — a message typed *while* the turn ran |
| `turn:end` | `turnId`, `endedAt`, `outcome?`, `content?` |
| `tool:batch` | `calls` — the whole batch announced as queued, before sequential dispatch |
| `tool:started` | `toolCallId`, `toolId`, `argsSummary`, `startedAt`, `danger`, `turnId?` |
| `tool:state` | `toolCallId`, `state` — promotes one announced call through its lifecycle |
| `tool:progress` | `toolCallId`, `message` — an in-tool substep |
| `tool:settled` | result classification, severity, `turnId?`, `asyncId?` |
| `approval:requested` | `approvalId`, `toolId`, `summary`, `requestedAt`, `needsTypedConfirm`, `riskClass?`, `consequence?`, `argsSummary?`, `turnId?` |
| `approval:decided` | `approvalId`, `decision`, `decidedAt` |
| `question:requested` | `questionId`, `question`, `options[{label,text}]`, `default`, `requestedAt`, `turnId?`, `toolCallId?` |
| `question:answered` | `questionId`, `choiceIndex`, `cancelled`, `answeredAt`, `label?`, `text?` |
| `usage` | per-round token accounting |
| `cost` | `total`, `complete` |
| `notice` | `level`, `message` |
| `model:rate-limited` | emitted when the provider throttled us after the retry budget |

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
  queue is above the high-water mark. Terminal and decision frames — `turn:end`,
  `approval:requested`, `host:shutdown`, command acks — **never wait and never drop**: they
  take the whole queue. This is the reserve that stops progress traffic from starving a
  result.
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
(`actions.getContext` / `worktree.getCurrent`); it is pinned at launch.

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
| MCP (`mcp --stdio`) | `daintree.approvals` / `daintree.approve`, or MCP elicitation — **delegated to the calling agent, not a human**, and only when launched with `--allow-delegated-approvals` | **not implemented** — the tool reports `QUESTION_UNAVAILABLE` |
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

The two rows above are also not the same KIND of decision, and the table should not be read
as though they were. On the native host a person sees the sheet and answers it. On MCP the
request goes to the model driving the session, which answers its own request — delegation,
not authorization. See [HEADLESS.md](HEADLESS.md#delegate-is-delegation-not-human-authorization).

`/backend`'s endpoint picker reuses this channel, marked local so Esc dismisses it instead of
cancelling the turn.

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

```bash
go test ./internal/app -run TestGeneratedDocsAreCurrent -update
```

Protocol behaviour is tested by driving NDJSON frames through `internal/host` and asserting on
the emitted event stream — see `host_test.go`, `wire_test.go`, `transport_test.go`,
`bridge_test.go`, `interrupt_test.go`, `wake_shutdown_test.go`.
