# Runtime behavior — convergence, compaction & model errors

Three runtime behaviors surprise new users the first time they hit them: a long turn being
**closed** with a plan instead of a result, the assistant silently **compacting** a long
conversation, and a turn ending with a terse **model error** instead of an answer. All three
are deliberate. This is the "why did it do that?" reference.

## Turn convergence

A turn ends when the model stops calling tools. Two guards close one that never does.

**Repetition.** A round that produces no `(tool, arguments, result)` triple the turn has not
already seen learned nothing — it re-ran work whose answer had not moved. `domain.TurnStallWarn`
consecutive such rounds nudge the model; `domain.TurnStallAbort` close the turn. Keying on the
RESULT and not the call alone is what lets a legitimate poll of a mutable read
(`watcher.list`, `agentTask.status`) keep going: its answer changes, so it is progress.

**Round budget.** A turn is closed after `domain.TurnRoundBudget` model rounds regardless,
with a warning at `domain.TurnRoundWarn`. A model can churn without ever repeating itself —
a different file every round, forever — and the budget is the backstop for that.

**What a close looks like.** Not silence and not an error: the loop spends its last round with
`tool_choice: "none"` and asks the model to report what it already set running, the plan it
would follow, and what it needs narrowed. That answer is the turn's reply. Only when the
closing round produces no prose at all does a deterministic "Stopped after N rounds…" stand in.
The human also gets a warning line saying the turn was closed rather than answered.

**It bounds the turn, not the work.** Nothing already spawned is discarded, and "continue"
starts a fresh turn with a fresh budget. The round budget counts rounds streamed since the turn
began and nothing rewinds it — a mid-turn injection resets the repetition tally but never the
budget, so every turn ends within `TurnRoundBudget + 1` rounds. The guard lives in
[`internal/agent/stall.go`](../internal/agent/stall.go).

## Auto-compaction

To keep long sessions affordable and inside the model's working set, the assistant
summarizes and drops old turns once the conversation grows too large.

**When it fires.** Before your message is sent, and again at the top of every subsequent
model round of that turn (a turn can run many rounds, so context has to be re-bounded each
time). The gate is `maybeAutoCompact` in
[`internal/agent/session.go`](../internal/agent/session.go); the constants live in
[`internal/domain/constants.go`](../internal/domain/constants.go).

The size estimate is the **larger** of two figures:

- `prompt_tokens` as actually reported by the backend on the previous round — the honest
  number, because it counts the ~18k tokens of tool schemas that a character count is
  blind to, but it is stale by whatever has been appended since;
- a live character estimate of the current history (≈ 4 chars per token,
  `domain.CharsPerToken`) — which sees this round's tool results and any daemon-injected
  note.

Taking the max means a large mid-round injection still trips the gate while the real
provider figure governs the steady state.

**The soft threshold is 500,000 tokens** (`AutoCompactTokenThreshold`), sized against the
large model's ~1M-token window (`LargeContextWindowTokens`).

**What happens.**

1. **Lossless pre-sweep first.** Before paying for a model call, a deterministic pass
   dedupes byte-identical tool results and collapses already-archived overflow stubs to
   their artifact placeholder. If that alone drops the history back under the threshold,
   no summary is taken at all.
2. **The backend's `checkpoint` task** builds a structured checkpoint. The CLI sends only
   a flattened transcript — `FlattenTranscript` folds in tool-call names *and* their
   argument JSON, so load-bearing IDs that exist only in arguments (e.g. a
   `terminal.read {"terminalId":"terminal-<uuid>"}`) survive into the checkpoint's
   ID-preservation pass. The backend owns the prompt.
3. **The full transcript is archived** as a durable artifact and a breadcrumb is appended
   to the summary, so anything the checkpoint rounded off stays recoverable via
   `artifact.read` rather than being lost.
4. **History is rebuilt** as the compaction markers plus the summary — and then a
   **verbatim recent tail** is re-appended (`AutoCompactVerbatimTailMessages` = 16
   messages, capped by `AutoCompactVerbatimTailTokenBudget` = 20,000 tokens, orphan-cleaned
   so no tool reply is left without its call). The summary rounds off exactly the
   references a mid-task orchestrator still needs — terminal / run / watcher / workflow
   IDs, the active branch, an open grant — so the raw tail keeps them intact.
5. **Durable facts are distilled into memory** off the critical path, after compaction, so
   the model stream is unblocked first. Novel facts are saved with source `compact`. This
   step is **silent** — it emits no attached session note.

The two markers that replace the dropped history are:

- a system marker, `[conversation compacted — earlier turns dropped from context]`
  (`compactionMarker`) — the rehydration boundary keys off this exact text;
- a user note prefixed `[checkpoint | depth N]` (`compactionNotePrefix`) followed by the
  summary. The depth counter makes a summary-of-summary chain observable instead of
  silently flattening detail over a long run.

**When the checkpoint fails.** Compaction is best-effort and never breaks a turn. A cancel
is just the turn tearing down (nothing counted). A real outage counts toward a bounded
fallback: after `AutoCompactFailureThreshold` (3) consecutive failures *and* once the
history passes the hard ceiling `AutoCompactHardTruncationThreshold` (800,000 tokens), the
assistant does a **model-free lossy head truncation**, keeping the most recent
`AutoCompactHardTruncationKeepMessages` (16) messages. That ceiling sits above the soft
threshold but below the model's window, so context can never grow unbounded just because
the summarizer is down.

**What you see.**

| Note | Meaning |
|---|---|
| `Auto-compacted conversation` | the normal path succeeded |
| `Auto-compact skipped: checkpoint failed` | the checkpoint call failed — emitted **once per failure streak**, not once per round, so an outage doesn't flood the footer |
| `Auto-compact fallback: truncated old history (checkpoint unavailable)` | the model-free truncation ran |

Memory distillation is deliberately silent; nothing else interrupts the turn.

**Doing it on purpose.** `/compact` runs the same summarize-and-reset on demand. `/clear`
is different — it drops the conversation entirely (`[conversation cleared — context reset
to initial state]`) with no summary, and it is the only wholesale teardown of
project-scoped supervision state (watchers, async futures, the attention inbox).

> **Not the same as the CTX gauge.** The attached session's `CTX%` reads "% of the *model's*
> context window in use" against the large model's ~1M-token window
> (`domain.LargeContextWindowTokens`), **not** "% toward auto-compaction." They measure
> different things, though at a 500K soft threshold the gauge will read roughly half full
> when compaction fires.

## Model errors (rate limit, backend down, unavailable)

When a turn can't reach a usable model response, the assistant ends the turn with a short,
stable reply rather than a raw provider blob. The mapping lives in `classifyBackendError`
([`internal/agent/session.go`](../internal/agent/session.go)); the typed error is
`backend.Error` ([`internal/backend`](../internal/backend)).

These reply prefixes are **byte-stable on purpose** — they are registered
`WAKE_FAILURE_PREFIX`es (see `internal/agent/wake.go`), so an autonomous wake turn that
fails this way is recognised as a non-result rather than treated as a real answer. Don't
reword them casually.

| What happened | Reply you see | Notes |
|---|---|---|
| Upstream/model rate limit or quota, after the backend's retry budget is spent | `Model rate-limited: …` | Raises a **model-health badge** in the attached session, cleared automatically by the next successful usage event. |
| The Daintree assistant backend is unreachable | `Can't reach the Daintree assistant backend — is it running? …` | The most common local-dev failure. Named as a connectivity problem with a next step instead of a dialer blob mislabeled as a model error. `/doctor` probes exactly this. |
| You pressed Escape to cancel the turn | `Turn cancelled` | A clean stop, not a failure — the loop treats it as such. |
| Any other model/transport error | `Model error: …` | The underlying error text is appended. |

**Where retries happen.** Three independent layers, and it matters which one you are
looking at:

1. **Provider hop (backend-owned).** The CLI does **not** talk to the model provider —
   the backend does, and it owns the provider credentials and the upstream retry budget.
   The `Model rate-limited:` reply only appears once *that* budget is exhausted.
2. **CLI↔backend hop (`internal/backend/retry.go`).** Every call to the backend — the
   streamed respond turn *and* everything routed through `doJSON` (tasks, capabilities,
   health, ready, version, non-streaming respond) — retries transient failures:
   **10 attempts**, exponential from 500ms and settling into a **10–15s** poll
   (~50–75s of backoff), the whole call additionally capped by a **2-minute** elapsed
   window (`MaxElapsed`) because the attempts themselves are not free. It is sized to ride
   out a **backend restart**, the failure it exists for: before this, a restart mid-turn
   meant one 286ms replay against a still-closed socket and a dead turn.

   **What is replayed:** `connect` (never reached the backend), 429 rate limits
   (honouring `Retry-After`, capped at 15s), and 502/503/504 that carry no application
   verdict — plus, mid-stream, the pre-content upstream/truncation errors.
   **What is not:** auth (401/403), contract (400), protocol (426); the backend's
   application verdicts, keyed on `error.code` rather than status because it reuses 502
   for both — `task_output_invalid` (the model ran and its output was unparseable),
   `upstream_error` (the provider *rejected* the request), `internal_error` (500, which
   may already have run a side effect); and a JSON attempt that exhausts its own 60s
   per-attempt timeout (`timeout` — the backend was answering, just slowly). A respond
   turn also stops retrying the moment any visible token has streamed, since a replay
   would duplicate on-screen text.

   **Visibility:** a respond retry emits ONE attached session note per round (a note is a
   standalone transcript cell, so one per attempt would stack up and commit out of order
   with the answer). Every retry on every endpoint emits a `backend.retry` debug-log line
   carrying `op`, so a stalled turn is distinguishable from a stalled utility task.
   `/doctor` opts out via `backend.WithoutRetry` — a probe reports the hop's state *now*.
3. **Daintree MCP side.** Read-only tool calls are auto-retried on a transient transport
   blip or an `MCP_RATE_LIMITED` throttle result (honouring the server's `retryAfter`,
   capped); mutations are single-shot so a retry can never double-apply.

Retries are always bounded by the caller's context: Escape cancels mid-backoff, and a
call that carries its own deadline (a boot handshake, a scheduler item) never outlives it.

## See also

- The backend integration — model, runbooks, prompt assembly — [`BACKEND.md`](BACKEND.md).
- Environment variables (tier, offline, state dir) — [`README.md`](../README.md#environment-variables).
- Full-fidelity tracing for debugging — set `DAINTREE_ASSISTANT_DEBUG_LOG=1`
  ([`LOGGING.md`](LOGGING.md) is the event reference).
- The turn loop and where compaction sits in it — [`ARCHITECTURE.md`](ARCHITECTURE.md).
