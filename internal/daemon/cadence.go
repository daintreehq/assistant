// Package daemon drives all autonomous work in-process: a 3s scheduler tick
// fires due timers and runs due watchers (a terminal-supervision state machine
// and a deterministic PR poller), persisting everything to the store so the
// architecture survives restarts and performs sleep catch-up.
//
// Foreground-only: ticks run only while the assistant is open. Watchers are
// session-scoped (cancelled on the next Db open) and every sub-thread publishes
// to the attention queue instead of interrupting the main thread.
//
// Port of src/daemon/{scheduler,watcherEngine,prWatcherEngine}.ts +
// src/watcherCadence.ts + the rate-limit/MCP-read knobs from src/reliability.ts.
// See docs/port/daemon.md. The pure helpers (EvaluateCondition, DecideOutcome,
// DeriveVerification, NextOutputState, HashTail, CollectModelJudges,
// HasTextCondition) carry no MCP/model dependency and are unit-tested directly.
package daemon

// --- Cadence / timing constants (watcherCadence.ts) --------------------------

// SchedulerTickMS is the scheduler tick interval AND the floor for any
// supervisor cadence — a supervisor watcher cannot check faster than the tick.
const SchedulerTickMS int64 = 3_000

// SupervisorDefaultCadenceMS is the default cadence for supervisor watchers
// (CLI-spawned workers). Kept equal to the tick so the stored cadence is
// honoured exactly.
const SupervisorDefaultCadenceMS int64 = 3_000

// MonitorDefaultCadenceMS is the default cadence for user-created background
// ("monitor") watchers. Slow, to keep classification cost low.
const MonitorDefaultCadenceMS int64 = 120_000

// PRWatcherCadenceMS is the fixed cadence for pr_state watchers (NOT
// user-configurable): 60 req/hr/watcher, inside forge auth limits.
const PRWatcherCadenceMS int64 = 60_000

// WatcherSpawnGraceMS is the grace after watcher creation during which an
// absent-from-getStatus terminal that has never been seen is "still
// registering", not exited. Once observed once, absence is a real exit
// regardless of this window.
const WatcherSpawnGraceMS int64 = 20_000

// --- Watcher-engine private knobs (watcherEngine.ts / reliability.ts) --------

// rateLimitCooldownMS is the minimum next-check delay once a terminal is seen
// rate-limited: nextCheckAt = now + max(cadenceMs, rateLimitCooldownMS).
const rateLimitCooldownMS int64 = 60_000

// McpReadTimeoutMS bounds each read-only MCP call (watcher reads only — never a
// mutation). Exported so the integrator wiring the concrete MCP client onto the
// MCP seam (CallRead) can apply this exact bound per request rather than letting a
// read hang until the outer tick context.
const McpReadTimeoutMS int64 = 20_000

// McpReadMaxRetries is the ADDITIONAL attempts after the first for read-only MCP
// calls (2 ⇒ up to 3 total). Mirrors MCP_READ_RETRY_POLICY.maxRetries. Exported so
// the integrator can request the documented retry budget on the read-only seam.
const McpReadMaxRetries = 2

// rateLimitTailWindow is the largest tail slice scanned for a rate-limit
// signature, bounded so a stale deep line can't keep re-flagging.
const rateLimitTailWindow = 1500

// judgeConfidenceFloor is the minimum judge confidence for a modelJudge
// condition to fire AND for an acceptance-judge YES/NO to be "confident".
const judgeConfidenceFloor = 0.6

// itemDeadlineMS bounds a single timer/watcher job inside a tick. Each item runs
// on its own goroutine; this deadline is the backstop that guarantees a genuinely
// hung item (a wedged MCP read past its own retries, a stuck model judge) can't
// keep the pass's WaitGroup — and therefore notify() — blocked forever. It is
// deliberately generous (well above McpReadTimeoutMS × retries + a model judge) so
// it never truncates legitimate slow work; it only caps an indefinite hang.
const itemDeadlineMS int64 = 120_000

// --- Terminal-preview constants (useTerminalPreview.ts — UI only) ------------

// MaxTerminals caps the number of preview cards.
const MaxTerminals = 4

// PreviewPollMS is the preview poll interval.
const PreviewPollMS int64 = 2500

// previewMaxLines is the terminal.getOutput line cap for previews.
const previewMaxLines = 40

// previewTailBytes is the tail slice (last N chars) shown in a preview.
const previewTailBytes = 3000
