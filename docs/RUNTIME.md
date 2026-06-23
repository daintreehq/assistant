# Runtime behavior — compaction & model errors

Two runtime behaviors surprise new users the first time they hit them: the assistant
silently **compacting** a long conversation, and a turn ending with a terse **model
error** instead of an answer. Both are deliberate. This is the "why did it do that?"
reference.

## Auto-compaction

To keep long sessions affordable and inside the model's working set, the assistant
summarizes and drops old turns once the conversation grows too large.

**When it fires.** At the *start* of every turn, before your message is sent, the
assistant estimates the conversation size (≈ 4 characters per token,
`domain.CharsPerToken`). If the estimate exceeds **60,000 tokens**
(`domain.AutoCompactTokenThreshold`) *and* there is real history beyond the fixed control
messages, it compacts. The check and the threshold live in
[`internal/domain/constants.go`](../internal/domain/constants.go); the logic is
`maybeAutoCompact` in [`internal/agent/session.go`](../internal/agent/session.go).

**What happens.**

1. The **small** model summarizes the conversation in 2–3 sentences (current goals, key
   decisions, pending work).
2. Before the transcript is discarded, a second small-model pass **distills durable facts**
   into persistent memory (saved with source `compact`), so cross-session knowledge isn't
   lost when the verbatim turns go away.
3. The working history is replaced with two markers:
   - a system marker — `[conversation compacted — earlier turns dropped from context]`
   - a user note — `[compacted summary of earlier conversation]` followed by the summary.

The earlier turns are **gone from the model's context** after this — the summary and any
distilled memories are what remains. Best-effort by design: if the summary call fails or
comes back empty, the conversation is left untouched and the turn proceeds (you'll see an
`Auto-compact skipped: …` note).

**What you see.** The cockpit emits an `Auto-compacted conversation` line, and (when facts
were saved) a `Distilled N memories before compacting` line. Nothing else interrupts the
turn.

**Doing it on purpose.** `/compact` runs the same summarize-and-reset on demand. `/clear`
is different — it drops the conversation entirely (`[conversation cleared — context reset
to initial state]`) with no summary.

> **Not the same as the CTX gauge.** The cockpit's `CTX%` reads "% of the *model's*
> context window in use" against the large model's ~1M-token window
> (`domain.LargeContextWindowTokens`), **not** "% toward auto-compaction." A small
> conversation reads ~1% on the gauge while still being far below the 60K compact
> threshold — the two numbers measure different things.

## Model errors (rate limit, quota, unavailable)

When a turn can't reach a usable model response, the assistant ends the turn with a short,
stable reply rather than a raw provider blob. The mapping lives in `classifyStreamError`
([`internal/agent/session.go`](../internal/agent/session.go)); the error types are in
[`internal/models/errors.go`](../internal/models/errors.go).

| What happened | Reply you see | Code | Notes |
|---|---|---|---|
| Provider rate-limit / quota exceeded (HTTP 429), after the retry budget is spent | `Model rate-limited: …` | `MODEL_RATE_LIMITED` | Raises a **model-health badge** in the cockpit. The provider's `Retry-After` is recorded internally but not shown today. |
| No API key, or offline mode | `Model unavailable: …` | `FIREWORKS_UNAVAILABLE` | Set `FIREWORKS_API_KEY` (or clear `DAINTREE_ASSISTANT_OFFLINE`). |
| You pressed Escape to cancel the turn | `Turn cancelled` | `CANCELLED` | A clean stop, not a failure — the loop treats it as such. |
| Any other model/transport error | `Model error: …` | — | The underlying error text is appended. |

**Rate limits retry first.** A 429 isn't surfaced immediately — the model layer retries
with backoff (`internal/models/reliability.go`); the `Model rate-limited:` reply only
appears once that budget is exhausted. The health badge **clears automatically** on the
next successful model usage, so once the provider recovers, the next good turn removes it.
Read-only Daintree tools are the only calls auto-retried on a transient transport blip;
mutations are single-shot so a retry can never double-apply.

## See also

- Environment variables (model overrides, tier, offline, state dir) — [`README.md`](../README.md#environment-variables).
- Full-fidelity tracing for debugging — set `DAINTREE_ASSISTANT_DEBUG_LOG=1`
  ([`README.md`](../README.md#debug-logging)).
- The turn loop and where compaction sits in it — [`ARCHITECTURE.md`](ARCHITECTURE.md).
