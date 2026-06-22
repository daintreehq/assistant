# Port-spec: `daemon` subsystem (scheduler + watchers)

Authoritative reference for the Go rewrite. Sourced from the TypeScript:
`src/daemon/scheduler.ts`, `src/daemon/watcherEngine.ts`, `src/daemon/prWatcherEngine.ts`,
`src/watcherCadence.ts`, `src/reliability.ts`, `src/schemas.ts`,
`src/storage/db.ts`, `src/queue.ts`, `src/ui/hooks/useTerminalPreview.ts`.

The daemon drives all autonomous work in-process: a 3s tick fires due **timers** and
runs due **watchers** (terminal FSM + PR poller), persisting everything to SQLite so it
survives restarts and performs sleep catch-up. State is in SQLite + ticks are idempotent,
so the architecture is ready to split into a detached process later. Foreground-only:
ticks only while the assistant is open; watchers are session-scoped (cancelled on the next
`Db` open). All sub-threads publish to the **attention queue** rather than interrupting the
main thread.

---

## 1. Cadence / timing constants (`src/watcherCadence.ts` + `src/reliability.ts`)

| Constant | Value | Meaning | Notes |
|---|---|---|---|
| `SCHEDULER_TICK_MS` | `3_000` | Scheduler tick interval; also the FLOOR for any supervisor cadence | A supervisor watcher cannot check faster than the tick. |
| `SUPERVISOR_DEFAULT_CADENCE_MS` | `3_000` | Default cadence for supervisor watchers (CLI-spawned workers) | Kept equal to the tick so the stored cadence is honoured exactly. |
| `MONITOR_DEFAULT_CADENCE_MS` | `120_000` | Default cadence for user-created background ("monitor") watchers | Slow to keep classification cost low. |
| `PR_WATCHER_CADENCE_MS` | `60_000` | Fixed cadence for `pr_state` watchers (NOT user-configurable) | 60 req/hr/watcher, inside GitHub/GitLab auth limits. |
| `WATCHER_SPAWN_GRACE_MS` | `20_000` | Grace after watcher creation: an absent-from-getStatus terminal that has never been seen is "still registering", not exited | Once observed once, absence is a real exit regardless of this window. |
| `RATE_LIMIT_COOLDOWN_MS` | `60_000` | (`watcherEngine.ts`, private) min next-check delay once a terminal is seen rate-limited | `nextCheckAt = now + max(cadenceMs, RATE_LIMIT_COOLDOWN_MS)`. |
| `MCP_READ_TIMEOUT_MS` | `20_000` | Per-attempt timeout for read-only MCP calls (watcher reads) | Applied ONLY to idempotent reads, never mutations. |
| `MCP_READ_RETRY_POLICY` | `{maxRetries:2, baseDelayMs:250, maxDelayMs:2_000}` | Retry budget for read-only MCP calls | `maxRetries` = ADDITIONAL attempts after the first (2 ⇒ up to 3 total). |
| `RATE_LIMIT_TAIL_WINDOW` | `1500` | Largest tail slice scanned for a rate-limit signature | Bounded so a stale deep line can't keep re-flagging. |
| `JUDGE_CONFIDENCE_FLOOR` | `0.6` | (`watcherEngine.ts`, private) min judge confidence for a `modelJudge` condition to fire AND for an acceptance-judge YES/NO to be "confident" | A confident YES below this is too uncertain to act on. |
| `MAX_TERMINALS` | `4` | (`useTerminalPreview.ts`, UI) cap on preview cards | See §9 — UI-only, may be re-homed or dropped. |
| `POLL_MS` | `2500` | (`useTerminalPreview.ts`, UI) preview poll interval | UI-only. |
| Preview output `maxLines` | `40` | (`useTerminalPreview.ts`) `terminal.getOutput` line cap for previews | tail sliced to last `3000` chars. |

`MCP_READ_OPTS` (in both watcher engines) = `{ timeoutMs: MCP_READ_TIMEOUT_MS, retries: MCP_READ_RETRY_POLICY.maxRetries }`.

### Other reliability constants (used by the model/MCP layers, relevant if the daemon shares the retry helper)
| Constant | Value |
|---|---|
| `MODEL_RETRY_POLICY` | `{maxRetries:3, baseDelayMs:500, maxDelayMs:10_000}` |
| `MODEL_REQUEST_TIMEOUT_MS` | `60_000` |
| `MODEL_STREAM_TIMEOUT_MS` | `300_000` |
| `MAX_RETRY_AFTER_MS` (private) | `30_000` |

`fullJitterDelay(attempt, baseMs, maxMs)` = `floor(rand() * (min(maxMs, baseMs*2^max(0,attempt)) + 1))`, 0-based attempt (full jitter, not equal/decorrelated).

---

## 2. Scheduler (`src/daemon/scheduler.ts`)

### 2.1 `SchedulerDeps` interface
| Field | Type | Note |
|---|---|---|
| `db` | `Db` | persistence |
| `queue` | `Queue` | attention queue |
| `router` | `ModelRouter` | small-model classification (passed through to watchers via `ctxFor`) |
| `registry` | `ToolRegistry` | for `call_safe_tool` timer payloads |
| `ctxFor` | `(actor, actorId?) => ToolContext` | builds a `ToolContext` for a non-interactive actor (`"watcher"`/`"timer"`) |
| `tickMs?` | `number` | defaults to `SCHEDULER_TICK_MS` |
| `onAttention?` | `(events: QueueEvent[]) => void` | called with newly-created attention+ events after each tick |

### 2.2 `Scheduler` class — behavior
- **Private state**: `timer` (interval handle), `running: boolean`, `current?: Promise<void>` (in-flight tick handle), `tickMs`, mutable `onAttention`.
- `setOnAttention(cb?)` — replace the attention callback (UI remount rebinds a fresh one).
- `start()` — idempotent (returns if already started). Sets an interval at `tickMs`. **No-overlap guard**: on each interval, if `running` is true it returns WITHOUT replacing `this.current` (a skipped tick must not overwrite the handle `drain()` awaits — otherwise `stop()/drain()` could return before the real tick releases MCP/DB). Otherwise `this.current = this.tick().catch(()=>{})`. Calls `timer.unref()` so the scheduler alone does not keep the process alive.
  - **Go mapping**: a goroutine with a `time.Ticker`; "no-overlap" = a `running` atomic/mutex-guarded bool; `current` = a channel or `sync.WaitGroup` the drain path waits on. `unref` has no Go analogue (drop it; lifecycle managed by context cancellation).
- `stop()` — clears the interval, sets `timer = undefined`. (Does NOT wait — call `drain()` after.)
- `drain()` — `await this.current`. Call after `stop()` before tearing down deps.
- `tick(now = Date.now())` — **one pass**. Re-checks `running` (returns if set), sets `running=true`, and in a `finally` resets `running=false`. Body:
  1. For each `db.dueTimers(now)`: call `fireTimer(t, now)` inside a per-timer `try/catch` that swallows. **Why**: `fireTimer`'s inner try/catch only covers payload execution; `reschedule()` and publish run OUTSIDE it, so a SQLite throw there would otherwise abort the whole loop — starving later timers AND skipping `notify()`.
  2. For each `db.dueWatchers(now)`: per-watcher `try/catch` that swallows (including a throwing `ctxFor()`, which sits OUTSIDE a promise `.catch`). Route by `w.kind`:
     - `"terminal"` → `runTerminalWatcherCheck(w, ctxFor("watcher", w.id))`
     - `"pr_state"` → `runPrWatcherCheck(w, ctxFor("watcher", w.id))`
     - `default` (unknown) → fail closed: `db.updateWatcher(w.id, {status:"error", lastCheckedAt: now})`. **Why fail closed**: a misrouted unknown kind would silently reschedule forever (false supervision) or run the terminal check against a record with no terminal targets.
  3. `notify()`.
- `notify()` (private) — if no `onAttention`, return. Selects fresh attention+ events: `queue.digest({ severityAtLeast: "attention", notifiedIsNull: true, maxItems: 20 })`. If non-empty: call `onAttention(fresh)` inside a try/catch (delivery failure must NOT skip marking — else the same events re-fire forever), then `queue.markNotified(fresh.map(e=>e.id))` **regardless** of delivery success.
  - **Ordering guarantee**: events are pushed exactly once — select never-notified, then stamp. Survives the dedupe path (which pins `createdAt`) and still catches a below-threshold event that later escalates to attention+.

### 2.3 `fireTimer(rec, now)` (private)
Parse `payload = JSON.parse(rec.payloadJson)` and `target = rec.targetJson ? JSON.parse(rec.targetJson) : undefined`. The payload shape:
```
{ type: TimerRecord.payloadType, message?: string, checkPrompt?: string,
  toolCall?: { toolName: string, args: unknown } }
```
- **Corrupt-row handling**: if JSON.parse throws → publish `{source:"timer", severity:"error", title: rec.title, summary: "Disabling corrupt timer <id>: <err>"}` (NO dedupeKey), then `db.updateTimer(rec.id, {status:"fired", lastFiredAt: now})`, then `db.revokeGrantsByActor(rec.id, now)`, and return. **Why**: a corrupt row would throw every tick and starve later timers; a disabled timer can never fire again so release its scoped grants.
- `dedupeKey = "timer:" + rec.id` — STABLE across every firing (NOT keyed by runCount) so a repeating timer updates one live inbox item in place. Shared by success and catch paths.
- `payloadType = payload.type ?? rec.payloadType` — dispatch on the JSON's `type`, falling back to the typed DB column (authoritative for hand-written/legacy rows whose JSON omits `type`).
- Payload dispatch (inside a try; on throw → publish `severity:"error"`, summary `"Timer check failed: <err>"`, with `target` + `dedupeKey`):
  - `"enqueue"` → publish `{severity:"attention", title: rec.title, summary: payload.message ?? rec.title, target, dedupeKey}`. **Why attention not info**: a scheduled enqueue is a user reminder; `info` sits below the surfacing threshold and reminders silently never appeared.
  - `"run_check"` (**deprecated, legacy rows only**) → publish `{severity:"attention", title: rec.title, summary: "Reminder (run_check is deprecated — use a watcher to observe real state): " + (payload.checkPrompt ?? rec.title), target, dedupeKey}`. **Why**: `run_check` never observed real state (no terminal/git/queue), so verdicts were pure priors. No longer creatable; legacy rows fire as plain reminders.
  - `"call_safe_tool"` + `payload.toolCall` → `res = await registry.dispatch(toolCall.toolName, toolCall.args, ctxFor("timer", rec.id))`. If `res.error?.code !== "CONFIRMATION_REQUIRED"`: publish `{severity: res.ok ? "info" : "error", title: rec.title, summary: res.summary, target, dedupeKey}`. **Why the CONFIRMATION_REQUIRED skip**: a confirm-required tool denied to a non-interactive actor is an expected structural outcome the registry already surfaces; don't double-raise a timer error.
- Always finishes with `reschedule(rec, now)`.

### 2.4 `reschedule(rec, now)` (private) — repeat / max-runs / repeat-until / sleep catch-up
```
runCount   = rec.runCount + 1
repeatDone = !rec.repeatEveryMs
          || (rec.maxRuns != null && runCount >= rec.maxRuns)
          || (rec.repeatUntil != null && now + rec.repeatEveryMs > rec.repeatUntil)
```
- **`repeatDone`** → `db.updateTimer(rec.id, {status: rec.repeatEveryMs ? "done" : "fired", runCount, lastFiredAt: now})` then `db.revokeGrantsByActor(rec.id, now)`. (One-shot timers end as `"fired"`; finished repeats end as `"done"`.)
- **Else (continue repeating)** → `db.updateTimer(rec.id, {fireAt: now + rec.repeatEveryMs, runCount, lastFiredAt: now, status: "scheduled"})`.
- **Sleep catch-up (single-fire)**: next fire is scheduled relative to **NOW**, not the missed deadline, so a long sleep produces ONE catch-up fire, not a storm. (Because `dueTimers` returns each scheduled row at most once per tick and `reschedule` moves `fireAt` forward from `now`.)
- **repeat-until edge**: stop if the NEXT fire would land PAST the deadline — no extra fire at/after the deadline.

---

## 3. Persisted records & enums (`src/schemas.ts`)

### 3.1 `TimerRecord`
| Field | Type | Note |
|---|---|---|
| `id` | string | `tmr_<uuid8>` (`randomUUID().slice(0,8)`) |
| `title` | string | |
| `fireAt` | number | wall-clock ms |
| `repeatEveryMs?` | number | absent ⇒ one-shot |
| `repeatUntil?` | number | wall-clock ms deadline |
| `maxRuns?` | number | |
| `runCount` | number | starts 0 |
| `payloadType` | `"enqueue" \| "run_check" \| "call_safe_tool"` | authoritative payload kind (DB column) |
| `payloadJson` | string | |
| `targetJson?` | string | serialized `EventTarget` |
| `status` | `"scheduled" \| "fired" \| "cancelled" \| "done"` | |
| `createdAt` | number | |
| `lastFiredAt?` | number | |

### 3.2 `WatcherRecord`
| Field | Type | Note |
|---|---|---|
| `id` | string | `wch_<uuid8>` |
| `kind` | `"terminal" \| "pr_state"` | scheduler routes by this; unknown ⇒ `error` |
| `title` | string | |
| `goal` | string | fed to the small-model prompts |
| `targetsJson` | string | JSON `string[]`. terminal: terminalIds. pr_state: a single display label `"PR #N"` (column stays non-null) |
| `cadenceMs` | number | supervisor cadence floored to `SCHEDULER_TICK_MS` at insert |
| `isSupervisor?` | boolean | stored as 0/1 INTEGER; supervisor ⇒ fast cadence + ending outcome promoted to attention |
| `modelTier` | `"small"\|"medium"\|"large"` | watcher classification uses this tier |
| `startAfterMs?` | number | |
| `stopAfterMs?` | number | timeout budget (age-based) |
| `stopWhenJson?` | string | serialized `WatchCondition` |
| `alertWhenJson?` | string | serialized `WatchCondition` |
| `optionsJson?` | string | engine-managed per-tick state (see §6.4 / §7.2) |
| `status` | `"created"\|"active"\|"paused"\|"condition_met"\|"timeout"\|"cancelled"\|"error"` | `dueWatchers` selects `'active'` only |
| `lastClassification?` | string | |
| `lastEpistemicKind?` | `EpistemicKind` | stored authoritatively by the engine |
| `lastCheckedAt?` | number | |
| `nextCheckAt` | number | NOT NULL; `dueWatchers` orders by it |
| `createdAt` | number | |

### 3.3 Enums / value sets (keep byte/string-exact — they persist + cross the model/queue wire)
- `ModelTier`: `["small","medium","large"]`
- `AgentState` (Daintree mirror): `["idle","working","waiting","directing","completed","exited"]`
- `Severity`: `["debug","info","attention","urgent","blocked","done","error"]` (NOTE: enum *declaration* order differs from severity weight order — see §6.6).
- `EventSource`: `["timer","terminal_watcher","worktree_watcher","pr_watcher","workflow","model_worker","system","user"]` (`worktree_watcher` kept for legacy events; the worktree watcher KIND was removed).
- `EpistemicKind`: `["observed","inferred","unverified"]`
- `WatcherClassification`: `["no_change","still_working","waiting_for_input","permission_prompt","command_failed","tests_failed","tests_passed","merge_conflict","completed_success","completed_unverified","completed_unknown","terminal_exited","rate_limited","needs_large_model","unknown"]`. **`completed_unverified` is set ONLY by the engine's verification pass, never by the small model** (absent from the model-facing prompt enum).
- `VerificationVerdict`: `["verified","failed","unknown"]`. Deserialization uses `.catch("unknown")` so a legacy `"clean"/"dirty"` blob → `"unknown"`, never a false `verified`.

### 3.4 `WatchCondition` DSL (recursive union; `.strict()` per leaf)
```
{ stateIs: AgentState }
{ runtimeStatusIs: "running" | "exited" }
{ contains: string }                  // non-empty, non-whitespace (rejected at create otherwise)
{ regex: string }                     // non-empty AND must compile (validated at create)
{ noOutputForMs: number }             // int, positive (rejects Infinity → would persist null → fire immediately)
{ modelJudge: string }                // non-empty, non-whitespace
{ all: WatchCondition[] }             // min length 1
{ any: WatchCondition[] }             // min length 1
{ not: WatchCondition }               // single child, NOT an array
```
Creation-time validation rejects degenerate conditions (empty `contains`/`all`/`any`, invalid/empty regex, non-positive `noOutputForMs`) so a watcher that can never do its job is never persisted.

### 3.5 Event / target / action types
- `EventTarget` (`.strict()`): `{ projectId?, worktreeId?, terminalId?, workflowRunId? }` all optional strings.
- `RecommendedAction` (`.strict()`): `{ label: string, toolName: string, args?: unknown, risk?: RiskClass, requiresConfirmation?: boolean }`.
- `QueuePublishArgs` (`.strict()`): `{ source: EventSource, severity: Severity, title, summary, target?, evidence?: string[], recommendedActions?: RecommendedAction[], dedupeKey?, ttlMs?, epistemicKind? }`.
- `QueueEvent` (DB row shape): adds `id` (`evt_<uuid8>`), `createdAt`, `updatedAt?` (advances on dedupe bump; `createdAt` stays fixed), `expiresAt?`, `resolvedAt?`, `count` (dedupe collapse count).

### 3.6 Model-call output schemas
- `WatcherVerdict` (`.strict()`): `{ classification: WatcherClassification, confidence: number[0..1], summary: string, evidence: string[] (default []), recommendedAction: enum ["none","focus_terminal","ask_user","send_input","spawn_helper","open_review"] (default "none") }`.
- `ModelJudgeAnswer` (`.strict()`): `{ reason: string, confidence: number[0..1], matched: boolean }`. **Field order matters**: `reason` BEFORE `matched` — emitting the rationale first gives the small model implicit chain-of-thought, improving the boolean. Preserve this order in the Go JSON schema / struct tags.
- `VerificationResult` (`.strip()`): `{ verdict: VerificationVerdict (catch "unknown"), hasGitChanges: boolean, changedFiles: int≥0 (default 0), changedFileList: string[] (default []), gitSummary: string, acceptanceCriteria?: string, criteriaMetSummary?: string, unresolvedWarnings: string[] (default []) }`.
- `VERIFICATION_EVIDENCE_PREFIX = "verification:"` — an evidence string of `"verification:" + JSON.stringify(VerificationResult)` carries the bundle. **Keep this exact prefix** — the conductor parses it.

---

(Continued in §4+ below.)

---

## 4. SQLite contract (`src/storage/db.ts`) — keep table/column names byte-stable

### 4.1 `timers` table
```sql
CREATE TABLE IF NOT EXISTS timers (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  fireAt INTEGER NOT NULL,
  repeatEveryMs INTEGER,
  repeatUntil INTEGER,
  maxRuns INTEGER,
  runCount INTEGER NOT NULL DEFAULT 0,
  payloadType TEXT NOT NULL,
  payloadJson TEXT NOT NULL,
  targetJson TEXT,
  status TEXT NOT NULL DEFAULT 'scheduled',
  createdAt INTEGER NOT NULL,
  lastFiredAt INTEGER
);
```

### 4.2 `watchers` table
```sql
CREATE TABLE IF NOT EXISTS watchers (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  goal TEXT NOT NULL,
  targetsJson TEXT NOT NULL,
  cadenceMs INTEGER NOT NULL,
  isSupervisor INTEGER NOT NULL DEFAULT 0,
  modelTier TEXT NOT NULL,
  startAfterMs INTEGER,
  stopAfterMs INTEGER,
  stopWhenJson TEXT,
  alertWhenJson TEXT,
  optionsJson TEXT,
  status TEXT NOT NULL DEFAULT 'created',
  lastClassification TEXT,
  lastEpistemicKind TEXT,
  lastCheckedAt INTEGER,
  nextCheckAt INTEGER NOT NULL,
  createdAt INTEGER NOT NULL
);
```
`isSupervisor` is stored 0/1; coerce back to bool on read (`rowToWatcher`).

### 4.3 `events` table (queue) + indexes
```sql
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, source TEXT NOT NULL, severity TEXT NOT NULL,
  title TEXT NOT NULL, summary TEXT NOT NULL, targetJson TEXT, evidenceJson TEXT,
  recommendedActionsJson TEXT, dedupeKey TEXT, epistemicKind TEXT,
  createdAt INTEGER NOT NULL, updatedAt INTEGER, notifiedAt INTEGER,
  expiresAt INTEGER, resolvedAt INTEGER, count INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_events_open ON events (resolvedAt, severity, createdAt);
CREATE INDEX IF NOT EXISTS idx_events_dedupe ON events (dedupeKey, resolvedAt);
```

### 4.4 DB methods the daemon depends on
| Method | SQL / behavior |
|---|---|
| `dueTimers(now)` | `SELECT * FROM timers WHERE status='scheduled' AND fireAt<=? ORDER BY fireAt`. Each due row returned at most once/tick (key to sleep-catchup single-fire). |
| `updateTimer(id, patch)` | dynamic UPDATE, columns allowlisted by `TIMER_UPDATE_COLS` = {title, fireAt, repeatEveryMs, repeatUntil, maxRuns, runCount, payloadType, payloadJson, targetJson, status, lastFiredAt}. `id`/`createdAt` immutable. |
| `dueWatchers(now)` | `SELECT * FROM watchers WHERE status='active' AND nextCheckAt<=? ORDER BY nextCheckAt`. |
| `updateWatcher(id, patch)` | allowlist `WATCHER_UPDATE_COLS` = {title, goal, targetsJson, cadenceMs, isSupervisor, modelTier, startAfterMs, stopAfterMs, stopWhenJson, alertWhenJson, optionsJson, status, lastClassification, lastEpistemicKind, lastCheckedAt, nextCheckAt}. |
| `insertTimer` / `insertWatcher` | generate `tmr_`/`wch_` ids; insert supervisor cadence floored to `SCHEDULER_TICK_MS`. |
| `revokeGrantsByActor(actorId, now=Date.now())` | `UPDATE automation_grants SET revokedAt=? WHERE actorId=? AND revokedAt IS NULL`; returns rows changed. Called whenever a timer/watcher reaches a terminal state. |
| `cancelStaleWatchers(now)` | run once per `Db` open (session boundary). Three statements: (1) revoke watcher grants for non-terminal watchers; (2) `UPDATE watchers SET status='cancelled' WHERE status IN ('active','created','paused')`; (3) `UPDATE events SET resolvedAt=? WHERE resolvedAt IS NULL AND source IN ('terminal_watcher','worktree_watcher','pr_watcher')`. **Why**: watchers are session-scoped — a new session never inherits a prior session's watchers. |
| `upsertEvent` | dedupe by `dedupeKey` over OPEN, non-expired events. On a hit: `count=count+1`, refresh `title/summary/severity/recommendedActions(overwrite→null if none)/epistemicKind(fallback to existing)/updatedAt/expiresAt`; **NEVER touch `createdAt`** (the scheduler's "is this new?" keys on `createdAt`). `evidence` falls back to existing when omitted (so a deduped completion poll never loses its latest `VerificationResult`). |

### 4.5 Queue methods
- `digest(opts)` → `db.listEvents(opts)`. `notify()` uses `{severityAtLeast:"attention", notifiedIsNull:true, maxItems:20}`.
- `markNotified(ids)` → stamps `notifiedAt`.
- `publish(args)` → validates `QueuePublishArgs`, computes `expiresAt = ttlMs ? now+ttlMs : undefined`, `upsertEvent`.
- Severity SQL ordering expression (`SEV_CASE`): `debug=0, info=1, done=2, attention=3, blocked=4, urgent=5, error=6, ELSE 1`. **Matches `SEVERITY_WEIGHT`** in the engine — keep both in sync.

---

## 5. PR watcher engine (`src/daemon/prWatcherEngine.ts`)

Polls `forge.getPR` deterministically — **NO model call**. The forge exposes no
review-thread read action, so it observes only: PR state (open/closed/merged), draft flag,
`updatedAt`. Last-seen state persisted in `WatcherRecord.optionsJson` → each tick is a pure
compare-and-publish.

### 5.1 Exported types
- `PrWatcherOptions`: `{ cwd?: string, prNumber: number, lastState?: string, lastIsDraft?: boolean, lastUpdatedAt?: string }`.
- `PrCheckResult`: `{ status: WatcherRecord["status"], transition?: "state_change"|"draft_ready"|"activity", published: boolean, state?: string }`.
- (internal) `PrFields`: `{ state?: "open"|"closed"|"merged", isDraft?: boolean, updatedAt?: string, title?: string }`.

### 5.2 `runPrWatcherCheck(rec, ctx): Promise<PrCheckResult>`
1. `now = Date.now()`. Parse `options` from `rec.optionsJson`; require a numeric `prNumber`. **Corrupt** → `updateWatcher(status:"error", lastCheckedAt:now)`, `revokeGrantsByActor`, publish `{source:"pr_watcher", severity:"error", title:"<title>: watcher disabled", epistemicKind:"unverified"}`, return `{status:"error", published:false}`.
2. **Timeout wins before any read**: if `rec.stopAfterMs && now-rec.createdAt >= rec.stopAfterMs` → `updateWatcher(status:"timeout")`, `revokeGrantsByActor`, publish `{severity:"info", epistemicKind:"observed", dedupeKey:"pr_watcher:<id>:timeout"}`, return `{status:"timeout", published:true}`.
3. `reschedule()` helper = `updateWatcher({lastCheckedAt:now, nextCheckAt:now+PR_WATCHER_CADENCE_MS, status:"active"})`.
4. **Transient guards (never stop on a hiccup, just reschedule + `{status:"active", published:false}`)**: MCP disconnected; `forge.getPR` throws; `res.isError`; or `extractPrFields` returns undefined (unrecognizable payload).
5. `forge.getPR` args: `{ cwd: options.cwd ?? ctx.projectPath, prNumber: options.prNumber }`, signal `undefined`, opts `MCP_READ_OPTS`.
6. **Diff vs baseline**:
   - `firstObservation = options.lastState === undefined`
   - `stateChanged = fields.state !== undefined && fields.state !== options.lastState`
   - `becameTerminal = (state==="merged"||"closed") && (firstObservation || stateChanged)`
   - `draftReady = options.lastIsDraft===true && fields.isDraft===false`
   - `activity = !becameTerminal && !draftReady && !stateChanged && advanced(lastUpdatedAt, fields.updatedAt)` (only when nothing more specific fired — a state change/merge also bumps updatedAt).
7. **Publish (priority order: terminal > draftReady > activity)**:
   - `becameTerminal`: `{severity:"attention", title:"<t>: PR #N <merged|closed>", summary, evidence:["PR #N","state: <state>"], epistemicKind:"observed", dedupeKey:"pr_watcher:<id>:state_change"}`; result `{status:"condition_met", transition:"state_change", published:true}`.
   - `draftReady`: `{severity:"attention", ..., evidence:["PR #N","draft: false"], dedupeKey:"pr_watcher:<id>:draft_ready"}`; result `{status:"active", transition:"draft_ready", published:true}`.
   - `activity`: `{severity:"info", ..., dedupeKey:"pr_watcher:<id>:activity"}`; result `{status:"active", transition:"activity", published:true}`.
8. **Persist**: `nextOptions = {...options, lastState: fields.state ?? options.lastState, lastIsDraft: fields.isDraft ?? options.lastIsDraft, lastUpdatedAt: fields.updatedAt ?? options.lastUpdatedAt}`. `updateWatcher({lastClassification: transition ?? "no_change", lastEpistemicKind:"observed", lastCheckedAt:now, nextCheckAt:now+PR_WATCHER_CADENCE_MS, optionsJson: JSON.stringify(nextOptions), status: result.status})`. If `status==="condition_met"` → `revokeGrantsByActor`.

### 5.3 Forge payload normalization (defensive, pure, never throws)
- `candidateObjects(res)`: collect `res.structuredContent` and `JSON.parse(res.text)` (if text parses), plus one-level unwrap under wrapper keys `["pr","pullRequest","mergeRequest","result","data"]`.
- `extractPrFields(res)`: for each candidate — `rawState = String(obj.state).toLowerCase()`; `merged = obj.merged===true || obj.merged_at!=null || obj.mergedAt!=null || rawState==="merged"`. Skip objects with neither a state nor a merged signal. Map: merged→`"merged"`; `"closed"`→`"closed"`; `"open"|"opened"`→`"open"`; any other recognized-but-unknown `state` → `continue` (it's an envelope field, fall through to the unwrapped PR). Draft = first defined of `isDraft, draft, work_in_progress, workInProgress`. `updatedAt = obj.updatedAt ?? obj.updated_at`. `title = obj.title`.
- `advanced(prev, next)`: both defined, both `Date.parse`-able, `next > prev`.

---

## 6. Terminal watcher engine (`src/daemon/watcherEngine.ts`) — pure helpers

### 6.1 `WatcherSignals`
`{ agentState?: string, runtimeStatus?: string ("running"|"exited"), waitingReason?: string ("prompt"|"question"), exitCode?: number, tail: string, msSinceOutput?: number, classification?: WatcherClassification, confidence?: number }`.

### 6.2 `MEANINGFUL` set (classifications worth interrupting the user)
`{ waiting_for_input, permission_prompt, command_failed, tests_failed, tests_passed, merge_conflict, completed_success, completed_unverified, completed_unknown, terminal_exited, rate_limited }`.

### 6.3 `TERMINAL_CLASS` set (watcher's job done → stop)
`{ completed_success, terminal_exited }`. **`completed_unverified` deliberately NOT here** — the agent reported completion but the worktree is dirty/unverified, so keep polling until a clean `completed_success` or the user resolves it. Note: an explicit `stopWhen:{stateIs:"completed"}` fires on the raw FSM state BEFORE the verification gate and stops regardless of verdict; to keep alive until clean, gate on the classification instead.

### 6.4 `SEVERITY_MAP: Record<WatcherClassification, Severity>`
| classification | severity |
|---|---|
| no_change | debug |
| still_working | debug |
| waiting_for_input | attention |
| permission_prompt | attention |
| command_failed | error |
| tests_failed | error |
| tests_passed | done |
| merge_conflict | blocked |
| completed_success | done |
| completed_unverified | attention |
| completed_unknown | info |
| terminal_exited | urgent |
| rate_limited | attention |
| needs_large_model | attention |
| unknown | info |

### 6.5 `evaluateCondition(cond, s: WatcherSignals, judgeResults?): boolean` (pure, exported)
Leaf handling: `stateIs`→`s.agentState===v`; `runtimeStatusIs`→`s.runtimeStatus===v`; `contains`→`s.tail.includes(v)`; `regex`→`new RegExp(v).test(s.tail)` (invalid regex caught → false); `noOutputForMs`→`s.msSinceOutput!==undefined && s.msSinceOutput>=v` (**undefined never trips — "not observed" is not "silence"**); `modelJudge`→lookup precomputed answer: `!!r && r.matched && r.confidence>=JUDGE_CONFIDENCE_FLOOR`; `all`→every; `any`→some; `not`→negate. **`modelJudge` leaves are NOT evaluated against the live model here** (would make it async/untestable) — answers are precomputed in `runModelJudges` and threaded in keyed by exact question string. A missing judge answer → false; `not:{modelJudge}` of a missing answer flips false→true (accepted wart, not three-valued logic).

### 6.6 `SEVERITY_WEIGHT` + `moreUrgent`
`{debug:0, info:1, done:2, attention:3, blocked:4, urgent:5, error:6}`. `moreUrgent(a,b)=weight[a]>weight[b]`. (Mirrors `SEV_CASE` SQL — keep in sync.)

### 6.7 `decideOutcome(args): CheckOutcome` (pure, exported)
`CheckOutcome = { classification, confidence, summary, evidence, epistemicKind, shouldPublish, severity, stop, stopReason? ("condition_met"|"timeout"|"terminal") }`.
```
sig          = {...signals, classification, confidence}
alertMatched = alertWhen ? evaluateCondition(alertWhen, sig, judgeResults) : false
stopMatched  = stopWhen  ? evaluateCondition(stopWhen,  sig, judgeResults) : false
changed      = classification !== previous
isTerminal   = TERMINAL_CLASS.has(classification)
shouldPublish = timedOut || alertMatched || stopMatched
             || (changed && MEANINGFUL.has(classification))
             || classification === "needs_large_model"
stop         = stopMatched || isTerminal || timedOut
stopReason   = stopMatched ? "condition_met" : timedOut ? "timeout" : isTerminal ? "terminal" : undefined
severity     = timedOut ? "attention" : SEVERITY_MAP[classification]
if ((alertMatched||stopMatched) && severity in {debug,info,done}) severity = "attention"
epistemicKind = classificationEpistemicKind(classification, usedModel)
```

### 6.8 `classificationEpistemicKind(classification, usedModel=false): EpistemicKind` (exported, in schemas.ts)
- `terminal_exited`, `waiting_for_input`, `rate_limited` → `usedModel ? "inferred" : "observed"` (dual-source classes).
- `permission_prompt`, `still_working`, `tests_failed`, `tests_passed`, `command_failed`, `merge_conflict`, `completed_success`, `completed_unverified`, `completed_unknown` → `"inferred"`.
- default (`no_change`, `unknown`, `needs_large_model`, unrecognized) → `"unverified"`.

### 6.9 Other pure helpers
- `hashTail(s): string` — stable 32-bit hash, base36: `h=0; for c: h=(h<<5)-h+c; h|=0; return (h>>>0).toString(36)`. **Reproduce exactly** (persisted in `outHash`, compared across ticks).
- `nextOutputState(prev, tail, now)` → `{state:{prev:prev?.prev, outHash, outAt}, msSinceOutput: max(0, now-outAt)}` where `changed = !prev || prev.outHash!==outHash`, `outAt = changed ? now : prev?.outAt ?? now`.
- `collectModelJudges(...conds): string[]` — every distinct `modelJudge` question, first-seen order, deduped (recurses into `all`/`any` arrays and the single `not` child).
- `hasTextCondition(cond?): boolean` — true if any `contains`/`regex` leaf present (recursive). Drives deep-tail reads.
- `runtimeFromAgentState(agentState?)` → `undefined` if absent, `"exited"` if `==="exited"`, else `"running"`.
- `countChangedFiles(sc)` / `extractChangedFileList(sc)` / `deriveVerification(sc, text)` — git-pulse parsing (see §8).

---

## 7. Terminal watcher — MCP reads & async helpers

### 7.1 Exported read types
- `TerminalStatusEntry`: `{ terminalId, agentState?, waitingReason?, error?, recentOutput?, exitCode?, spawnedAt?, lastTransitionAt? }`. Integer fields validated with `Number.isInteger` (rejects NaN/Infinity/fractional/null/strings → undefined).
- `StatusBatch`: `{ ok: boolean, byId: Map<string, TerminalStatusEntry> }`. `ok=false` ⇒ the call failed (a missing id is NOT "closed"); `ok=true` + absent id ⇒ gone.
- `ReadOutputResult`: `{ok:true, value:string} | {ok:false, value:""}` — `ok:false` (read failed) is DISTINCT from `ok:true,value:""` (genuinely silent terminal). Never conflate (would falsely advance `noOutputForMs`).
- `ListedTerminal`: `{ agentState?, waitingReason?, exitCode? }`.
- `TerminalListResult`: `{ ok: boolean, byId: Map<string, ListedTerminal> }`. `ok=true` ONLY when the call succeeded AND returned a recognizable `terminals` array (even empty). A non-empty array yielding ZERO parseable ids → schema drift → `ok=false` (so a target absent from it is not falsely declared exited).

### 7.2 `readStatuses(ctx, terminalIds, includeOutput=false): StatusBatch`
- Empty ids → `{ok:true, byId:empty}`.
- One batched `terminal.getStatus` for ALL ids. args `{terminalIds}`; if `includeOutput`: `args.includeOutput = {lines: 50, stripAnsi: true}` (50 = Daintree-documented max). Threads `ctx.signal` (extraction polls supply it; watcher/timer contexts leave undefined).
- `res.isError` → `{ok:false}`.
- Parse the `terminals` array from BOTH `structuredContent` AND the `text` body via `parseMcpArray` (Daintree returns it in `text`, not structuredContent — reading structuredContent alone silently saw zero terminals every tick).
- Catch-all → `{ok:false}`.

### 7.3 `readOutput(ctx, terminalId, tailBytes=12000): ReadOutputResult`
`terminal.getOutput` args `{terminalId, maxLines: 200}`, threads `ctx.signal`. Content from `parseMcpString(out, "content")` (falls back to raw text body). `out.isError` → `{ok:false}` (guard runs BEFORE value use so error JSON never leaks as fake output). Returns `value.slice(-tailBytes)`. Catch → `{ok:false}`.

### 7.4 `readTerminalList(ctx): TerminalListResult`
`terminal.list` args `{}`. Merge `terminals` from `structuredContent.terminals` AND a JSON `text` body; key by `id` (fallback `terminalId`). Used to cross-check a terminal `getStatus` omitted (getStatus has been observed to drop live agent terminals that `terminal.list` still reports — id-namespace/scope gap). Never throws.

### 7.5 `runTerminalWatcherCheck(rec, ctx): Promise<CheckOutcome>` — the FSM (per-tick)
1. `now = Date.now()`. Parse `targets` (non-empty `string[]`), `stopWhen`, `alertWhen`, `options: WatcherOptions`. **Corrupt** → `updateWatcher(status:"error")`, `revokeGrantsByActor`, publish `{source:"terminal_watcher", severity:"error", title:"<t>: watcher disabled", epistemicKind:"unverified"}`, return a `stop:true, stopReason:"terminal"` outcome.
2. `judgeQuestions = collectModelJudges(alertWhen, stopWhen)` (deduped — a shared question costs one call).
3. `needsDeepTail = hasTextCondition(alertWhen) || hasTextCondition(stopWhen)`.
4. `timedOut = Boolean(rec.stopAfterMs && now-rec.createdAt >= rec.stopAfterMs)`.
5. `perTerminal = {...options.perTerminal}`.
6. **One batched** `readStatuses(ctx, targets, true)` if MCP connected, else `{ok:false, byId:empty}`.
7. If `statuses.ok && some target absent from byId` → `readTerminalList(ctx)` ONCE (`list`). (No separate isConnected gate — `statuses.ok` already implies connected; a separate gate would open a flap window.)
8. **Per terminal** compute `signals`, `classification` (init `"unknown"`, `confidence 0.4`), `summary`, `evidence`, `usedModel=false`. `entry = statuses.byId.get(terminalId)`. Branches:
   - **MCP disconnected** → `needs_large_model`, "Daintree MCP not connected; cannot read terminal."
   - **`statuses.ok && !entry`** (absent from getStatus) — cross-check `list`:
     - `listed` present (alive per terminal.list):
       - `exited` → `terminal_exited` conf 0.95, evidence `["agentState=exited (terminal.list)"]`, + nonzero exitCode evidence.
       - `waiting` → if `options.spawnMode==="explore" && waitingReason!=="question"`: `completed_success` conf 0.85 (explore is read-only; nothing to git-verify; gateCompletion would loop forever on a pre-existing dirty tree). Else `waiting_for_input` conf 0.9 (summary "asking a question" if `waitingReason==="question"`, else "waiting for input").
       - `completed` → `gateCompletion(ctx, options.verificationScope, ["agentState=completed (terminal.list)"], {rec, signals, acceptanceCriteria})`.
       - else (working but getStatus dropped it) → `readOutput(ctx, terminalId)` deep read; classify from content (see content path §7.6). On read failure: freeze prior output state, increment `readFailures`, `no_change` conf 0.4, skip model.
     - `!list || !list.ok` (inventory unreadable) → `no_change` conf 0.4 (CANNOT prove exit — original false-exit bug source; stay alive).
     - `!prevState?.seen && now-rec.createdAt < WATCHER_SPAWN_GRACE_MS` → `no_change` conf 0.5 ("not yet registered; will re-check").
     - else (absent from BOTH + past grace) → `terminal_exited` conf 0.9, evidence `["absent from terminal.getStatus and terminal.list"]`, signals `{agentState:"exited", runtimeStatus:"exited", tail:""}`.
   - **else (entry present)** — `agentState = entry.agentState`, `waitingReason = entry.waitingReason`:
     - Tail: prefer `entry.recentOutput` when `!needsDeepTail && recentOutput !== undefined` (empty string is a valid "no output yet" — fall back on `undefined`, not falsiness). Else deep `readOutput` (sets `readFailed = !read.ok`).
     - On `readFailed`: freeze prior output state, `readFailures++`, `outHash = prevState?.outHash`, leave `msSinceOutput` unset. Else `nextOutputState` advances; clear `readFailures` to 0.
     - `exited` → `terminal_exited` conf 0.95, + nonzero exitCode evidence (clean exit 0 silent; classification stays `terminal_exited`).
     - `waiting` → same explore-idle vs waiting_for_input logic as above.
     - `completed` → `gateCompletion(...)`.
     - `readFailed` (still working) → `no_change` conf 0.4, skip model.
     - `tail.trim().length>0` → content path (§7.6).
     - else → `no_change`, "No new output."
     - `entry.error` appended as evidence `status error: <error>` (does not override verdict).
9. **Judges**: if `judgeQuestions.length>0 && connected` → `runModelJudges(...)` against THIS terminal's signals. For each judge that `matched && confidence>=JUDGE_CONFIDENCE_FLOOR`, append evidence `judge[<question>]: <reason>`.
10. `decideOutcome({classification, confidence, summary, evidence, previous: prevState?.prev, signals, stopWhen, alertWhen, judgeResults, timedOut, usedModel})`.
11. Persist per-terminal state: `{...prev, prev: outcome.classification, seen: prevState?.seen || Boolean(entry) || Boolean(list?.byId.get(terminalId))}` (latch `seen` once reported anywhere).
12. **Publish** if `outcome.shouldPublish`. **Supervisor promotion**: if `rec.isSupervisor && outcome.stop && SEVERITY_WEIGHT[outcome.severity] < SEVERITY_WEIGHT.attention` → severity bumped to `"attention"` (so "the agent finished" — normally `done` — actually surfaces). Publish `{source:"terminal_watcher", severity, title:"<t>: <humanize(classification)>", summary: targets.length>1 ? "[<id>] <summary>" : summary, target:{terminalId}, evidence, epistemicKind, dedupeKey:"watcher:<rec.id>:<terminalId>", recommendedActions: recommendedActionsFor(classification, terminalId)}`.
    - `dedupeKey` keyed by terminal (NOT classification) so concurrent terminals stay distinct while one terminal's evolving state updates one inbox item.
    - `humanize(c)` = `c.replace(/_/g," ")`.
13. **Aggregate**: `headline` = most-urgent per-terminal outcome (by `SEVERITY_WEIGHT`; default `no_change/debug` when no terminals).
14. **Stop semantics (across ALL terminals, NOT the headline)**: `timeout` if any outcome timed out; else `condition_met` if any matched a stop condition; else `terminal` only if EVERY target stopped. `stop = stopReason !== undefined`.
15. **Rate-limit backoff**: `rateLimited = any outcome.classification==="rate_limited"`; `nextCheckAt = now + (rateLimited ? max(rec.cadenceMs, RATE_LIMIT_COOLDOWN_MS) : rec.cadenceMs)`.
16. `updateWatcher({lastClassification: headline.classification, lastEpistemicKind: headline.epistemicKind, lastCheckedAt: now, nextCheckAt, optionsJson: JSON.stringify({...options, perTerminal}), status: stop ? (stopReason==="timeout" ? "timeout" : "condition_met") : "active"})`. If `stop` → `revokeGrantsByActor(rec.id, now)`.
17. Return `{...headline, stop, stopReason}`.

### 7.6 Content-classify path (shared by both entry-present and list-fallback working branches)
`classifyKey = "<agentState>|<exitCode>|<outHash>"` (msSinceOutput EXCLUDED — noOutputForMs is deterministic, folding time in would re-invoke the model every tick).
1. `detectRateLimitSignature(tail.slice(-RATE_LIMIT_TAIL_WINDOW))` → `rate_limited` conf 0.9, evidence `["rate-limit signature in recent output"]` (deterministic, model-free).
2. else `prevState?.lastClassifyKey === classifyKey` → `no_change` conf 0.5 (skip model — identical inputs).
3. else `usedModel=true`; `classifyWithModel(rec, signals, ctx, prevState?.prev)`. **Latch `lastClassifyKey` ONLY when** `classification not in {unknown, completed_success, rate_limited}`. (Skip `unknown`: model error, retry. Skip `completed_success`: routes through gateCompletion whose git-clean check is a hidden input not in the key — latching would dedupe a demoted `completed_unverified` into `no_change` forever. Skip `rate_limited`: latching would drop the cooldown before recovery.) If `classification==="completed_success"` → route through `gateCompletion`.

### 7.7 `WatcherOptions` (persisted in `optionsJson`)
`{ perTerminal?: Record<terminalId, TerminalState>, verificationScope?: {worktreeId?}, spawnMode?: "edit"|"explore" (absent ⇒ treated as "edit"), acceptanceCriteria?: string }`.
`TerminalState`: `{ prev?: string, outHash?: string, outAt?: number, seen?: boolean, readFailures?: number, lastClassifyKey?: string }`.

### 7.8 Model calls
- `classifyWithModel(rec, signals, ctx, previous?)` → `ctx.router.json(rec.modelTier, {messages:[{system: WATCHER_SYSTEM_PROMPT},{user: buildWatcherUserPrompt({goal: rec.goal, agentState, runtimeStatus, lastOutputAt: msSinceOutput!==undefined ? "<floor(ms/1000)>s ago" : undefined, previous, tail})}], temperature:0}, WatcherVerdict)`. On throw → `{classification:"unknown", confidence:0.3, summary:"Could not classify...", evidence:[], recommendedAction:"none"}`.
- `runModelJudges(questions, rec, signals, ctx)` → for each question (in PARALLEL via Promise.all), `ctx.router.json(rec.modelTier, {messages:[{system: JUDGE_SYSTEM_PROMPT},{user: buildJudgeUserPrompt({question, goal, agentState, runtimeStatus, waitingReason, lastOutputAt, tail})}], temperature:0}, ModelJudgeAnswer)`. Per-question failure degrades to `{reason:"Could not evaluate the question.", confidence:0, matched:false}` (one bad question never aborts the map). Returns `Map<question, ModelJudgeAnswer>`. **Latency bounded by the slowest single call** — keep parallelism (Go: errgroup / goroutines + a result map under a mutex, or a buffered channel).

---

## 8. Completion verification gate (§ issue #83)

### 8.1 `deriveVerification(sc, text): VerificationResult` (pure, exported)
- `countChangedFiles(sc)`: first of `changedFiles|changed_files|fileCount|changeCount` as number→`max(0,floor(v))` or array→length; else sum of arrays/numbers in `staged|unstaged|untracked|modified|added|deleted`; else undefined.
- `extractChangedFileList(sc)`: string entries (or `{path}` objects) from `changedFiles|changed_files` then the grouped keys; deduped, first-seen order.
- `dirtyFlag` = first defined of `sc.isDirty`, `sc.dirty`, `!sc.clean`, `!sc.isClean`.
- `hasGitChanges`: dirtyFlag if defined; else `changedFiles>0` if defined; else text markers (**check DIRTY markers before CLEAN**: `/Changes not staged|Changes to be committed|Untracked files|modified:|new file:|deleted:|renamed:/i` → true; `/nothing to commit|working tree clean|no changes/i` → false).
- **Dirty wins**: `changedFiles>0` overrides a clean flag (a self-contradictory `{isDirty:false, changedFiles:3}` is never read as clean).
- `hasGitChanges===undefined` → verdict `"unknown"`, hasGitChanges false, "git state could not be determined from the project pulse".
- dirty → verdict `"unknown"` (a dirty tree is uncommitted work — NOT failure, NOT verified), gitSummary `"<count> uncommitted file change(s) in the worktree"`.
- clean → verdict `"verified"`, "working tree clean (no uncommitted changes)". **Never returns `"failed"`** — only the acceptance judge can confidently fail.

### 8.2 `runVerificationPass(ctx, scope?): VerificationResult` (exported)
MCP disconnected / `isError` / throw → `verdict:"unknown"` (`unverifiable(...)`). Else `git.getProjectPulse` args `{...(scope?.worktreeId ? {worktreeId} : {})}` opts `MCP_READ_OPTS`; `deriveVerification(structuredContent, text)`. (`git.getProjectPulse` is a read tool — no confirmation, safe from watcher context.)

### 8.3 `gateCompletion(ctx, scope, baseEvidence, gate?)` → `GatedCompletion {classification, confidence, summary, evidence}`
`verification = runVerificationPass(ctx, scope)`; `isClean = verdict==="verified" && !hasGitChanges`; `criteria = gate?.acceptanceCriteria?.trim()`.
- **No contract (legacy git-clean gate)**: clean → `completed_success` conf 0.85 ("worktree clean and verified"). Else → `completed_unverified` conf 0.8 (summary depends on hasGitChanges).
- **Contract present**: `verification.acceptanceCriteria = criteria`. Judge ONLY if `gate && connected && gate.signals.tail.trim().length>0` (empty tail = no evidence; judging nothing could harden a transport hiccup into a false "failed" → leave "unknown"). Question: `"Did the agent satisfy this acceptance contract for the task? Contract: <criteria>"` via `runModelJudges`. `confident = answer && answer.confidence>=JUDGE_CONFIDENCE_FLOOR`. `criteriaMetSummary = answer.reason`.
  - confident YES + isClean → verdict `verified`, `completed_success` conf `max(0.85, answer.confidence)`.
  - confident NO → verdict `failed`, `completed_unverified` conf `answer.confidence` (non-terminal — a later attempt can satisfy it).
  - else (met but dirty/unreadable, or not confident) → verdict `unknown`, `completed_unverified` conf 0.8.
- `finalizeGate` appends evidence `"verification:" + JSON.stringify(verification)` (final verdict, never a stale snapshot).
- Called from BOTH the FSM `completed` path AND the small-model `completed_success` path — the gate cannot be bypassed.

### 8.4 `recommendedActionsFor(classification, terminalId)`
- `waiting_for_input` / `permission_prompt` → `[{label:"Focus terminal", toolName:"terminal.focus", args:{terminalId}, risk:"ui", requiresConfirmation:false}]`.
- `completed_unverified` → `[{label:"Review completion", toolName:"terminal.focus", args:{terminalId}, risk:"ui", requiresConfirmation:false}]`.
- else undefined. (`terminal.focus` is the real UI tool; `open_review` is display-only.)

---

## 9. Terminal previews (`src/ui/hooks/useTerminalPreview.ts`) — UI-only

`TerminalPreview = {terminalId, watcherId?, title?, agentState?, runtimeStatus?, tail, updatedAt}`. Polls every `POLL_MS=2500`. Filters `kind==="terminal"` watchers, flattens targets, dedupes by terminalId (first watcher owns the card), caps at `MAX_TERMINALS=4`. Per terminal: parallel `terminal.getStatus {terminalIds:[id]}` + `terminal.getOutput {terminalId, maxLines:40}`; attribute status only when returned id matches; tail `content.slice(-3000)`. Best-effort (errors swallowed). **Raw scrollback shown to the human ONLY — never fed to the main model.** In Go+Bubble Tea this becomes a `tea.Cmd` tick (or a background goroutine emitting `tea.Msg`); the model integration boundary must be preserved.

---

## 10. External contracts to keep wire/schema-compatible
- **MCP tool names + args** (exact): `terminal.getStatus {terminalIds:string[], includeOutput?:{lines:50, stripAnsi:true}}`, `terminal.getOutput {terminalId, maxLines:200|40}`, `terminal.list {}`, `terminal.focus {terminalId}`, `git.getProjectPulse {worktreeId?}`, `forge.getPR {cwd, prNumber}`. Result parsing must read BOTH `structuredContent` AND the JSON `text` body (Daintree populates `text`).
- **SQLite**: table/column names in §4 (timers, watchers, events + indexes; `automation_grants` for grant revocation).
- **ID formats**: `tmr_<uuid8>`, `wch_<uuid8>`, `evt_<uuid8>` (first 8 chars of a UUID v4). Timestamps are epoch **milliseconds** (`Date.now()`), integers.
- **Dedupe keys** (stable strings — collapse inbox rows in place): `timer:<id>`, `watcher:<id>:<terminalId>`, `pr_watcher:<id>:state_change|draft_ready|activity|timeout`.
- **Evidence prefix**: `verification:` carries a JSON `VerificationResult` parsed by the conductor.
- **Enum string values** (§3.3) persisted in SQLite + crossing the model JSON wire — keep byte-exact.
- **Model JSON schemas** (`WatcherVerdict`, `ModelJudgeAnswer`) — field names + the `reason`-before-`matched` order.
- **Event sources**: `terminal_watcher`, `pr_watcher`, `timer` (+ legacy `worktree_watcher` in resolve/cancel filters).

---

## 11. Go mapping proposal
- **Package `daemon`**: `Scheduler` (struct with `*sql.DB` wrapper, `*Queue`, `Router`, `Registry`, `ctxFor`, `tickMs`, `running atomic.Bool`, `wg sync.WaitGroup`/`done chan`). `Start(ctx)`/`Stop()`/`Drain()`/`Tick(now time.Time)`. Use `time.NewTicker`; skip a tick when `running` is set (CompareAndSwap). `Drain` waits on the in-flight goroutine.
- **`daemon` also holds** `runTerminalWatcherCheck` + `runPrWatcherCheck` as functions taking a `*ToolContext`.
- **Package `verify`** (or keep in `daemon`): the pure helpers — `EvaluateCondition`, `DecideOutcome`, `DeriveVerification`, `NextOutputState`, `HashTail`, `CollectModelJudges`, `HasTextCondition`, `ClassificationEpistemicKind` — all unit-testable with no MCP/model. Keep them pure.
- **Types**: Go structs for `TimerRecord`, `WatcherRecord`, `WatcherSignals`, `CheckOutcome`, `VerificationResult`, etc. `WatchCondition` → a sealed-ish interface OR a single struct with optional pointer fields per variant + a discriminator method; serialize to the SAME JSON shape (one key per leaf). Enums → typed `string` constants with the exact values.
- **time**: use `time.Time`/`time.Duration` internally but PERSIST epoch-ms int64 (don't change the on-disk format). `Date.now()` → `time.Now().UnixMilli()`.
- **JSON**: `encoding/json`; for `VerificationResult.verdict` legacy `.catch("unknown")`, validate on unmarshal and coerce unknown values to `"unknown"`.
- **SQLite**: `modernc.org/sqlite` (pure Go, no cgo) or `mattn/go-sqlite3`. Reuse the existing column-allowlist pattern for dynamic UPDATEs (prevents identifier injection).
- **Retry/jitter**: port `fullJitterDelay` (use `math/rand`), `MCP_READ_OPTS`. `cenkalti/backoff` is an option but the hand-rolled full-jitter is small — port directly to match behavior.
- **Parallel judges**: `golang.org/x/sync/errgroup` (but degrade per-question instead of failing the group — collect results, never return the error) OR plain goroutines + `sync.Map`/mutex.
- **Regex**: Go `regexp` (RE2) — NOTE semantic difference from JS RegExp (no backreferences/lookahead). Conditions are validated at creation; document that a JS-only regex feature will be rejected. Catch-and-false on compile error in `EvaluateCondition` (compile lazily / cache).
- **`humanize`** = `strings.ReplaceAll(c, "_", " ")`.

---

## 12. DELETE / do-not-port (Node/Bun/React/OpenTUI-specific)
- `timer.unref()` — no Go analogue; lifecycle is `context.Context` cancellation.
- The React hook `useTerminalPreview.ts` as-is (`useEffect`/`useState`/`setInterval`) — re-implement as a Bubble Tea `tea.Cmd`/goroutine. Keep the constants (`MAX_TERMINALS=4`, `POLL_MS=2500`, `maxLines:40`, `slice(-3000)`) and the "never feed raw scrollback to the main model" invariant.
- `AbortSignal`/`abortableSleep`/`AbortError` plumbing → `context.Context` + `ctx.Done()`. (`ctx.signal` threaded into `readStatuses`/`readOutput` for extraction-poll cancellation → pass a `context.Context`.)
- OpenAI/MCP SDK-specific error predicates (`isRetriableModelError`, `APIError`, `McpError`/`ErrorCode`) — re-derive against whatever Go HTTP/MCP client is used; keep the retriable conditions (429, 5xx, connection errors, MCP RequestTimeout/-32001 & ConnectionClosed/-32000, and the transport regex `fetch failed|ECONNRESET|ETIMEDOUT|ECONNREFUSED|socket hang up|network error|timed out`).
- Zod schema objects (`.strict()`/`.strip()`/`.catch()`) — replace with Go struct + explicit validation; preserve the validation RULES (non-empty contains/regex, positive int noOutputForMs, regex compiles, etc.).
- `NodeJS.Timeout`, `setInterval`/`clearInterval` — `time.Ticker`.

## 13. Risks / non-obvious contracts (do not lose these)
1. **No-overlap drain handle** must not be overwritten by a skipped tick (else `Drain` returns early, tearing down MCP/DB under a live tick).
2. **`notify()` marks-notified regardless of delivery success** — else attention events re-fire every tick forever.
3. **Sleep catch-up = single fire**: repeating timers reschedule from `now`, not the missed deadline.
4. **`createdAt` is never refreshed on dedupe** — the "is this new?" notify check keys on it.
5. **`ok=false` (read failed) vs `ok=true,value=""` (silent)** must stay distinct everywhere — conflation falsely advances `noOutputForMs` and re-runs the classifier on stale input.
6. **getStatus omits live terminals** — a missing id is never proof of exit without a successful `terminal.list` cross-check (false-exit bug).
7. **Spawn grace**: never-seen terminal within 20s = still registering, not exited.
8. **Completion gate cannot be bypassed** — both FSM `completed` and model `completed_success` route through `gateCompletion`; never latch `completed_success` in `lastClassifyKey` (the git-clean check is a hidden input).
9. **`completed_unverified` is engine-only**, never in the model prompt enum; it keeps the watcher alive (not in `TERMINAL_CLASS`).
10. **Dirty-wins** in `deriveVerification`; verdict legacy `.catch("unknown")` must coerce old `clean`/`dirty` blobs to `unknown`.
11. **Supervisor ending-outcome promotion to `attention`** — without it a clean completion (`done`) never surfaces.
12. **Stop semantics computed across ALL terminals**, not the headline (one done terminal can't stop a multi-terminal watcher).
13. **Rate-limit cooldown** (`max(cadence, 60s)`) + never latching `rate_limited` so the cooldown lifts on recovery.
14. **`ModelJudgeAnswer` field order** (`reason` before `matched`) is a deliberate chain-of-thought lever.
15. **`hashTail` must be reproduced bit-exactly** (persisted, compared across ticks/sessions).
16. **PR watcher: timeout wins before any read; transient failures reschedule without publishing; `becameTerminal` fires on first observation too.**
17. **Grant revocation on every terminal state** (timer done/fired, watcher stop/error/timeout, PR condition_met/timeout/error) so a recycled actor id can't inherit a stale authorization.
18. **Session-scoped watchers**: `cancelStaleWatchers` on every `Db` open cancels leftover active/created/paused watchers and resolves their open events.
