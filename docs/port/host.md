# Port spec: `src/host/**` → Go (`internal/host`)

Authoritative reference for porting the **embedded assistant-host protocol**
subsystem to Go (Bubble Tea UI in the wider app; this subsystem itself has NO UI).
Implement from this file; do not re-read the TS. Source of truth: TS
`src/host/protocol.ts`, `src/host/index.ts`, `src/host/bridge.ts`,
`src/host/errorGuard.ts`, plus imported contracts from `src/agent/wake.ts`,
`src/agent/events.ts`, `src/agent/loop.ts`, `src/cli/app.ts`.

> **WIP branch — no backwards compat.** No old-DB-format preservation required.
> The current shapes below are the *behavioral reference*; the Go port may favor a
> clean Go-native schema while preserving the externally-observable wire contract
> and ordering. The one hard external contract is the **protocol** Daintree
> consumes (event/command names + fields + `PROTOCOL_VERSION`), because Daintree
> validates inbound messages with Zod and rejects unknown shapes/versions.

---

## 0. TRANSPORT CHANGE (read first)

**TS transport (current):** Electron `utilityProcess.fork()` child. The host
speaks over `process.parentPort` (Electron `MessagePortMain`):
- Inbound (Daintree → host): `parentPort.on("message", ({data}) => …)`. The FIRST
  inbound message is the **session descriptor**; every subsequent inbound message
  is a **command**. `parentPort.start?.()` is called once to begin delivery.
- Outbound (host → Daintree): `parentPort.postMessage(event)` — structured-clone
  JS objects, not strings.
- Diagnostics: `process.stderr.write(...)` for warnings; `process.exit(0|1)`.

**Go transport (target): stdio NDJSON.**
- One JSON object per line.
- **stdin** = inbound command stream (first line = descriptor, rest = commands).
- **stdout** = outbound `HostEvent` stream, one JSON object per line.
- **stderr** = human-readable diagnostics (warnings, fatal traces). Never put
  protocol JSON on stderr.
- Replace `postMessage(obj)` with `json.Marshal(obj)` + `'\n'` to stdout (flush
  per line). Replace the `parentPort.on("message")` listener with a line-scanner
  goroutine over stdin (`bufio.Scanner` with a raised buffer, or a streaming
  `json.Decoder` reading whitespace-delimited values).
- Replace `process.exit(0|1)` with an explicit exit path that **flushes stdout
  first** (the TS code's `setImmediate(() => process.exit())` exists purely to let
  the last `postMessage` flush — preserve that: emit terminal event, flush, then
  exit).
- `parentPort.start?.()` has no analog; just start reading stdin.

**Behavior to preserve verbatim across the transport swap:** message ordering,
the descriptor-first handshake, the single-active-prompt rule, the
foreign/garbled-message drop, the exit-after-flush, and every event/command name
and field below.

---

## 1. What this subsystem does

The native host is the entry point Daintree forks when it wants to render the
assistant as native UI (in TS, native React; in Go, this is the headless protocol
core that a Bubble Tea front-end or Daintree itself drives). It:

1. Validates it is in the right launch context (TS: `process.parentPort` exists).
2. Installs a **bootstrap error guard** synchronously BEFORE any heavy init, so an
   early failure is reported and the process exits instead of hanging the
   readiness wait.
3. Receives + validates the **session descriptor** (first inbound message).
4. Wires up the `App` (the full assistant runtime: models, tools, MCP, daemon),
   connects MCP best-effort.
5. Announces `host:ready`, then services commands (prompt / approval / interrupt /
   hibernate / shutdown) until shut down.
6. Bridges the in-process agent event stream + tool-confirm hook into wire
   `HostEvent`s (`bridge.ts`).
7. Runs autonomous **read-only wake turns** when background watchers surface
   terminal activity while idle.

Three files:
- `protocol.ts` — pure types + vocabularies + 3 helpers (`severityForResult`,
  `isSessionDescriptor`, `isHostCommand`). No I/O.
- `bridge.ts` — `HostBridge`: adapts `AgentEventSink` + confirm hook → events;
  manages turn lifecycle, approvals, redaction. No transport dependency (injected
  `post` callback).
- `index.ts` — transport owner + command loop + boot/teardown + wake reactor.
- `errorGuard.ts` — install/dispose process-level uncaught/unhandled handlers.

---

## 2. Protocol version & vocabularies (`protocol.ts`)

### Constant

| Name | Value | Meaning |
|---|---|---|
| `PROTOCOL_VERSION` | `1` | Wire-format version. MUST equal Daintree's `ASSISTANT_HOST_PROTOCOL_VERSION`. Bump in lockstep on any breaking change. Daintree Zod-rejects an unrecognized version. |

### Enums / string-literal union vocabularies (values are VERBATIM wire strings)

**`AuditResult`** (7 values):
`"success"`, `"error"`, `"confirmation-pending"`, `"unauthorized"`, `"dedup"`,
`"collision"`, `"rate_limited"`.

**`AuditSeverity`** (5 values):
`"info"`, `"notice"`, `"warning"`, `"error"`, `"critical"`.

**`ConfirmationDecision`** (3 values):
`"approved"`, `"rejected"`, `"timeout"`.

**`TurnOutcomeClass`** (12 values):
`"answered"`, `"hedged"`, `"refused"`, `"docs-empty"`, `"tier-rejected"`,
`"mcp-not-ready"`, `"agent-stuck"`, `"tool-error"`, `"reasoning-loop"`,
`"hibernate-resume-stale"`, `"cancelled"`, `"unknown"`.

**`TurnRole`** (2 values): `"user"`, `"assistant"`.

**`HostShutdownReason`** (4 values): `"hibernate"`, `"revoke"`, `"error"`, `"exit"`.
(NB: `"revoke"` is defined in the type but never emitted by `index.ts` — teardown
only ever passes `"hibernate"`, `"error"`, `"exit"`. Keep `"revoke"` in the Go
enum for completeness/Daintree parity.)

### `severityForResult(result AuditResult) AuditSeverity`

Maps via the const `SEVERITY_BY_RESULT` (mirror of Daintree `mcpServer.ts`):

| AuditResult | AuditSeverity |
|---|---|
| `success` | `info` |
| `dedup` | `info` |
| `confirmation-pending` | `notice` |
| `unauthorized` | `warning` |
| `rate_limited` | `warning` |
| `collision` | `warning` |
| `error` | `error` |

Note `critical` is in the `AuditSeverity` set but is never produced by this map.

---

## 3. SessionDescriptor (first inbound message)

Non-secret descriptor Daintree sends once as the first inbound message. **Carries
NO bearer token or MCP URL** — those arrive via env, so a leaked port/stdin
message can never carry the secret.

| Field | Type | Required | Notes |
|---|---|---|---|
| `sessionId` | string | yes | The host's session id; stamped on every emitted event. |
| `windowId` | number | yes | Daintree window id (validated but read from env by `loadConfig`, not the descriptor, for the actual binding). |
| `projectId` | string | yes | Validated; actual project id read from env by `loadConfig`. |
| `cwd` | string | yes | Project path → passed as `overrides.projectPath`; also where `DAINTREE.md` is read from. |
| `tier` | string | yes | Validated; actual tier read from env by `loadConfig`. |
| `protocolVersion` | number | yes | Must equal `PROTOCOL_VERSION` (1) or boot rejects with `protocol-mismatch`. |
| `resumeSessionId` | string | optional | Hibernate/resume handle. When present, the App is built with this id instead of `sessionId` (conversation state replays). Echoed back in `host:ready.resumedSessionId`. |

**`isSessionDescriptor(value)` narrowing rule (validation gate):** object, non-null,
AND `sessionId`/`projectId`/`cwd`/`tier` are `string` AND `windowId`/`protocolVersion`
are `number`. `resumeSessionId` is NOT checked (optional). Go: a strict struct
unmarshal + presence/type check on the six required fields. A failing descriptor →
emit `host:error` code `bad-descriptor`, then teardown `"error"`.

---

## 4. Host → Daintree events (`HostEvent` union)

Every event carries `type` (discriminator) and `sessionId`. The Go encoder writes
each as one NDJSON line on stdout. Field names are verbatim.

| Event `type` | Fields (besides `type`,`sessionId`) | When emitted |
|---|---|---|
| `host:ready` | `protocolVersion:number`, `resumedSessionId?:string` | After App wired, MCP connect attempted, daemon started, runtime fatal handlers installed. `resumedSessionId` only when descriptor had `resumeSessionId`. |
| `turn:start` | `turnId:string`, `role:"user"\|"assistant"`, `startedAt:number` | User turn: emitted instantly in `startExchange`. Assistant turn: on first `assistantStart` of a send(). |
| `turn:token` | `turnId:string`, `chunk:string` | Each streamed assistant token (suppressed if interrupted or no active turn). |
| `turn:end` | `turnId:string`, `endedAt:number`, `outcome?:TurnOutcomeClass` | User turn: emitted instantly (same ts as start). Assistant turn: on close. |
| `tool:started` | `toolCallId:string`, `toolId:string`, `argsSummary:string`, `startedAt:number`, `turnId?:string`, `danger:boolean` | On `toolCall` sink event (suppressed if interrupted). |
| `tool:settled` | `toolCallId:string`, `toolId:string`, `durationMs:number`, `result:AuditResult`, `severity:AuditSeverity`, `errorCode?:string`, `turnId?:string` | On `toolResult` sink event (suppressed if interrupted). |
| `approval:requested` | `approvalId:string`, `toolId:string`, `summary:string`, `requestedAt:number`, `turnId?:string` | When a mutating tool needs confirmation (confirm hook). |
| `approval:decided` | `approvalId:string`, `decision:ConfirmationDecision`, `decidedAt:number` | On resolve (command decide, timeout, or drain). |
| `host:error` | `code:string`, `message:string` | Any error surface (see code table §9). |
| `host:shutdown` | `reason:HostShutdownReason`, `resumeSessionId?:string` | Emitted FIRST in teardown, before app.shutdown(). |

**Timestamp format:** all `*At` / `*Ms` fields are JS `Date.now()` epoch
milliseconds (integers). `durationMs` is `max(0, endedAt - startedAt)`. Keep
millisecond integers in Go (`time.Now().UnixMilli()`), do NOT switch to RFC3339 —
Daintree expects numeric ms.

---

## 5. Daintree → host commands (`HostCommand` union)

Each carries `type` + `sessionId`. Inbound on stdin (first line excepted = descriptor).

| Command `type` | Extra fields | `isHostCommand` validity rule | Effect |
|---|---|---|---|
| `prompt` | `text:string` | `text` is string | Run a command-driven turn (see §7). Rejected with `turn-in-progress` if busy. |
| `approval:decide` | `approvalId:string`, `decision:string` | both strings | Resolve outstanding approval → bridge.resolveApproval. |
| `interrupt` | — | always | Cancel in-flight turn (see §8). |
| `hibernate` | — | always | teardown(`"hibernate"`, sessionId). |
| `shutdown` | — | always | teardown(`"exit"`). |

`HostCommandType = HostCommand["type"]` — the union of the 5 strings above.

**`isHostCommand(value)` rule:** object, non-null, `sessionId` is string, then
switch on `type` per the table; unknown `type` → false. Go: discriminated decode
keyed on `type`, validate required fields per arm, else reject.

**Inbound routing in `index.ts` `port.on("message")`:**
- State machine: `state ∈ {"await-descriptor","running"}` (starts `await-descriptor`).
- In `await-descriptor`: data must be a `SessionDescriptor` (else `bad-descriptor`
  + teardown error). On success → `state="running"`, `void boot(data)`.
- In `running`: if `!isHostCommand(data) || data.sessionId !== sessionId` →
  **silently drop** (foreign/garbled; Daintree Zod-validates too). Else dispatch.

**sessionId match guard:** every command's `sessionId` must equal the booted
session id, else dropped. Preserve this in Go.

---

## 6. HostBridge (`bridge.ts`)

The adapter. Constructed with `HostBridgeOptions`:

| Option | Type | Default | Purpose |
|---|---|---|---|
| `sessionId` | string | — | Stamped on emitted events. |
| `post` | `(HostEvent) => void` | — | Transport sink (injected; tests + transport-swap friendly). |
| `riskOf` | `(toolName) => RiskClass\|undefined` | `() => undefined` | Look up tool risk for the `danger` hint. |
| `now` | `() => number` | `Date.now` | Injectable clock (tests). |
| `approvalTimeoutMs` | number | `5 * 60_000` (300000) | Unanswered confirm → `timeout`. |

### Constants

| Name | Value | Use |
|---|---|---|
| `DEFAULT_APPROVAL_TIMEOUT_MS` | `5 * 60_000` = **300000** ms (5 min) | Approval auto-timeout. |
| `ARGS_SUMMARY_MAX_STRING` | `80` | Strings longer than this collapse in `redactArgs`. |

### Internal state

- `activeTurnId: string|null` — the single open assistant turn id (one at a time).
- `interrupted: boolean` — once true, suppresses token/tool forwarding until next
  `startExchange`.
- `pendingApprovals: Map<approvalId, {resolve, timer}>` — outstanding confirms.
- `toolStartedAt: Map<toolCallId, number>` — `startedAt` recorded on tool:started,
  drained on settle to compute `durationMs`.

### `sink` — the `AgentEventSink` handed to `App.setHooks({agentEvents})`

Maps in-process loop events to wire events. **Turn model:** ONE assistant turn
spans a whole `AgentSession.send()` even across multiple model iterations + tool
calls. The loop fires `assistantStart()` once per iteration, but only the FIRST
opens the turn; later ones continue streaming into it; tool calls nest under it via
`turnId`.

| Sink method | Guard | Action |
|---|---|---|
| `assistantStart()` | skip if `interrupted` OR `activeTurnId` already set | mint `activeTurnId`, emit `turn:start role=assistant`. |
| `assistantToken(chunk)` | skip if `interrupted` OR no `activeTurnId` | emit `turn:token`. |
| `assistantEnd(content)` | skip if no `activeTurnId` | `closeTurn("answered")` if `content.trim()` non-empty, else `closeTurn("unknown")`. |
| `assistantCancelled()` | only if `activeTurnId` | `closeTurn("cancelled")`. |
| `toolCall(event)` | skip if `interrupted` | record `toolStartedAt[id]=startedAt`; emit `tool:started` with `argsSummary=redactArgs(args)`, `danger=isDanger(name)`, `turnId=activeTurnId??undefined`. |
| `toolResult(event)` | skip if `interrupted` | drain `toolStartedAt[id]`; `audit=resultToAudit(result)`; emit `tool:settled` with `durationMs=startedAt? max(0,endedAt-startedAt):0`. |
| `error(message)` | — | emit `host:error` code `turn-error`; if `activeTurnId` → `closeTurn("unknown")`. |
| `info()` | — | **no-op** (no protocol channel; intentionally dropped). |

(`usage` is optional on `AgentEventSink`; the bridge sink does NOT implement it —
token/cost usage is not forwarded over the host protocol. Note: there is NO
`turn:token`-style usage event in this protocol.)

### Lifecycle methods

| Method | Behavior |
|---|---|
| `startExchange()` | Reset per-turn state: `interrupted=false`, `activeTurnId=null`. Mint a turnId, emit `turn:start role=user` then immediately `turn:end` (same `now()` ts). **Prompt text is NOT carried** — Daintree originated the prompt and already has it. |
| `settleTurn(outcome="unknown")` | If `activeTurnId`, `closeTurn(outcome)`. Called in the `prompt`/`wake` finally to close any dangling assistant turn. |
| `interrupt()` | If no `activeTurnId`, no-op. Else set `interrupted=true`, `closeTurn("agent-stuck")`. Stops forwarding the in-flight turn output. |
| `confirm({toolName,summary})` | Mint `approvalId`, emit `approval:requested`. Return a `Promise<boolean>`. Arm a `setTimeout(approvalTimeoutMs)` → `resolveApproval(id,"timeout")` (if `approvalTimeoutMs>0`); `timer.unref()` so it doesn't keep the loop alive. Store `{resolve: d => resolve(d==="approved"), timer}`. |
| `resolveApproval(approvalId, decision)` | If not pending, no-op. Else delete from map, clear timer, emit `approval:decided`, call `pending.resolve(decision)`. |
| `settlePendingApprovals(decision="rejected")` | Iterate a snapshot of keys, `resolveApproval(id, decision)` each. Used on interrupt + teardown drain. |
| `closeTurn(outcome)` (private) | If `activeTurnId`, null it, emit `turn:end` with `outcome`. |
| `isDanger(toolName)` (private) | `risk = riskOf(name); risk !== undefined && risk !== "read"`. I.e. any non-`read` risk class = danger. |
| `genId(prefix)` (private) | `${prefix}_${randomUUID().slice(0,8)}` — e.g. `turn_1a2b3c4d`, `apr_…`. |

**ID format:** `<prefix>_<8 hex chars of a UUIDv4>`. Prefixes: `turn` (turns),
`apr` (approvals). Go: `fmt.Sprintf("%s_%s", prefix, uuid.NewString()[:8])` or first
8 hex of a random 16-byte UUID. Daintree treats these as opaque strings — only the
prefix+shape matters for parity, not the algorithm.

### Audit mapping helpers

**`resultToAudit(res ToolResult)`** → `{result, severity, errorCode?}`:
- If `res.ok` → `{result:"success", severity:"info"}` (no errorCode).
- Else `code = res.error?.code`; `result = ERROR_CODE_TO_RESULT[code] || "error"`;
  `severity = severityForResult(result)`; `errorCode = code`.

**`ERROR_CODE_TO_RESULT` map** (tool error code → AuditResult):

| Error code | AuditResult |
|---|---|
| `CONFIRMATION_REQUIRED` | `confirmation-pending` |
| `UNAUTHORIZED` | `unauthorized` |
| `TIER_REJECTED` | `unauthorized` |
| `FORBIDDEN` | `unauthorized` |
| `RATE_LIMITED` | `rate_limited` |
| `DEDUP` | `dedup` |
| `DUPLICATE` | `dedup` |
| `COLLISION` | `collision` |
| (any other / undefined) | `error` |

### `redactArgs(args unknown) → string`

Single-level, redacted JSON view of tool args for the timeline. Raw arg values may
carry file content / terminal output / prompt text and must NEVER cross the
boundary verbatim. Rules:
- `null`/`undefined` → `""` (empty string).
- top-level `string`: if `len > 80` → `"<string: N chars>"` (N = char length),
  else `JSON.stringify(s)` (i.e. quoted).
- top-level non-object (number/bool) → `JSON.stringify(v)`.
- top-level object/array: build a new object mapping each own key via `redactValue`:
  - string: `len > 80` → `"<string: N chars>"`, else the string as-is.
  - array → `"<array>"`.
  - non-null object → `"<object>"`.
  - else (number/bool/null) → as-is.
  Then `JSON.stringify(out)`; on throw → `"<unserializable>"`.
  **NB:** a top-level array is iterated with `Object.entries`, so its numeric
  indices become string keys → it serializes as an object `{"0":…}`, not an array.
  Preserve this quirk only if Daintree relies on it; otherwise document as a known
  oddity. Mirrors the redaction Daintree's MCP audit applies to `argsSummary`.

Go: `len` is the JS string `.length` (UTF-16 code units). For faithful parity use
UTF-16 length; in practice rune/byte length differs only for non-BMP/multibyte —
acceptable to use `utf8.RuneCountInString` and note the divergence, OR match
exactly with `utf16.Encode`. Document the choice.

---

## 7. index.ts — boot, command loop, wake reactor

### Process-level state (in `main()` closure)

- `sessionId = ""` (filled on descriptor; the bootstrap guard closes over it so a
  pre-descriptor crash still names the empty session).
- `state: "await-descriptor" | "running"` (`await-descriptor`).
- `ready: boolean` (false until `host:ready` emitted).
- `busy: boolean` (a turn — command or wake — is running).
- `turnController: AbortController|null` — the in-flight COMMAND turn's aborter.
  **Wake turns intentionally do NOT register here** (autonomous, read-only,
  unabortable).
- `bridge: HostBridge|null`.
- `app: {session:{send(input,opts?):Promise<string>}, shutdown():Promise<void>}|null`.
- `pendingWake: QueueEvent[]` — queued actionable wake events.
- `wakeRetried: boolean` — one-retry budget per burst.
- `summarizedTerminals: Set<string>` — terminal ids already reported this session
  (kept in sync conceptually with the Ink controller's set).

### `boot(descriptor)` sequence (exact order)

1. `sessionId = descriptor.sessionId`.
2. If `descriptor.protocolVersion !== PROTOCOL_VERSION` → emit `host:error` code
   `protocol-mismatch` (message names both versions), `teardown("error")`, return.
3. Dynamic-import (AFTER the bootstrap guard) `App` (cli/app), `startDebugLog`,
   `loadProjectInstructions`. *(Go: just regular imports; the "after guard" timing
   is irrelevant once there's no lazy native module load — but DO keep the guard
   active across heavy init so a failed init still reports + exits.)*
4. `appSessionId = descriptor.resumeSessionId ?? descriptor.sessionId`.
5. `projectInstructions = loadProjectInstructions(descriptor.cwd)`; if it has a
   `warning`, write it to stderr.
6. `App.create({ sessionId: appSessionId, overrides: { projectPath: descriptor.cwd,
   projectInstructions: projectInstructions.content } })`. MCP url/token/tier/project
   id come from env via `loadConfig()`, NOT the descriptor.
7. `startDebugLog(config, appSessionId)` (no-op unless enabled).
8. Construct `HostBridge({ sessionId, post, riskOf: name => registry.get(name)?.risk })`.
9. `instance.setHooks({ agentEvents: bridge.sink, confirm: req =>
   bridge.confirm({toolName: req.toolName, summary: req.summary}) })`.
10. `await instance.connectMcp()` — **best-effort**, a degraded MCP is not a boot
    failure (surfaces in prompt context + tool results).
11. `instance.startScheduler(events => { actionable = events.filter(isActionableWake);
    if none return; if pendingWake empty → wakeRetried=false; push actionable;
    void reactWake() })`. Starts the daemon (watchers/timers tick in-host).
12. `disposeBootstrapGuard()` then install long-lived `uncaughtException` /
    `unhandledRejection` → `onFatal` (emit `host:error` code `uncaught`,
    `teardown("error")`).
13. `ready = true`; emit `host:ready` (with `resumedSessionId` only when descriptor
    had `resumeSessionId`).

### `handleCommand(cmd)`

Guard: if `!ready || !bridge || !app` → emit `host:error` code `not-ready`
("Host is still starting."), return.

- **`prompt`**:
  - If `busy` → emit `host:error` code `turn-in-progress`
    ("A turn is already running; interrupt it before sending another prompt."),
    return. **(ONE ACTIVE PROMPT invariant.)**
  - `busy = true`; `bridge.startExchange()`.
  - Mint `controller = new AbortController()`; `turnController = controller`.
  - `try { await app.session.send(cmd.text, {signal: controller.signal}) }`
    `catch (err) { emit host:error code turn-failed }`
  - `finally`: **identity guard** — only `turnController = null` if
    `turnController === controller` (a later turn's controller must not be nulled
    by a stale finally). `bridge.settleTurn("answered")` (closes any dangling
    assistant turn — no-ops if loop already closed it). `busy = false`. If
    `pendingWake.length > 0` → `void reactWake()` (drain wakes deferred during turn).
- **`approval:decide`**: `bridge.resolveApproval(cmd.approvalId, cmd.decision)`.
- **`interrupt`**: see §8.
- **`hibernate`**: `await teardown("hibernate", sessionId)` — the resume handle IS
  the sessionId (conversation state persists in SQLite keyed by sessionId).
- **`shutdown`**: `await teardown("exit")`.

### `reactWake()` — autonomous read-only wake (the wake reactor)

Guard: if `busy || !ready || !bridge || !app` → return.
1. `events = pendingWake.splice(0)` (drain all). If empty → return.
2. `busy = true`; `bridge.startExchange()`.
3. `try`: `reply = await app.session.send(buildWakePrompt(events,
   {alreadySummarized: summarizedTerminals}), {readOnly: true})`.
   - `wakeRetried = false`.
   - If `!isWakeFailureReply(reply)`: for each event with a `target.terminalId`,
     add it to `summarizedTerminals`. (Only record on a REAL reply — `send()`
     returns a sentinel string on model failure, never throws, so guarding on the
     sentinel prevents marking terminals reported on a transient outage.)
4. `catch (err)`: emit `host:error` code `wake-failed`. If `!wakeRetried` →
   `wakeRetried = true`, `pendingWake.unshift(...events)` (requeue for ONE retry).
5. `finally`: `bridge.settleTurn("answered")`; `busy = false`; if `pendingWake`
   non-empty → `void reactWake()` (chain).

**Wake gating (`agent/wake.ts`):**
- `isActionableWake(e)` = `e.source === "terminal_watcher" && Boolean(e.target?.terminalId)`.
  Only terminal-watcher events with a real terminal id wake the model; model/user
  queue events never trigger an autonomous turn.
- `buildWakePrompt(events, {alreadySummarized})` builds the internal nudge. A
  terminal already in `alreadySummarized` (or seen earlier in THIS batch) gets a
  one-line "already reported — acknowledge, do NOT call terminal.read/summarize/
  extract again" downgrade; new ones get full read-and-summarize guidance. Prompt
  text is read-only context; the model's reaction is what surfaces.
- `isWakeFailureReply(reply)` = `reply.startsWith(prefix)` for any prefix in
  `WAKE_FAILURE_PREFIXES`: **`"Model unavailable:"`, `"Model error:"`,
  `"Tool projection failed:"`, `"Reached the tool-iteration limit"`,
  `"Stopped: called "`, `"Turn cancelled"`**. These are the sentinel strings
  `AgentSession.send` returns instead of throwing on a failed turn; a wake must NOT
  record terminals as summarized on these.

### `teardown(reason, resumeSessionId?)`

1. `bridge?.settlePendingApprovals("rejected")` — reject every outstanding approval
   so a parked `dispatch()` unblocks with USER_DECLINED.
2. Emit `host:shutdown` (with `resumeSessionId` only if provided). **Emitted BEFORE
   app.shutdown()** so Daintree sees the reason even if shutdown hangs.
3. `try { await app?.shutdown() } catch {}` (best-effort; exiting regardless).
4. `setImmediate(() => process.exit(0))` — let the messages flush first.
   **Go: flush stdout, then `os.Exit(0)`.**

### `main()` entry

- If `!parentPort` → `stderr.write("…host entry requires an Electron utility
  process.\n")`, `process.exit(1)`. **(Go: the equivalent precondition is "stdin is
  a usable command stream"; if launched wrong, write diagnostic to stderr, exit 1.)**
- Install `installBootstrapErrorGuard(report)` where `report(code,message)` emits
  `host:error` ONLY if `sessionId` is set (pre-descriptor crash names empty
  session, see code below).
- Register `port.on("message", …)` (the routing in §5), call `port.start?.()`.

---

## 8. Interrupt semantics (command `interrupt`) — three coordinated actions

Order matters; preserve all three:
1. `turnController?.abort()` — abort the running command turn's signal so
   `send()` actually stops mid-stream (cooperative checks in
   `AgentSession.send`/`ModelRouter.stream`), freeing `busy` and preventing
   post-cancel tool execution.
2. `bridge.settlePendingApprovals("rejected")` — the signal alone CANNOT unpark a
   turn awaiting a tool approval (`dispatch()` awaits `ctx.confirm()` →
   `bridge.confirm()`, a promise that only settles on `approval:decide` or the
   5-min timeout). Rejecting any outstanding approval makes `dispatch` return
   USER_DECLINED so `send()` reaches its next signal check instead of stranding
   `busy` forever.
3. `bridge.interrupt()` — display side: set `interrupted`, stop forwarding the
   in-flight turn's tokens/tools, close the turn with `"agent-stuck"`.

**Pending-approval rejection on interrupt AND shutdown/hibernate:** both
`interrupt` and `teardown` call `settlePendingApprovals("rejected")` — never strand
a parked confirm.

---

## 9. host:error codes (the `code` value set — verbatim)

| `code` | Source | Meaning |
|---|---|---|
| `bootstrap-error` | errorGuard | Uncaught/unhandled before runtime handlers installed. Followed by `exit(1)`. |
| `protocol-mismatch` | boot | Descriptor `protocolVersion` ≠ `PROTOCOL_VERSION`. → teardown error. |
| `bad-descriptor` | message router | First message not a valid descriptor. → teardown error. |
| `not-ready` | handleCommand | Command arrived before `ready`. |
| `turn-in-progress` | prompt | A prompt arrived while busy. |
| `turn-failed` | prompt | `send()` threw for a command turn. |
| `wake-failed` | reactWake | `send()` threw for a wake turn. |
| `turn-error` | bridge.sink.error | The loop emitted a fatal-for-turn error. |
| `uncaught` | runtime onFatal | `uncaughtException`/`unhandledRejection` after boot. → teardown error. |

---

## 10. errorGuard.ts

`installBootstrapErrorGuard(report (code,message)=>void) => disposer ()=>void`.
- Registers `process.on("uncaughtException", onError)` and
  `process.on("unhandledRejection", onRejection→onError)`.
- `onError(err)`: `message = err.stack ?? err.message` (or `String(err)`);
  `report("bootstrap-error", message)`; `setImmediate(() => process.exit(1))` (let
  the message flush first).
- Returns a disposer that `process.off`s both handlers (so the long-lived runtime
  handlers don't double-report).

**Go mapping:** there is no `uncaughtException` analog; the equivalent is a
top-level `recover()` in the goroutines that do heavy init + the command loop,
plus channeling `panic`/error returns into a single `report(code,message)` →
emit `host:error` → flush → `os.Exit(1)`. The "dispose then install long-lived"
two-phase pattern maps to: a boot-phase recover wrapper that, once `host:ready`
is emitted, hands off to the steady-state error path (`uncaught` code). Keep the
"only report if sessionId is set" rule.

`errMessage(err)` helper: `err instanceof Error ? (err.stack ?? err.message) :
String(err)` — use Go error string (with stack if available, e.g. via a
stack-capturing error or `debug.Stack()` on recover).

---

## 11. External contracts to KEEP

### Env vars (read by `loadConfig`, NOT the descriptor) — secrets never on the wire

| Var | Role |
|---|---|
| `DAINTREE_MCP_URL` | MCP server URL (secret-ish; env only). |
| `DAINTREE_MCP_TOKEN` | MCP bearer token (secret; env only). |
| `DAINTREE_WINDOW_ID` | Window binding. |
| `DAINTREE_PROJECT_ID` | Project binding. |
| `DAINTREE_ASSISTANT_TIER` | Actor tier (default `system`). |

Resolution precedence (from project CLAUDE.md / config.ts): **CLI overrides → env →
project `.env` → assistant's own `.env` → defaults.** The host passes `projectPath`
+ `projectInstructions` as `overrides`; everything else flows through `loadConfig`.
The descriptor's `windowId`/`projectId`/`tier` are validated but the live values
come from env — keep that split (a leaked descriptor carries no secret and cannot
re-bind the session).

### Protocol contract (the hard one)
- Event names, command names, field names, vocabulary string values: **verbatim**.
- `PROTOCOL_VERSION = 1`. Daintree Zod-validates; drift surfaces at the boundary.
- Daintree source of truth: `shared/types/ipc/assistantHost.ts` +
  `shared/types/ipc/mcpServer.ts` (audit vocab + `SEVERITY_BY_RESULT`). Keep in
  lockstep.

### ID / timestamp formats
- IDs: `<prefix>_<8 hex>` (`turn_…`, `apr_…`).
- Timestamps: epoch **milliseconds** integers (`Date.now()` → `UnixMilli()`).

### Prompt-cache key (referenced by the wider runtime, not this file)
- `MAIN_PROMPT_CACHE_KEY = "daintree-main"` (in `agent/loop.ts`) — a plain
  unversioned constant. Not part of the host wire protocol, but the host drives the
  same `AgentSession`, so preserve it in the agent port.

### SQLite (referenced, owned by storage subsystem)
- Conversation state persists keyed by `sessionId`; hibernate/resume replays via the
  same id. `run_events` append-only table is written by `RunEventSink`
  (`agent/events.ts`) with columns `runId`, `seq` (monotonic per run), `type`,
  `payload` (JSON, capped at `MAX_RUN_EVENT_PAYLOAD = 8000` bytes, oversize →
  `{truncated:true,bytes,preview}`). The host does not touch the DB directly; this
  is context for the App it wires. **No old-schema preservation required** — Go may
  define a native schema, but keep the keyed-by-sessionId resume behavior.

---

## 12. Non-obvious edge cases & ordering (do not lose)

1. **Descriptor-first handshake**: first inbound message MUST be the descriptor;
   anything else → `bad-descriptor` + teardown. After that, descriptors are not
   accepted again (state is `running`).
2. **Foreign-message drop**: in `running`, non-command or wrong-`sessionId` messages
   are silently dropped (no error event).
3. **One active prompt**: a second `prompt` while `busy` is rejected
   (`turn-in-progress`), NOT queued. Wakes, by contrast, ARE queued (`pendingWake`).
4. **Wake vs command serialization**: both gated by `busy`. A wake that arrives
   during a command turn is deferred and drained in the command's `finally`; a wake
   chains itself in its own `finally` if more pending.
5. **Wake one-retry budget**: `wakeRetried` resets to false when a burst starts from
   empty `pendingWake` (in the scheduler callback AND on success); a failed wake
   requeues once via `unshift`, then gives up.
6. **summarizedTerminals**: only updated on a NON-failure reply
   (`!isWakeFailureReply`); prevents a transient outage from permanently downgrading
   a terminal's later events to one-line acks.
7. **Turn identity guard**: `prompt` finally only nulls `turnController` if it's
   still the same controller (avoids a stale finally clobbering a newer turn).
8. **settleTurn after send**: error paths in the loop don't emit `assistantEnd`;
   `settleTurn("answered")` closes any dangling assistant turn (no-op if already
   closed).
9. **interrupted latch**: once `interrupt()` sets `interrupted=true`, all
   token/tool/start forwarding is suppressed until the next `startExchange` resets
   it. Approvals are still resolvable (decide/timeout) but rejected en masse on
   interrupt.
10. **User turn is zero-duration**: `startExchange` emits `turn:start` then
    `turn:end` with the SAME timestamp; the prompt text is never carried.
11. **host:shutdown emitted before app.shutdown()**: reason reaches Daintree even
    if shutdown hangs; exit is via `setImmediate`/flush-then-exit.
12. **approval timer is unref'd**: it must not keep the process alive on its own.
13. **`turn:token`/`tool:*` are suppressed when `interrupted`, but `assistantEnd`/
    `assistantCancelled` still close the turn** (so it can't leak open).
14. **`severityForResult` never returns `critical`**; `interrupt` always closes the
    turn as `"agent-stuck"` (not `"cancelled"`); `assistantCancelled` (from the
    loop's abort path) closes as `"cancelled"`. Both can fire — the first to run
    closes the turn, the second no-ops (guarded by `activeTurnId`).

---

## 13. Proposed Go mapping

### Packages
- `internal/host` — protocol types, the `Host` orchestrator (boot/command loop/
  wake/teardown), transport.
- `internal/host/wire` (or inline) — `HostEvent`/`HostCommand` types + JSON tags +
  marshal/validate. Pure, no I/O.
- `internal/host/bridge` — `Bridge` (the `HostBridge` port): event adaptation,
  approvals, redaction, audit mapping. Depends only on a `Post func(HostEvent)` and
  a `RiskOf func(string) (RiskClass, bool)`.

### Key types / interfaces
- `type HostEvent` — model as a tagged-union: either an interface
  `MarshalHostEvent` per concrete struct, or a single struct with `Type string` +
  all fields + `omitempty` (simplest for one-line NDJSON). Recommend per-type
  structs implementing a marshaler that injects `"type"` + `"sessionId"`.
- `type HostCommand` — decode via a two-pass: unmarshal `{Type, SessionId}`, then
  the arm-specific struct. Provide `ParseCommand([]byte) (HostCommand, error)`.
- Vocabularies as Go `string`-typed named consts (`type AuditResult string`, etc.)
  with the exact value sets above; validate on decode.
- `Bridge` struct with a `sync.Mutex` guarding `activeTurnID`, `interrupted`,
  `pendingApprovals`, `toolStartedAt`. Approvals: `map[string]pendingApproval` where
  `pendingApproval{resolve chan ConfirmationDecision, timer *time.Timer}` — or a
  `chan` the `confirm` caller blocks on (Go-idiomatic vs the JS promise).
- `AgentEventSink` Go interface mirroring `agent/events.ts` (assistantStart/Token/
  End/Cancelled, toolCall, toolResult, error, info; usage optional → separate
  optional interface). The bridge implements it.
- `confirm` returns `(bool, )` blocking on a channel selected against the timer;
  the host calls it from the dispatcher path.

### Transport
- `Transport interface { Send(HostEvent) error; Commands() <-chan inbound }` with a
  stdio impl: a goroutine `bufio` line-reader over `os.Stdin` feeding a channel; a
  mutex-guarded `json.Encoder` over `os.Stdout` (line-delimited). Diagnostics via a
  plain `os.Stderr` writer.
- The first line is decoded as a descriptor; subsequent lines as commands. Mirror
  the `await-descriptor`/`running` state machine.

### Concurrency model
- JS is single-threaded; Go is not. Use a **single command-loop goroutine** (a
  `for range commands` select) so command handling is serialized like the JS event
  loop. `busy`, `pendingWake`, `wakeRetried`, `summarizedTerminals` are owned by
  that goroutine (no extra locking) EXCEPT the `Bridge`'s own state (the agent loop
  runs `send()` on another goroutine and calls sink methods concurrently → the
  Bridge needs its own mutex). The `turnController` maps to a `context.CancelFunc`;
  `interrupt` calls it + drains approvals + `bridge.Interrupt()`.
- `send(ctx, text, opts)` runs on a worker goroutine; the command loop awaits its
  result (or, to keep servicing `interrupt`/`approval:decide` mid-turn, the loop
  must NOT block — run send in a goroutine and keep selecting; `busy` gates new
  prompts). **This is the key structural point**: in JS, `await send()` doesn't
  block the event loop, so `interrupt`/`approval:decide` still process while a turn
  runs. The Go loop MUST replicate that — do not block the command loop on `send`.

### Suggested libs
- `github.com/google/uuid` for IDs (`uuid.NewString()[:8]`).
- stdlib `encoding/json`, `bufio`, `context`, `time`, `sync` — no external protocol
  lib needed.
- Time: `time.Now().UnixMilli()`.

---

## 14. What to DELETE (Node/Bun/Electron/React-specific)

- `process.parentPort` / `MessagePortMain` / the `ParentPort` interface +
  `port.start?.()` — replaced by stdio NDJSON.
- `postMessage(structuredClone)` semantics — replaced by JSON line encoding.
- Dynamic `await import(...)` (lazy-loading `App`/`startDebugLog`/
  `loadProjectInstructions` to defer native module load) — Go has no such concern;
  use ordinary imports. KEEP the surrounding behavior (report+exit on init failure).
- `setImmediate(() => process.exit())` — replace with flush-stdout-then-`os.Exit`.
- `process.on("uncaughtException"/"unhandledRejection")` — no analog; replace with
  `recover()` + error-channel funnel (see §10).
- `timer.unref()` — Go timers don't keep the process alive; just `Stop()` them.
- `randomUUID().slice(0,8)` Node crypto → `github.com/google/uuid`.
- The "must run inside an Electron utility process" precondition message — replace
  with the stdio precondition.
- Anything React/OpenTUI/Bun: NONE of it lives in `src/host/**` (this subsystem is
  headless), so nothing to strip there — but do NOT pull in the `ui/` layer when
  porting; the host emits structured events only.

---

## 15. File-by-file porting checklist

| TS file | Go target | Notes |
|---|---|---|
| `protocol.ts` | `internal/host/wire` | Types, vocab consts, `severityForResult`, `parseDescriptor`, `parseCommand`. Pure. |
| `bridge.ts` | `internal/host/bridge` | `Bridge` + `redactArgs` + `resultToAudit` + maps + constants (300000, 80). Mutex-guarded. |
| `errorGuard.ts` | recover wrapper in `internal/host` | Two-phase: boot guard → steady-state. |
| `index.ts` | `internal/host` `Host.Run` | Transport, state machine, boot, command loop, wake reactor, teardown. Keep the non-blocking-command-loop property. |
| `agent/wake.ts` (imported) | `internal/agent/wake` | `IsActionableWake`, `BuildWakePrompt`, `IsWakeFailureReply` + the 6 sentinel prefixes. |
| `agent/events.ts` (imported) | `internal/agent` (sink iface) | `AgentEventSink` interface only (the rest is the agent port). |
