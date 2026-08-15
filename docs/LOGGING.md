# Logging / the per-session diagnostic trace

> **Dev-only, owner-only.** This is the session diagnostic trace gated behind
> `DAINTREE_ASSISTANT_DEBUG_LOG=1` — it is a development aid, not a product surface.
> It writes one append-only human-readable file per session under `~/.daintree/logs`
> (`<date>-<sessionId>.log`, dir 0700 / file 0600, pruned after 7 days). Disabled, it
> is a no-op and never throws. See `internal/debuglog`.
>
> **It is not a support artifact.** Credentials are redacted before anything is written
> (shapes, plus this process own key and MCP token by exact value), but the trace still
> contains your conversation, terminal output, file
> excerpts, issue/PR bodies, and memory contents. Never paste one into an issue or hand
> it to someone else — use `daintree-assistant support-bundle`, which is redacted,
> bounded, and shows you what it will include before it writes.
>
> The raw Daintree MCP URL + bearer token used to be written to the top of every log as
> an `mcp.credentials` line, so a session log could be replayed by hand against the live
> MCP. **That was removed** — a short-lived MCP token still authorises system-tier
> Daintree actions for its whole validity window, and log files outlive it. To replay MCP
> calls, take the credentials from the running process's environment
> (`DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN`) while it still owns them.

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
truncated}` — byte length, a sha256 prefix (so an unchanged payload is recognisable
round-to-round without re-printing it), and a bounded preview. The `tool.call` args +
result are the deliberate exception: logged once per call, they are the thing the dev
loop greps, so they are written in full up to a **64 KiB per-value cap**. Beyond that the
middle is elided, keeping the head AND the tail — build and test output puts the failure
at the *end*, so a head-only cut discards the one line you opened the log for — with the
true size and a content hash in between. **No per-token lines** are ever written; only
first-token timing + aggregate stats.

> **Everything is redacted before it is written.** `internal/redact` runs at the write
> boundary (`debuglog.formatLine`), not at the call sites, so no event can opt out.
> Structured values are walked and re-marshaled; free text goes through the credential
> patterns. `Summary`'s byte count and sha256 describe the **redacted** form — hashing the
> raw payload would leave a verifier that could confirm a guessed credential, which is
> exactly what redaction is meant to deny. See [`../internal/redact`](../internal/redact)
> for what is deliberately NOT redacted (the conversation, artifacts, and scheduled job
> payloads, all of which need their raw values to function).

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
| `backend.respond.request` | before each round's stream | `round` `instructionRevision` `statePresent`/`stateBytes`; `startup` = `{sha, projectPresent, rosterPresent, instructionBytes, agent counts/completeness}` (never instruction contents); `input` = visible-history `{messageCount, messageRoles, messagesSha, toolCount, toolNames, toolsetSha, toolChoice, lastMessage}`; `runtime` (tier/MCP/typed worktree/open terminals/`display` = the `{columns, contentWidth}` the reply was shaped for, absent when unmeasured or withheld); `turn` (goal preview + memory/workflow counts) |
| `backend.respond.raw_meta` | each HTTP attempt's SSE meta arrives | `backendRequestId` `model` `newlyLoadedCount`; transport observation only, so a retried logical round can contain more than one |
| `backend.respond.skill_cue` | eager skill-loaded cue reaches the sinks | `skills`; absent when no new skill is surfaced and de-duplicated across retries |
| `backend.respond.meta` | retry-safe meta commits | `backendRequestId` `model` `promptVersion` `catalogRevision` `stateSha` `warnings`; `skills` = `{active, newlyLoaded, selector{ran,degraded,taskType,confidence,reason}}`; normally fires with first content or successful tool-only completion |
| `backend.respond.done` | round completed | `durationMs` `firstTokenMs` `contentChars` `contentPreview` `finishReason` `toolCallCount` `toolCalls[]` (id + name + args preview/hash) `usage` `cost` `reasoningPresent` |
| `backend.respond.error` | non-cancel respond failure | `durationMs` `error` |

`request` = what the backend was **shown**; `meta` = what it **decided** (incl. which
skill it loaded — the surface that says "fix the selector, not the tool"); `done` = what
it **produced**.

`done`'s `cost` block (`{total, main, selector, complete}`, in USD, on the caller's own
OpenRouter key) is present only when the backend reported one — its **absence means
unknown, never free**, and it is never zero-filled. It is logged per round rather than
only as a session total because "why was that turn expensive?" is a question you answer
by reading a log, and because `selector` is the share prompt work can actually move.
Read it next to `usage.cachedTokens / usage.promptTokens`: the prompt-cache hit ratio is
what the backend's byte-stable prompt assembly buys, and a collapse in it is the first
symptom of a regression that costs the user money directly.

### Tools
| event | when | key fields |
|---|---|---|
| `tool.call` | every dispatched call (args post-decode, redacted, 64 KiB cap) | `tool` `toolCallId` `runId` `sessionId` `risk` `actor` `actorId` `outcome` `ok` `durationMs` `summary` `args` `result` `error` |
| `tool.args.invalid` | args weren't valid JSON (never reached dispatch) | `runId` `toolCallId` `tool` `argsPreview` |
| `tool.not_offered` | tool excluded by an explicit allowlist (dormant today) | `runId` `toolCallId` `tool` |
| `tool.cancelled_stub` | calls given a synthetic CANCELLED result on abort | `runId` `count` |
| `tool.repeat.warning` / `tool.repeat.abort` | circuit breaker fired on a repeated failing call | `runId` `tool` `count` `errorCode` `signature` |

### MCP
| event | when | key fields |
|---|---|---|
| `mcp.call` | every MCP tool call (once, on exit) | `mcpTool` `callKind` (read/mutation) `retries` `attempts` `durationMs` `transportOk`; `isError` + `text`/`structured` summary on success, `error` on failure (`transportOk` = no Go error; a tool-level failure still has `transportOk=true` + `isError=true`) |

### Backend utility tasks
| event | when | key fields |
|---|---|---|
| `backend.task` | every RunTask round trip, success or failure (checkpoint, memory_distill, watcher_classify, terminal_judge/summarize/extract, …) | `task` `durationMs` `inputBytes` `outputBytes` `ok`; `error` on failure. A `/compact` shows up as one `checkpoint` + one `memory_distill` line. |

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
`session.start`, `backend.retry`
(`op` `attempt` `maxAttempts` `delayMs` `error` — fires per transient-failure replay on
**any** backend call; `op` is `respond` or the JSON method+path, so a stalled turn and a
stalled utility task are distinguishable),
`watcher.*` / `spawn.*`, `skill.step.*`, `reconcile.*`, `session.checkpoint.resumed`.

## Suggested analyzer workflow

1. Scan `turn.end` for `status=failed|cancelled` (or a slow `durationMs`).
2. `grep <runId>` to pull that turn's whole timeline.
3. Read its `backend.respond.request`/`raw_meta`/`skill_cue`/`meta`/`done` per round: was the model shown the
   right context? did it load the right skill? what did it choose?
4. Inspect failed `tool.call` / `tool.args.invalid` / `tool.repeat.*` for that turn.
5. Follow a failing `tool.call` down to its `mcp.call` to see whether the fault was the
   tool, the args, or the MCP layer (throttle/transport).
6. Decide the fix surface: bad args + ambiguous schema → local tool desc/schema or a
   backend skill; correct args + tool error → local tool impl or MCP; wrong/no skill →
   backend selector; missing context → CLI startup/runtime/turn assembly; backend 4xx →
   CLI/backend contract.
