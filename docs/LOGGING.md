# Logging / the per-session diagnostic trace

> **Temporary, dev-only.** This is the full-fidelity debug trace gated behind
> `DAINTREE_ASSISTANT_DEBUG_LOG=1` — it is a development aid, not a product surface.
> It writes one append-only human-readable file per session under `~/.daintree/logs`
> (`<date>-<sessionId>.log`, dir 0700 / file 0600, pruned after 7 days). Disabled, it
> is a no-op and never throws. See `internal/debuglog`.

## Why this exists

The trace is the **ground truth** for how the model and tools actually behaved. The
recurring dev loop here is: a real session misbehaves → you grep its log → you find
where the model misjudged or misused a tool → you fix the *system* (prompt/skill in
`../assistant-backend`, or a local tool shape) so it can't recur.

The backend migration moved the model call behind `Backend.RespondStream`, which left
the trace blind exactly where it used to be richest — there is no more
`model.request`/`model.response` for the main loop. The `backend.respond.*` and turn
lifecycle events below restore that coverage.

## Correlation

Every turn-scoped line carries the ids you grep by:

- `runId` — one `Session.Send()` turn (also the `/explain` run id).
- `turnId` — one backend turn; shared across every round of a turn.
- `round` — the model round within a turn (0-based).
- `toolCallId` — one tool call (ties a `tool.call` back to the `backend.respond.done`
  that requested it).

`grep <runId>` over the log gives you the whole turn in order.

## Payload policy: bounded, hashable

The per-round events (`backend.respond.*`) and every `mcp.call` would, if dumped in
full, re-print the entire conversation / prompt / terminal scrollback every round — the
O(turns²) blowup. So they carry a **`debuglog.Summary`** instead: `{bytes, sha, preview,
truncated}` — full byte length, a sha256 prefix (so an unchanged payload is recognisable
round-to-round without re-printing it), and a bounded preview. Two deliberate exceptions
stay full-fidelity: the `tool.call` args + result (logged once per call, the thing the
dev loop greps) and the conversation persisted in SQLite. **No per-token lines** are ever
written — only first-token timing + aggregate stats.

## Event reference

### Turn lifecycle
| event | when | key fields |
|---|---|---|
| `turn.start` | a turn begins | `runId` `turnId` `sessionId` `isWake` `promptPreview` `historyLen` |
| `turn.end` | a turn ends (any path) | `status` (complete/cancelled/failed) `durationMs` `rounds` `replyPreview` |

`turn.end status=failed` is the fastest "which turn went wrong" filter.

### Backend respond round (the model call)
| event | when | key fields |
|---|---|---|
| `backend.respond.request` | before each round's stream | `round` `instructionRevision` `statePresent`/`stateBytes`; `input` = `{messageCount, messageRoles, messagesSha, toolCount, toolNames, toolsetSha, toolChoice, lastMessage}`; `runtime` (tier/project/mcp); `turn` (goal preview + memory/workflow counts) |
| `backend.respond.meta` | first SSE meta event | `backendRequestId` `model` `promptVersion` `catalogRevision` `stateSha` `warnings`; `skills` = `{active, newlyLoaded, selector{ran,degraded,taskType,confidence,reason}}` |
| `backend.respond.done` | round completed | `durationMs` `firstTokenMs` `contentChars` `contentPreview` `finishReason` `toolCallCount` `toolCalls[]` (id + name + args preview/hash) `usage` `reasoningPresent` |
| `backend.respond.error` | non-cancel respond failure | `durationMs` `error` |

`request` = what the backend was **shown**; `meta` = what it **decided** (incl. which
skill it loaded — the surface that says "fix the selector, not the tool"); `done` = what
it **produced**.

### Tools
| event | when | key fields |
|---|---|---|
| `tool.call` | every dispatched call (full-fidelity; args are post-decode) | `tool` `toolCallId` `runId` `sessionId` `risk` `actor` `actorId` `outcome` `ok` `durationMs` `summary` `args` `result` `error` |
| `tool.args.invalid` | args weren't valid JSON (never reached dispatch) | `runId` `toolCallId` `tool` `argsPreview` |
| `tool.not_offered` | tool excluded by an explicit allowlist (dormant today) | `runId` `toolCallId` `tool` |
| `tool.cancelled_stub` | calls given a synthetic CANCELLED result on abort | `runId` `count` |
| `tool.repeat.warning` / `tool.repeat.abort` | circuit breaker fired on a repeated failing call | `runId` `tool` `count` `errorCode` `signature` |

### MCP
| event | when | key fields |
|---|---|---|
| `mcp.call` | every MCP tool call (once, on exit) | `mcpTool` `callKind` (read/mutation) `retries` `attempts` `durationMs` `transportOk`; `isError` + `text`/`structured` summary on success, `error` on failure (`transportOk` = no Go error; a tool-level failure still has `transportOk=true` + `isError=true`) |

### Async futures (asyncwork.Coordinator)
| event | when | key fields |
|---|---|---|
| `async.registered` | an async invocation entered the live poll set | `asyncId` `tool` `group` `terminals` `expiresAt` |
| `async.settled` | every watched terminal settled (or the deadline hit) | `asyncId` `expired` `settleAt` `outcomes` |
| `async.published` | ONE grouped completion event published to the queue | `group` `asyncIds` `eventId` `title` |
| `async.publish_failed` | the queue publish failed (retried next tick) | `group` `asyncIds` `error` |
| `async.claim_lost` | a settle/finalize write lost to a concurrent async.cancel | `asyncId` `stage` |
| `async.read.backoff` | consecutive status-read failures triggered the 5s backoff | `consecutiveFailures` |

### Other (pre-existing)
`session.start`, `mcp.credentials` (temporary debug aid), `backend.retry`,
`watcher.*` / `spawn.*`, `skill.step.*`, `reconcile.*`, `session.checkpoint.resumed`.

## Suggested analyzer workflow

1. Scan `turn.end` for `status=failed|cancelled` (or a slow `durationMs`).
2. `grep <runId>` to pull that turn's whole timeline.
3. Read its `backend.respond.request`/`meta`/`done` per round: was the model shown the
   right context? did it load the right skill? what did it choose?
4. Inspect failed `tool.call` / `tool.args.invalid` / `tool.repeat.*` for that turn.
5. Follow a failing `tool.call` down to its `mcp.call` to see whether the fault was the
   tool, the args, or the MCP layer (throttle/transport).
6. Decide the fix surface: bad args + ambiguous schema → local tool desc/schema or a
   backend skill; correct args + tool error → local tool impl or MCP; wrong/no skill →
   backend selector; missing context → CLI runtime/turn assembly; backend 4xx →
   CLI/backend contract.
