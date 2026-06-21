# Port Spec: Storage Subsystem (`src/storage/`)

Faithful Go (Bubble Tea CLI) port spec for the durable SQLite store of
`daintree-assistant`. Source of truth: `src/storage/db.ts` (1880 lines) and
`src/storage/sqliteDriver.ts`, with record/enum types from `src/schemas.ts`.

> **WIP branch — no back-compat.** This branch is a clean rewrite. The existing
> SQLite shape below is recorded as a **behavioral reference**, not a migration
> target. There is no old DB to preserve (`npm run db:reset` is the dev policy);
> the Go build MAY adopt a clean Go-native schema. Where behavior is load-bearing
> (dedupe semantics, session-boundary sweeps, FTS escaping, atomic grant consume),
> it is flagged "KEEP" and must be reproduced exactly.

---

## 1. What this subsystem is

A single per-project SQLite file (`state.db`) holding all durable daemon state:
timers, watchers, the attention-queue inbox (`events`), the tool-dispatch audit
trail, per-run event logs, conversation transcript, skill-selection diagnostics,
automation grants, the workflow ledger, agent-launch sagas, skill run state, and
cross-session project memories (with an FTS5 recall index).

Design properties (KEEP as a whole):

- **Append-heavy + auditable.** Every autonomous action carries an idempotency /
  dedupe key. Append-only tables are bounded by a retention sweep, not by callers.
- **Single-writer, synchronous.** The foreground-only daemon invariant guarantees
  one process owns the DB. The TS driver is synchronous (`DatabaseSync`); the code
  relies on no-interleaving for "atomic" check-and-decrement (grants) and
  check-and-set (events, memories) **without explicit transactions**.
- **Session boundary = DB open.** Construction is treated as a fresh session
  boundary: stale watchers cancelled, stale agent-launch sagas failed, retention
  swept. There is no shutdown hook — open is the single reliable invalidation point.
- **`*Json` columns** hold serialized JSON; the store generally leaves them raw
  for the tool layer to (de)serialize, except `events` and grant-authorization
  logic which parse inside the store.

### Runtime driver abstraction (`sqliteDriver.ts`) — **DELETE in Go**

TS picks `bun:sqlite` under Bun, `node:sqlite` under Node, behind a thin
`SqliteDatabase`/`SqliteStatement` interface (`exec`, `prepare`→`run|get|all`,
`close`; positional params only). The one behavioral normalization: `bun:sqlite`
`.get()` returns `null` on a miss, `node:sqlite` returns `undefined`; the Bun
wrapper coerces `null → undefined` because `db.ts` checks `=== undefined`.
**In Go this whole adapter disappears** — use one CGO-free SQLite library directly
(see §11). The `null`/`undefined` distinction collapses to Go's zero/`sql.Null*`.

---

## 2. PRAGMAs & connection setup (KEEP semantics)

Run once in the constructor, in this exact order (busy_timeout FIRST so it covers
the WAL transition's write lock):

| Order | PRAGMA | Value | Why |
|---|---|---|---|
| 1 | `busy_timeout` | `5000` ms | Generous retry budget for a single-writer local CLI; without it a contended lock fails at 0 ms. Per-connection (not persisted). |
| 2 | `journal_mode` | `WAL` | Concurrent read while writing; takes a write lock on a fresh file (hence busy_timeout first). |
| 3 | `foreign_keys` | `ON` | (No FKs declared today, but enabled.) |

Then: `exec(SCHEMA)` (all `CREATE … IF NOT EXISTS`), `migrate()`,
`cancelStaleWatchers()`, `cancelStaleAgentLaunches()`, then a try/catch-wrapped
`gcRetentionSweep(now, retention)`.

**`migrate()`** is keyed on `PRAGMA user_version`. Today it is a single no-op
baseline (`user_version` lands at 1); SCHEMA's `IF NOT EXISTS` builds everything.
Dev policy: hard-reset the file rather than chain migrations. **Go: a single
`schema.sql` exec + a version pragma is sufficient.** No migration framework needed.

---

## 3. Full SQLite SCHEMA (reference — every table/column/index/trigger)

Path: `~/.daintree/assistant-cli[/<projectIdDir>]/state.db` (see §9).
All timestamps are **Unix epoch milliseconds (INTEGER)**. Booleans stored as
INTEGER 0/1 (SQLite has no bool — `toSqlValue` coerces; `rowToWatcher` coerces
`isSupervisor` back). `id` columns are `<prefix>_<uuid8>` TEXT (see §8).

### 3.1 `timers`
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `tmr_…` |
| title | TEXT NOT NULL | |
| fireAt | INTEGER NOT NULL | wall-clock ms |
| repeatEveryMs | INTEGER | nullable |
| repeatUntil | INTEGER | nullable |
| maxRuns | INTEGER | nullable |
| runCount | INTEGER NOT NULL DEFAULT 0 | |
| payloadType | TEXT NOT NULL | `enqueue`\|`run_check`\|`call_safe_tool` |
| payloadJson | TEXT NOT NULL | |
| targetJson | TEXT | nullable JSON |
| status | TEXT NOT NULL DEFAULT 'scheduled' | `scheduled`\|`fired`\|`cancelled`\|`done` |
| createdAt | INTEGER NOT NULL | |
| lastFiredAt | INTEGER | nullable |

No secondary indexes.

### 3.2 `watchers`
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `wch_…` |
| kind | TEXT NOT NULL | `terminal`\|`pr_state` |
| title | TEXT NOT NULL | |
| goal | TEXT NOT NULL | |
| targetsJson | TEXT NOT NULL | JSON `string[]` (terminalIds, or single `"PR #N"` label) |
| cadenceMs | INTEGER NOT NULL | supervisors floored to `SCHEDULER_TICK_MS` on insert |
| isSupervisor | INTEGER NOT NULL DEFAULT 0 | bool 0/1 |
| modelTier | TEXT NOT NULL | `small`\|`medium`\|`large` |
| startAfterMs | INTEGER | nullable |
| stopAfterMs | INTEGER | nullable |
| stopWhenJson | TEXT | nullable (serialized WatchCondition) |
| alertWhenJson | TEXT | nullable (serialized WatchCondition) |
| optionsJson | TEXT | nullable |
| status | TEXT NOT NULL DEFAULT 'created' | `created`\|`active`\|`paused`\|`condition_met`\|`timeout`\|`cancelled`\|`error` |
| lastClassification | TEXT | nullable (a WatcherClassification string) |
| lastEpistemicKind | TEXT | nullable (`observed`\|`inferred`\|`unverified`) |
| lastCheckedAt | INTEGER | nullable |
| nextCheckAt | INTEGER NOT NULL | |
| createdAt | INTEGER NOT NULL | |

No secondary indexes. **Insert default status is `'active'`** in `insertWatcher`
(rec.status ?? "active"), even though the column DEFAULT is `'created'` — the code
path always supplies `active`. (Note this discrepancy when porting.)

### 3.3 `events` (attention-queue inbox)
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `evt_…` |
| source | TEXT NOT NULL | EventSource (8 values, §4) |
| severity | TEXT NOT NULL | Severity (7 values, §4) |
| title | TEXT NOT NULL | |
| summary | TEXT NOT NULL | |
| targetJson | TEXT | nullable (serialized EventTarget) |
| evidenceJson | TEXT | nullable (JSON `string[]`) |
| recommendedActionsJson | TEXT | nullable (JSON RecommendedAction[]) |
| dedupeKey | TEXT | nullable |
| epistemicKind | TEXT | nullable |
| createdAt | INTEGER NOT NULL | pinned; never bumped on dedupe |
| updatedAt | INTEGER | bumped on each dedupe; recency key |
| notifiedAt | INTEGER | nullable; set by markNotified |
| expiresAt | INTEGER | nullable TTL |
| resolvedAt | INTEGER | nullable |
| count | INTEGER NOT NULL DEFAULT 1 | dedupe collapse count |

Indexes:
- `idx_events_open` on `(resolvedAt, severity, createdAt)`
- `idx_events_dedupe` on `(dedupeKey, resolvedAt)`

### 3.4 `audit_log` (tool-dispatch forensic record)
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `aud_…` |
| ts | INTEGER NOT NULL | |
| actor | TEXT NOT NULL | `main`\|`watcher`\|`timer`\|`workflow`\|`system` |
| toolName | TEXT NOT NULL | |
| argsJson | TEXT NOT NULL | |
| outcome | TEXT NOT NULL | `ok`\|`error`\|`denied`\|`dedup`\|`grant_ok` |
| durationMs | INTEGER NOT NULL | |
| summary | TEXT NOT NULL | |
| resultJson | TEXT | nullable |
| grantSource | TEXT | nullable (`local`\|`daintree`) |
| grantId | TEXT | nullable |
| runId | TEXT | nullable (groups dispatches into a run) |

Index: `idx_audit_ts` on `(ts)`.

### 3.5 `run_events` (append-only per-run event log)
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `rne_…` |
| runId | TEXT NOT NULL | |
| seq | INTEGER NOT NULL | monotonic within run, starts at 0 |
| ts | INTEGER NOT NULL | |
| type | TEXT NOT NULL | e.g. `assistant:start`, `tool:call`, `tool:result` |
| payload | TEXT | nullable JSON |

Indexes:
- `idx_run_events_run` UNIQUE on `(runId, seq)` — DB backstop against duplicated seq.
- `idx_run_events_ts` on `(ts)` — for the retention sweep's per-run MAX(ts) scan.

### 3.6 `conversation`
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `msg_…` |
| sessionId | TEXT NOT NULL | session-fresh; old sessions never reload |
| seq | INTEGER NOT NULL | |
| role | TEXT NOT NULL | `system`\|`user`\|`assistant`\|`tool` |
| content | TEXT NOT NULL | |
| toolCallsJson | TEXT | nullable |
| toolCallId | TEXT | nullable |
| createdAt | INTEGER NOT NULL | |

Indexes: `idx_conv_session` on `(sessionId, seq)`; `idx_conv_createdat` on `(createdAt)` (for age sweep).

### 3.7 `skill_selection_log`
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `rsl_…` |
| ts | INTEGER NOT NULL | |
| sessionId | TEXT NOT NULL | |
| userInput | TEXT NOT NULL | |
| selectedSkillIdsJson | TEXT NOT NULL | JSON `string[]` |
| confidence | REAL NOT NULL | |
| taskType | TEXT | nullable |
| reason | TEXT | nullable |

Index: `idx_skill_sel_ts` on `(ts)`.

### 3.8 `automation_grants`
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `grt_…` |
| actorId | TEXT NOT NULL | `wch_…` or `tmr_…` |
| actorType | TEXT NOT NULL | `watcher`\|`timer` |
| allowedRiskClassesJson | TEXT | nullable JSON array of RiskClass |
| allowedToolNamesJson | TEXT | nullable JSON array of tool names |
| expiresAt | INTEGER NOT NULL | wall-clock ms |
| maxUses | INTEGER NOT NULL | |
| usesRemaining | INTEGER NOT NULL | |
| revokedAt | INTEGER | nullable; explicit revoke only (use-exhaustion does NOT set it) |
| createdAt | INTEGER NOT NULL | |
| source | TEXT NOT NULL DEFAULT 'local' | `local`\|`daintree` (daintree reserved/unused) |

Index: `idx_grants_actor` on `(actorId, revokedAt, expiresAt)`.
Invariant (TS-layer, not schema): at least one of the two allowlists must be
non-empty. Authorization is **union**: toolName in names-list OR riskClass in
classes-list.

### 3.9 `workflow_runs`
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `wfr_…` |
| issueNumber | INTEGER | nullable |
| issueUrl | TEXT | nullable |
| issueTitle | TEXT | nullable |
| branch | TEXT | nullable |
| worktreeId | TEXT | nullable |
| prNumber | INTEGER | nullable |
| prUrl | TEXT | nullable |
| terminalIdsJson | TEXT | nullable JSON `string[]` |
| watcherIdsJson | TEXT | nullable JSON `string[]` |
| queueEventIdsJson | TEXT | nullable JSON `string[]` |
| status | TEXT NOT NULL DEFAULT 'pending' | `pending`\|`active`\|`blocked`\|`done`\|`cancelled`\|`failed` |
| nextActionJson | TEXT | nullable (single serialized RecommendedAction) |
| notesJson | TEXT | nullable JSON `string[]` |
| createdAt | INTEGER NOT NULL | |
| updatedAt | INTEGER NOT NULL | store-forced on update |
| completedAt | INTEGER | nullable; caller-settable on terminal transition |

Index: `idx_workflow_runs_status` on `(status, updatedAt)`.

### 3.10 `agent_launches` (idempotent spawn saga)
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `agt_…` |
| idempotencyKey | TEXT NOT NULL | deterministic hash of {taskPrompt, worktreeId, agentId, mode} |
| agentId | TEXT NOT NULL | |
| worktreeId | TEXT | nullable |
| mode | TEXT NOT NULL | `edit`\|`explore` |
| title | TEXT NOT NULL | |
| name | TEXT NOT NULL | deterministic launch name (for terminal.list reconcile) |
| terminalId | TEXT | nullable |
| watcherId | TEXT | nullable |
| stage | TEXT NOT NULL DEFAULT 'launch_requested' | 7 stages, §4 |
| errorCode | TEXT | nullable |
| errorMessage | TEXT | nullable |
| createdAt | INTEGER NOT NULL | |
| updatedAt | INTEGER NOT NULL | store-forced on update |

Index: `idx_agent_launches_key` on `(idempotencyKey, stage, updatedAt)`.

### 3.11 `skill_run_state`
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `rrs_…` |
| sessionId | TEXT NOT NULL | |
| skillId | TEXT NOT NULL | |
| currentStep | INTEGER NOT NULL DEFAULT 0 | denormalized "where we left off"; 0 = not started |
| stepsJson | TEXT NOT NULL DEFAULT '[]' | JSON SkillStepProgress[] |
| status | TEXT NOT NULL DEFAULT 'active' | `active`\|`completed`\|`abandoned` |
| startedAt | INTEGER NOT NULL | |
| updatedAt | INTEGER NOT NULL | store-forced on update |
| completedAt | INTEGER | nullable; caller-settable on terminal transition |

Index: `idx_skill_run_state_key` UNIQUE on `(sessionId, skillId)` — natural key, one run per pair.

### 3.12 `memories` + `memories_fts` (FTS5)
| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | `mem_…` |
| content | TEXT NOT NULL | |
| category | TEXT | nullable free tag |
| source | TEXT NOT NULL DEFAULT 'assistant' | `user`\|`assistant`\|`compact` |
| pinnedAt | INTEGER | nullable; non-null ⇒ pinned |
| deletedAt | INTEGER | nullable; non-null ⇒ soft-deleted |
| createdAt | INTEGER NOT NULL | |
| updatedAt | INTEGER NOT NULL | |

Indexes:
- `idx_memories_category` on `(category, deletedAt)`
- `idx_memories_pinned` on `(pinnedAt) WHERE pinnedAt IS NOT NULL AND deletedAt IS NULL` (partial)

FTS5 virtual table (external-content over `memories.content`):
```sql
CREATE VIRTUAL TABLE memories_fts USING fts5(
  content, content='memories', content_rowid='rowid');
```
Triggers keep the index in lockstep (KEEP behavior):
- `memories_ai` AFTER INSERT → `INSERT INTO memories_fts(rowid, content) VALUES(new.rowid,new.content)`
- `memories_ad` AFTER DELETE → `INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content)`
- `memories_au` AFTER UPDATE → delete-then-insert (the 'delete' command, then the new row)

Soft-deleted rows STAY indexed; recall filters them via the JOIN's `m.deletedAt IS NULL`.
**Caution (KEEP):** `gcRetentionSweep` hard-DELETEs soft-deleted memories from the
base table only; `memories_ad` auto-evicts from FTS. Because `forgetMemory` never
mutates `content`, `old.content` always matches the indexed value — do NOT also
issue a manual FTS 'delete' on sweep (double-evict corrupts the index).

---

## 4. Enums & value sets (exact — from `schemas.ts`)

| Enum | Values |
|---|---|
| RiskClass | `read`, `local`, `ui`, `terminal`, `project`, `git`, `external`, `system` |
| Tier | `supervisor`, `operator`, `system` |
| ModelTier | `small`, `medium`, `large` |
| AgentState | `idle`, `working`, `waiting`, `directing`, `completed`, `exited` |
| Severity | `debug`, `info`, `attention`, `urgent`, `blocked`, `done`, `error` |
| EventSource | `timer`, `terminal_watcher`, `worktree_watcher`, `pr_watcher`, `workflow`, `model_worker`, `system`, `user` |
| EpistemicKind | `observed`, `inferred`, `unverified` |
| WatcherClassification | `no_change`, `still_working`, `waiting_for_input`, `permission_prompt`, `command_failed`, `tests_failed`, `tests_passed`, `merge_conflict`, `completed_success`, `completed_unverified`, `completed_unknown`, `terminal_exited`, `rate_limited`, `needs_large_model`, `unknown` |
| TimerRecord.payloadType | `enqueue`, `run_check`, `call_safe_tool` |
| TimerRecord.status | `scheduled`, `fired`, `cancelled`, `done` |
| WatcherRecord.kind | `terminal`, `pr_state` |
| WatcherRecord.status | `created`, `active`, `paused`, `condition_met`, `timeout`, `cancelled`, `error` |
| AuditRecord.actor | `main`, `watcher`, `timer`, `workflow`, `system` |
| AuditRecord.outcome | `ok`, `error`, `denied`, `dedup`, `grant_ok` |
| AutomationGrantActorType | `watcher`, `timer` |
| AutomationGrantSource | `local`, `daintree` |
| ConversationMessageRecord.role | `system`, `user`, `assistant`, `tool` |
| WorkflowRunStatus | `pending`, `active`, `blocked`, `done`, `cancelled`, `failed` |
| MemorySource | `user`, `assistant`, `compact` |
| SkillRunStatus | `active`, `completed`, `abandoned` |
| SkillStepStatus | `done`, `skipped` |
| AgentLaunchStage | `launch_requested`, `agent_started`, `terminal_bound`, `watcher_attached`, `confirmed`, `failed`, `ambiguous` (only `confirmed`/`failed` are terminal) |

Severity ordering (KEEP — used for filtering/ordering events):
`debug=0, info=1, done=2, attention=3, blocked=4, urgent=5, error=6` (default `1`).
Both a TS map (`SEVERITY_ORDER`) and a SQL `CASE` (`SEV_CASE`) encode it; they must
stay in sync. Note `done` (2) ranks BELOW `attention`/`blocked`/`urgent`/`error`.

Composite WatchCondition DSL (serialized into stopWhenJson/alertWhenJson):
`{stateIs}`, `{runtimeStatusIs:'running'|'exited'}`, `{contains}` (non-empty),
`{regex}` (valid, non-empty), `{noOutputForMs}` (positive int), `{modelJudge}`
(non-empty), `{all:[]}`/`{any:[]}` (≥1), `{not}`. Validation is in the Zod schema —
reproduce these guards in Go validation (empty/degenerate conditions rejected at
creation, NOT at evaluation).

---

## 5. Magic constants, limits, cadences (exact numbers)

| Constant | Value | Source | Meaning |
|---|---|---|---|
| `busy_timeout` | 5000 ms | db.ts | lock retry budget |
| `DAY_MS` | 86_400_000 | db.ts | |
| `SCHEDULER_TICK_MS` | 3_000 ms | watcherCadence.ts | supervisor cadence floor |
| `SUPERVISOR_DEFAULT_CADENCE_MS` | 3_000 ms | watcherCadence.ts | |
| `MONITOR_DEFAULT_CADENCE_MS` | 120_000 ms | watcherCadence.ts | |
| `PR_WATCHER_CADENCE_MS` | 60_000 ms | watcherCadence.ts | |
| `WATCHER_SPAWN_GRACE_MS` | 20_000 ms | watcherCadence.ts | |
| uuid slice | `randomUUID().slice(0,8)` | db.ts | 8-hex-char id suffix |
| pruneOldRuns chunk | 900 | db.ts | per-batch id count (under SQLITE_MAX_VARIABLE_NUMBER 999) |

**RetentionPolicy (`DEFAULT_RETENTION`)** — each plain log table keeps the **newer
of** maxAge OR last keepN rows:

| Field | Default | Table effect |
|---|---|---|
| auditLogMaxAgeMs | 30 × DAY_MS | audit_log age cutoff |
| auditLogKeepRows | 5000 | audit_log row floor (sized > 500 runs × per-run dispatches) |
| runEventsMaxAgeMs | 14 × DAY_MS | run_events age cutoff (by run) |
| runEventsKeepRuns | 500 | run_events run floor |
| conversationMaxAgeMs | 90 × DAY_MS | conversation age cutoff |
| conversationKeepRows | 1000 | conversation row floor |
| skillSelLogMaxAgeMs | 30 × DAY_MS | skill_selection_log age cutoff |
| skillSelLogKeepRows | 500 | skill_selection_log row floor |
| eventsTerminalAgeMs | 7 × DAY_MS | hard-delete resolved/expired events past window |
| memoriesDeletedAgeMs | 30 × DAY_MS | hard-delete soft-deleted memories past undo window |

**Query default limits:** `listAudit` 50; `queryAudit` 200; `listSkillSelections`
50; `listRuns` 20; `listMemories` default 50, clamped `[1,200]`; `recallMemories`
default 10, clamped `[1,50]`. `AuditFilters.limit` default 200.

---

## 6. Store method inventory (signatures + 1-2 line behavior)

`DbOptions { now?: ()=>number; retention?: Partial<RetentionPolicy> }` — both for
tests (pin sweep clock, shrink windows).

### Lifecycle / internal
| Method | Behavior |
|---|---|
| `constructor(dbPath, opts?)` | Open, set PRAGMAs, exec SCHEMA, migrate, cancelStaleWatchers, cancelStaleAgentLaunches, try/catch gcRetentionSweep. |
| `close()` | Close handle. |
| `raw()` | Escape hatch returning the raw driver handle. |
| `private migrate()` | user_version-keyed; single no-op baseline → user_version=1. |
| `private cancelStaleWatchers(now?)` | **KEEP exact order:** (1) revoke `automation_grants` where `actorType='watcher'` & not revoked & actorId in watchers with status in (active,created,paused); (2) set those watchers' status='cancelled'; (3) resolve all open `events` where source in (terminal_watcher,worktree_watcher,pr_watcher). Order matters: revoke before flip so no grant is live for a cancelled watcher. |
| `private cancelStaleAgentLaunches(now?)` | Set stage='failed', errorCode=COALESCE(existing,'SESSION_ENDED'), errorMessage=COALESCE(existing,'session ended before confirmation'), updatedAt=now WHERE stage NOT IN (confirmed,failed). |
| `gcRetentionSweep(now, retention)` `@internal` | Order: pruneOldRuns FIRST (co-deletes run's audit rows), then deleteByAgeAndCount on audit_log/conversation/skill_selection_log, then DELETE terminal events past window, then DELETE soft-deleted memories past window. No VACUUM (freelist reuse). |
| `private deleteByAgeAndCount(table,timeCol,cutoff,keepN)` | `DELETE … WHERE timeCol < cutoff AND rowid NOT IN (SELECT rowid … ORDER BY timeCol DESC LIMIT keepN)`. table/timeCol are fixed internal identifiers (interpolation safe). |
| `private pruneOldRuns(cutoff, keepRuns)` | Select runIds with MAX(ts) < cutoff, `LIMIT -1 OFFSET keepRuns` (skip kept tail). In ONE transaction, chunked by 900, DELETE run_events then audit_log by runId. Rollback on error. |
| `private applyUpdate(table, allowedSet, id, patch)` | Dynamic UPDATE; only allowlisted columns; `toSqlValue` coercion; no-op if no allowed keys. |

### timers
| Method | Behavior |
|---|---|
| `insertTimer(rec)` → TimerRecord | Defaults id `tmr_`, runCount 0, status 'scheduled', createdAt now. |
| `getTimer(id)` | by id. |
| `listTimers(status?)` | all or by status, ORDER BY fireAt. |
| `dueTimers(now)` | status='scheduled' AND fireAt <= now, ORDER BY fireAt. |
| `updateTimer(id, patch)` | applyUpdate w/ TIMER_UPDATE_COLS. |

`TIMER_UPDATE_COLS`: title, fireAt, repeatEveryMs, repeatUntil, maxRuns, runCount,
payloadType, payloadJson, targetJson, status, lastFiredAt. (`id`/`createdAt` immutable.)

### watchers
| Method | Behavior |
|---|---|
| `insertWatcher(rec)` → WatcherRecord | Default id `wch_`, status 'active', createdAt now; **supervisor cadence floored to `max(cadenceMs, SCHEDULER_TICK_MS)`**; isSupervisor stored 0/1. |
| `getWatcher(id)` | rowToWatcher (coerces isSupervisor→bool). |
| `listWatchers(status?)` | all or by status, ORDER BY createdAt. |
| `dueWatchers(now)` | status='active' AND nextCheckAt <= now, ORDER BY nextCheckAt. |
| `updateWatcher(id, patch)` | applyUpdate w/ WATCHER_UPDATE_COLS. |

`WATCHER_UPDATE_COLS`: title, goal, targetsJson, cadenceMs, isSupervisor, modelTier,
startAfterMs, stopAfterMs, stopWhenJson, alertWhenJson, optionsJson, status,
lastClassification, lastEpistemicKind, lastCheckedAt, nextCheckAt.

### events (KEEP dedupe semantics carefully)
| Method | Behavior |
|---|---|
| `upsertEvent(ev)` → QueueEvent | If `ev.dedupeKey`: find open (resolvedAt NULL & (expiresAt NULL OR > now)) row with same dedupeKey, newest first. If found: `count = count+1`, refresh title/summary/severity/epistemicKind, **overwrite recommendedActions outright (clear to null if none)**, evidence/epistemicKind **fall back to existing when omitted**, bump updatedAt, refresh expiresAt — **do NOT touch createdAt** (scheduler's "is new?" keys on createdAt). Else INSERT new (id `evt_`, count default 1, updatedAt=createdAt). |
| `getEvent(id)` | rowToEvent. |
| `listEvents(opts?)` | WHERE not-expired; unless includeResolved add resolvedAt IS NULL; if notifiedIsNull add notifiedAt IS NULL; if severityAtLeast add SEV_CASE >= n; ORDER BY SEV_CASE DESC, COALESCE(updatedAt,createdAt) DESC; optional LIMIT maxItems. |
| `markNotified(ids, ts?)` | Set notifiedAt per id (loop). |
| `resolveEvent(id)` → bool | Set resolvedAt WHERE resolvedAt IS NULL; returns changes>0. |

`QueueDigestOptions`: severityAtLeast?, maxItems?, includeResolved?, notifiedIsNull?.

### audit
| Method | Behavior |
|---|---|
| `insertAudit(rec)` → AuditRecord | id `aud_`, ts now default. |
| `listAudit(limit=50)` | ORDER BY ts DESC LIMIT. |
| `queryAudit(filters?)` | Optional AND-combined: actor/toolName/outcome/tsFrom/tsTo via `(? IS NULL OR col=?)` pattern (each filter bound twice); limit default 200; ts DESC. **node:sqlite throws on bound `undefined` → coerce `?? null`.** |
| `listAuditByRunId(runId)` | WHERE runId ORDER BY ts ASC (oldest first). |

`AuditFilters`: actor?, toolName?, outcome?, tsFrom?, tsTo?, limit? (ms-int inclusive bounds).

### run events
| Method | Behavior |
|---|---|
| `insertRunEvent(rec)` → RunEventRecord | id `rne_`, ts now default. |
| `listRunEvents(runId)` | ORDER BY seq ASC (replay order). |
| `listRuns(limit=20)` | Aggregate over run_events GROUP BY runId: runId, MIN(ts) firstTs, MAX(ts) lastTs, COUNT(*) eventCount; ORDER BY lastTs DESC. (No run table.) |

### automation grants (KEEP consume atomicity)
| Method | Behavior |
|---|---|
| `insertGrant(rec)` → AutomationGrantRecord | id `grt_`, usesRemaining default = maxUses, revokedAt null, source 'local', createdAt now. |
| `getGrant(id)` | by id. |
| `listGrants(actorId?, now?)` | Live grants: revokedAt NULL & expiresAt > now & usesRemaining > 0, optional actorId, ORDER BY createdAt. |
| `consumeGrant(actorId, actorType, toolName, riskClass, now?)` → grant\|undef | Iterate live grants for actor; require matching actorType; `grantAuthorizes` (union of tool-name/risk-class lists); atomic `UPDATE … usesRemaining=usesRemaining-1 WHERE id=? AND usesRemaining>0 AND revokedAt IS NULL AND expiresAt>?`; if changes>0 return updated grant. |
| `revokeGrant(id, now?)` → bool | Set revokedAt WHERE revokedAt IS NULL. |
| `revokeGrantsByActor(actorId, now?)` → count | Revoke all live for actor. |

`grantAuthorizes(g, toolName, riskClass)`: parse the two JSON arrays (tolerant of
null/garbage → `[]`); true if toolName in names list OR riskClass in classes list.

### conversation
| `insertMessage(rec)` → record | id `msg_`, createdAt now. |
| `listMessages(sessionId)` | WHERE sessionId ORDER BY seq. |

### skill selection log
| `insertSkillSelection(rec)` → record | id `rsl_`, ts now. |
| `listSkillSelections(limit=50)` | ORDER BY ts DESC LIMIT. |

### workflow runs
| `insertWorkflowRun(rec)` → record | id `wfr_`, status 'pending', createdAt/updatedAt now. |
| `getWorkflowRun(id)` | rowToWorkflowRun (NULL→undefined; *Json stay raw). |
| `listWorkflowRuns(status?)` | all or by status, ORDER BY updatedAt DESC. |
| `updateWorkflowRun(id, patch)` | **No-op guard:** only proceed if patch changes an allowed col other than updatedAt; then force `updatedAt=Date.now()` (never from patch); completedAt IS caller-settable. |

`WORKFLOW_UPDATE_COLS`: issueNumber, issueUrl, issueTitle, branch, worktreeId,
prNumber, prUrl, terminalIdsJson, watcherIdsJson, queueEventIdsJson, status,
nextActionJson, notesJson, updatedAt, completedAt.

### skill run state
| `insertSkillRunState(rec)` → record | id `rrs_`, currentStep 0, stepsJson '[]', status 'active', startedAt/updatedAt now. |
| `getSkillRunState(sessionId, skillId)` | natural key lookup. |
| `listSkillRunStates(sessionId?)` | all or by session, ORDER BY updatedAt DESC. |
| `updateSkillRunState(id, patch)` | force updatedAt=now; applyUpdate w/ SKILL_RUN_UPDATE_COLS. |

`SKILL_RUN_UPDATE_COLS`: currentStep, stepsJson, status, updatedAt, completedAt.

### agent launches
| `insertAgentLaunch(rec)` → record | id `agt_`, stage 'launch_requested', createdAt/updatedAt now. |
| `getAgentLaunch(id)` | rowToAgentLaunch. |
| `findActiveAgentLaunch(idempotencyKey)` | WHERE idempotencyKey & stage NOT IN (confirmed,failed) ORDER BY updatedAt DESC LIMIT 1. |
| `updateAgentLaunch(id, patch)` | force updatedAt=now; applyUpdate w/ AGENT_LAUNCH_UPDATE_COLS. |

`AGENT_LAUNCH_UPDATE_COLS`: agentId, worktreeId, mode, title, name, terminalId,
watcherId, stage, errorCode, errorMessage, updatedAt.

### memories (KEEP FTS escaping)
| `insertMemory(rec)` → record | id `mem_`, source 'assistant', createdAt/updatedAt now; trigger indexes content. |
| `getMemory(id, {includeDeleted?})` | Hidden if deletedAt != null (explicit null check — epoch 0 still deleted) unless includeDeleted. |
| `listMemories({category?,pinnedOnly?,includeDeleted?,limit?})` | WHERE deletedAt IS NULL (unless includeDeleted) [+category] [+pinnedAt NOT NULL]; limit clamp [1,200] default 50; ORDER BY (pinnedAt IS NOT NULL) DESC, COALESCE(pinnedAt,updatedAt) DESC. |
| `recallMemories(query, {category?,limit?})` | Trim; empty→`[]`. **Tokenize on whitespace, quote EACH token doubling internal `"`, space-join → FTS5 implicit-AND keyword search.** JOIN memories_fts↔memories WHERE m.deletedAt IS NULL AND memories_fts MATCH ? [+category]; ORDER BY bm25(memories_fts); limit clamp [1,50] default 10. |
| `forgetMemory(id, now?)` → bool | Soft-delete: set deletedAt+updatedAt WHERE deletedAt IS NULL. Does NOT mutate content (so FTS old.content matches on later hard-delete). |
| `pinMemory(id, now?)` → record\|undef | Set pinnedAt+updatedAt WHERE deletedAt IS NULL AND pinnedAt IS NULL (idempotent; guard avoids re-jumping order). |
| `unpinMemory(id, now?)` → record\|undef | Set pinnedAt=NULL+updatedAt WHERE deletedAt IS NULL AND pinnedAt IS NOT NULL (idempotent). |

`toSqlValue(v)`: undefined/null→null; bool→1/0; string/number/bigint/Uint8Array
passthrough; else String(v).

---

## 7. Session-boundary & retention behavior (the load-bearing edge cases)

1. **Stale watcher cancel (KEEP).** On open: revoke watcher grants → cancel
   non-terminal watchers (`active`/`created`/`paused`) → resolve all open
   watcher-sourced events. Rationale: watchers are session-scoped; an inherited
   active watcher points at gone terminals and would fire false `terminal_exited`.
   Terminal statuses (`condition_met`/`timeout`/`cancelled`/`error`) are left for
   the UI history. Events table has NO sessionId and watcher publishes carry no TTL,
   so orphaned watcher events must be resolved explicitly — scoped to the 3 watcher
   sources only so timer/system/user events persist.
2. **Stale agent-launch reconcile (KEEP).** On open: non-terminal sagas → `failed`
   so a fresh launch's idempotencyKey isn't blocked by a dead in-flight record.
3. **Retention sweep ordering (KEEP).** run_events pruned by **whole run** BEFORE
   audit's own sweep (co-deletes the run's audit rows), so audit's count floor is
   spent on retained rows not dead-run rows — preserving the `/explain` pairing of
   run_events↔audit_log by runId. audit window (30d) intentionally wider than
   run_events (14d).
4. **Best-effort housekeeping.** gcRetentionSweep is wrapped in try/catch in the
   constructor — a sweep failure must never abort DB construction.
5. **No VACUUM** — freed pages return to the freelist; append-heavy file never shrinks.
6. **Insert-vs-DEFAULT discrepancy:** `insertWatcher` supplies status `'active'`
   though column DEFAULT is `'created'`. Preserve the active-on-insert behavior.

---

## 8. ID prefixes, timestamps, JSON formats (external contract — KEEP)

ID format: `<prefix>_<8 lowercase hex>` (first 8 chars of a UUIDv4).

| Prefix | Table |
|---|---|
| `tmr_` | timers |
| `wch_` | watchers |
| `evt_` | events |
| `aud_` | audit_log |
| `rne_` | run_events |
| `msg_` | conversation |
| `rsl_` | skill_selection_log |
| `grt_` | automation_grants |
| `wfr_` | workflow_runs |
| `agt_` | agent_launches |
| `rrs_` | skill_run_state |
| `mem_` | memories |

- **All timestamps:** Unix epoch **milliseconds**, INTEGER. (Go: `time.Now().UnixMilli()`.)
- **Booleans:** stored 0/1 INTEGER (`isSupervisor`).
- **JSON columns:** `targetsJson`/`*IdsJson`/`notesJson`/`evidenceJson`/
  `allowed*Json`/`selectedSkillIdsJson` are JSON `string[]`; `stepsJson` is
  `SkillStepProgress[]`; `targetJson` is EventTarget; `recommendedActionsJson` is
  `RecommendedAction[]`; `nextActionJson` is a single RecommendedAction;
  `payloadJson`/`optionsJson`/`stopWhenJson`/`alertWhenJson` are free serialized JSON.
- **FTS MATCH escaping** (security boundary, KEEP): per-token quote with internal
  `"`→`""`; never pass raw user input to MATCH (raises SQLite syntax error otherwise).

---

## 9. State path & env var precedence (external contract — KEEP)

DB file: `path.join(stateDir, "state.db")`. `stateDir` precedence (highest first),
from `config.ts`:
1. explicit override / `DAINTREE_ASSISTANT_STATE_DIR` (**trusted env only** — a bound
   project's `.env` must NOT redirect the DB; exfiltration class).
2. per-project: `~/.daintree/assistant-cli/<projectIdToDir(DAINTREE_PROJECT_ID)>`
   (so concurrent projects never share state.db).
3. legacy flat: `~/.daintree/assistant-cli/`.

`windowId` (`DAINTREE_WINDOW_ID`) is read but does NOT yet affect the path.
The DB file being per-project is WHY `memories` has no `projectId` column.
Related env: `DAINTREE_ASSISTANT_DEBUG_LOG` / `DAINTREE_ASSISTANT_LOG_DIR` (log dir
is GLOBAL, `~/.daintree/logs`, trusted-env override only — out of storage scope).

---

## 10. What to DELETE / not port

| Drop | Why |
|---|---|
| `sqliteDriver.ts` entirely (Bun/Node dual driver, createRequire, null↔undefined coercion) | Go uses one SQLite lib directly. |
| `node:sqlite`/`bun:sqlite` import dance & esbuild `external` notes | N/A in Go. |
| `randomUUID().slice(0,8)` via `node:crypto` | Use a Go UUID lib; keep the 8-hex-suffix format. |
| `toSqlValue` boolean→0/1 dance | Go drivers handle bools, or map explicitly for `isSupervisor`. |
| `?? null` undefined-coercion for binds (node:sqlite quirk) | Go uses typed nullables. |
| Zod runtime schemas | Replace with Go struct validation; keep the value sets & DSL guards. |
| OpenTUI/React/Ink/Bun cockpit references | UI is Bubble Tea; storage is UI-agnostic. |
| `migrate()` user_version chain scaffolding | Single schema exec; no chain (dev hard-resets). |
| `raw()` escape hatch | Optional; expose the `*sql.DB` if needed for tests only. |

**KEEP exactly:** table/column/index/trigger names; ID prefixes; ms timestamps;
JSON column shapes; severity ordering; FTS escaping; dedupe/upsert semantics;
session-boundary sweep order; retention ordering & numbers; grant union-authorize &
atomic consume; the env-var precedence + trusted-env rule for stateDir.

---

## 11. Concrete Go mapping proposal (no code yet)

### Packages
- `internal/storage` — the `Store` (port of `Db`), schema embed, sweeps.
- `internal/domain` (or reuse a shared `schemas` pkg) — record structs + enum
  consts + validation (RiskClass, Severity, WatcherClassification, etc.). These are
  shared across daemon/tools/models, mirroring `src/schemas.ts`.

### SQLite library options
- **`modernc.org/sqlite`** (pure Go, CGO-free) — strongly preferred to keep the
  CLI cross-compilable and dependency-light; supports FTS5 in recent builds.
  Verify FTS5 + external-content + triggers + `bm25()` are enabled in the target
  version before committing.
- Fallback: `github.com/mattn/go-sqlite3` (CGO, needs `fts5` build tag). Use only
  if modernc's FTS5 proves insufficient.
- Use `database/sql` with a single connection / `SetMaxOpenConns(1)` to preserve
  the single-writer, no-interleave assumption the "atomic" consume/resolve rely on
  (or wrap consume/resolve/upsert in explicit transactions for safety — Go's pool
  can otherwise interleave goroutines, unlike the synchronous TS driver).

### Key types / interfaces (proposed shapes, described not coded)
- `type Store struct` holding `*sql.DB`, a `now func() time.Time` (test seam, from
  `DbOptions.now`), and a `Retention` value.
- `type Retention struct` with the 10 fields from §5 (durations as `time.Duration`,
  counts as `int`); `DefaultRetention` package var.
- One struct per record (TimerRecord, WatcherRecord, EventRecord/QueueEvent,
  AuditRecord, RunEventRecord, RunSummary, AutomationGrant, ConversationMessage,
  SkillSelectionLog, WorkflowRun, AgentLaunch, SkillRunState, Memory) with
  `sql.NullInt64`/`*int64`/`*string` for nullable columns; JSON columns as `string`
  fields (raw) plus typed accessors where the store itself parses (events, grants).
- Enums as `type X string` + `const` blocks; a `severityRank(Severity) int` helper
  and a matching SQL `CASE` builder (single source — generate the CASE from the
  map to avoid drift).
- Method set mirrors §6 one-to-one (Go naming: `InsertTimer`, `DueWatchers(now)`,
  `ConsumeGrant(...)`, `RecallMemories(...)`, etc.), returning `(T, error)` /
  `(*T, error)` / `(bool, error)`.
- Column allowlist + dynamic UPDATE: replace the TS `applyUpdate` + `Set<string>`
  with explicit typed `UpdateX(id, patch)` methods OR a small allowlisted
  field-map builder; keep the "only touch row if a non-updatedAt col changed"
  guard for workflow_runs and the forced-updatedAt rule for
  workflow/skill_run/agent_launch updates.

### Schema delivery
- Embed `schema.sql` via `//go:embed`; `Exec` it on open (all `CREATE … IF NOT
  EXISTS`), set a version pragma, then run the three boundary routines:
  `cancelStaleWatchers`, `cancelStaleAgentLaunches`, `gcRetentionSweep` (the last
  wrapped so its error is logged, never fatal).

### Test seams
- `now func() time.Time` and an in-memory DB (`:memory:`) for unit tests, matching
  the TS `DbOptions { now, retention }` pattern and `Pass ":memory:"` convention.

---

## 12. Open questions / verify-before-build

- Confirm `modernc.org/sqlite` build has FTS5 (external-content + `bm25()` +
  trigger-driven sync) working; if not, decide CGO trade-off early.
- Decide whether to keep the implicit single-writer no-transaction "atomicity" or
  wrap consume/resolve/upsert/pin in explicit transactions (recommended in Go since
  the `database/sql` pool can interleave goroutines — the TS synchronous guarantee
  does not carry over).
- `insertWatcher` status default `'active'` vs column DEFAULT `'created'`: preserve
  the active-on-insert code behavior; align the column default to avoid confusion in
  a clean schema.
