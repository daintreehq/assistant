-- Daintree assistant durable store. Clean Go-native schema (no migration chain;
-- dev policy hard-resets the file). All timestamps are Unix epoch milliseconds
-- (INTEGER). Booleans are 0/1. IDs are "<prefix><8 hex of uuid v4>" TEXT.
-- Every CREATE is IF NOT EXISTS so the single exec is idempotent on open.

-- 3.1 timers — persist across sessions.
CREATE TABLE IF NOT EXISTS timers (
  id            TEXT PRIMARY KEY,
  title         TEXT NOT NULL,
  fireAt        INTEGER NOT NULL,
  repeatEveryMs INTEGER,
  repeatUntil   INTEGER,
  maxRuns       INTEGER,
  runCount      INTEGER NOT NULL DEFAULT 0,
  payloadType   TEXT NOT NULL,
  payloadJson   TEXT NOT NULL,
  targetJson    TEXT,
  status        TEXT NOT NULL DEFAULT 'scheduled',
  createdAt     INTEGER NOT NULL,
  lastFiredAt   INTEGER
);
-- The scheduler runs this predicate every tick. Status first excludes the much
-- larger terminal-history set, while fireAt supplies both the range and ordering.
CREATE INDEX IF NOT EXISTS idx_timers_due ON timers(status, fireAt);

-- 3.2 watchers — project-scoped supervisors: rows survive process boundaries
-- and are adopted by the next owner (cockpit or supervisor daemon); /clear is
-- the only wholesale teardown. NB: insert path supplies 'active'; the column
-- DEFAULT is aligned to 'active' here (clean schema, no confusion).
CREATE TABLE IF NOT EXISTS watchers (
  id                 TEXT PRIMARY KEY,
  kind               TEXT NOT NULL,
  title              TEXT NOT NULL,
  goal               TEXT NOT NULL,
  targetsJson        TEXT NOT NULL,
  cadenceMs          INTEGER NOT NULL,
  isSupervisor       INTEGER NOT NULL DEFAULT 0,
  modelTier          TEXT NOT NULL,
  startAfterMs       INTEGER,
  stopAfterMs        INTEGER,
  stopWhenJson       TEXT,
  alertWhenJson      TEXT,
  optionsJson        TEXT,
  status             TEXT NOT NULL DEFAULT 'active',
  lastClassification TEXT,
  lastEpistemicKind  TEXT,
  lastCheckedAt      INTEGER,
  nextCheckAt        INTEGER NOT NULL,
  createdAt          INTEGER NOT NULL,
  -- WHY a watcher reached a terminal 'cancelled' status, so a /clear teardown
  -- ('session_cleared', stamped by CancelLiveWatchers) is distinguishable from a
  -- deliberate user cancel ('user_cancelled', stamped by watcher.cancel). NULL on
  -- active rows and on natural terminal states (condition_met/timeout/error).
  -- endedAt is the epoch-ms of that cancel.
  endedReason        TEXT,
  endedAt            INTEGER,
  -- Back-link to the durable workflow_runs ledger row a supervisor watcher drives.
  -- NULL for non-supervisor / manually-created watchers. When set, the daemon advances
  -- that row's status as the watcher reaches a terminal state.
  workflowRunId      TEXT
);
-- Scheduler due scans and status-filtered operational views must stay proportional
-- to LIVE rows, not to the lifetime history retained in this project database.
CREATE INDEX IF NOT EXISTS idx_watchers_due ON watchers(status, nextCheckAt);
CREATE INDEX IF NOT EXISTS idx_watchers_status_created ON watchers(status, createdAt);

-- 3.3 events — attention-queue inbox. createdAt is pinned; updatedAt is the
-- dedupe-bump recency key.
CREATE TABLE IF NOT EXISTS events (
  id                      TEXT PRIMARY KEY,
  source                  TEXT NOT NULL,
  severity                TEXT NOT NULL,
  title                   TEXT NOT NULL,
  summary                 TEXT NOT NULL,
  targetJson              TEXT,
  evidenceJson            TEXT,
  recommendedActionsJson  TEXT,
  dedupeKey               TEXT,
  epistemicKind           TEXT,
  createdAt               INTEGER NOT NULL,
  updatedAt               INTEGER,
  notifiedAt              INTEGER,
  expiresAt               INTEGER,
  resolvedAt              INTEGER,
  count                   INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_events_open   ON events(resolvedAt, severity, createdAt);
CREATE INDEX IF NOT EXISTS idx_events_dedupe ON events(dedupeKey, resolvedAt);

-- 3.4 audit_log — tool-dispatch forensic record.
CREATE TABLE IF NOT EXISTS audit_log (
  id          TEXT PRIMARY KEY,
  ts          INTEGER NOT NULL,
  actor       TEXT NOT NULL,
  toolName    TEXT NOT NULL,
  argsJson    TEXT NOT NULL,
  outcome     TEXT NOT NULL,
  durationMs  INTEGER NOT NULL,
  summary     TEXT NOT NULL,
  resultJson  TEXT,
  grantSource TEXT,
  grantId     TEXT,
  runId       TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts);

-- 3.5 run_events — append-only per-run replay log.
CREATE TABLE IF NOT EXISTS run_events (
  id      TEXT PRIMARY KEY,
  runId   TEXT NOT NULL,
  seq     INTEGER NOT NULL,
  ts      INTEGER NOT NULL,
  type    TEXT NOT NULL,
  payload TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_run_events_run ON run_events(runId, seq);
CREATE INDEX IF NOT EXISTS idx_run_events_ts ON run_events(ts);

-- 3.6 conversation — session-fresh transcript. reasoningContent persists an
-- assistant turn's DeepSeek chain-of-thought so it survives resume and replays
-- correctly (the API 400s on a tool-call turn missing it); NULL when thinking is off.
-- name is the message's wire `name`, and today exactly one kind of row carries one:
-- the server-delivered compacted context block, which wears the reserved
-- `daintree_compaction`. That name is the ONLY thing telling the backend where frozen
-- history ends, so a block that rehydrated without it would be re-compacted and the
-- request would carry history the server already replaced. NULL on every other row.
CREATE TABLE IF NOT EXISTS conversation (
  id               TEXT PRIMARY KEY,
  sessionId        TEXT NOT NULL,
  seq              INTEGER NOT NULL,
  role             TEXT NOT NULL,
  content          TEXT NOT NULL,
  name             TEXT,
  reasoningContent TEXT,
  toolCallsJson    TEXT,
  toolCallId       TEXT,
  createdAt        INTEGER NOT NULL
);
-- UNIQUE so a (sessionId, seq) collision is rejected at the storage layer, not
-- merely caught on read by the rehydrator (mirrors run_events' idx_run_events_run).
CREATE UNIQUE INDEX IF NOT EXISTS idx_conv_session   ON conversation(sessionId, seq);
CREATE INDEX IF NOT EXISTS idx_conv_createdat ON conversation(createdAt);

-- 3.8 automation_grants — non-interactive actor authorization (union allowlist).
CREATE TABLE IF NOT EXISTS automation_grants (
  id                     TEXT PRIMARY KEY,
  actorId                TEXT NOT NULL,
  actorType              TEXT NOT NULL,
  allowedRiskClassesJson TEXT,
  allowedToolNamesJson   TEXT,
  expiresAt              INTEGER NOT NULL,
  maxUses                INTEGER NOT NULL,
  usesRemaining          INTEGER NOT NULL,
  revokedAt              INTEGER,
  createdAt              INTEGER NOT NULL,
  source                 TEXT NOT NULL DEFAULT 'local'
);
CREATE INDEX IF NOT EXISTS idx_grants_actor ON automation_grants(actorId, revokedAt, expiresAt);

-- 3.9 workflow_runs — end-to-end workflow ledger.
CREATE TABLE IF NOT EXISTS workflow_runs (
  id                TEXT PRIMARY KEY,
  issueNumber       INTEGER,
  issueUrl          TEXT,
  issueTitle        TEXT,
  branch            TEXT,
  worktreeId        TEXT,
  prNumber          INTEGER,
  prUrl             TEXT,
  terminalIdsJson   TEXT,
  watcherIdsJson    TEXT,
  queueEventIdsJson TEXT,
  status            TEXT NOT NULL DEFAULT 'pending',
  nextActionJson    TEXT,
  notesJson         TEXT,
  createdAt         INTEGER NOT NULL,
  updatedAt         INTEGER NOT NULL,
  completedAt       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_status ON workflow_runs(status, updatedAt);

-- 3.10 agent_launches — idempotent spawn saga.
CREATE TABLE IF NOT EXISTS agent_launches (
  id             TEXT PRIMARY KEY,
  idempotencyKey TEXT NOT NULL,
  agentId        TEXT NOT NULL,
  worktreeId     TEXT,
  mode           TEXT NOT NULL,
  title          TEXT NOT NULL,
  name           TEXT NOT NULL,
  terminalId     TEXT,
  watcherId      TEXT,
  stage          TEXT NOT NULL DEFAULT 'launch_requested',
  errorCode      TEXT,
  errorMessage   TEXT,
  createdAt      INTEGER NOT NULL,
  updatedAt      INTEGER NOT NULL,
  -- Back-link to the durable workflow_runs ledger row this spawn created. Set once
  -- the ledger row exists so an idempotent retry re-uses it instead of duplicating.
  workflowRunId  TEXT
);
CREATE INDEX IF NOT EXISTS idx_agent_launches_key ON agent_launches(idempotencyKey, stage, updatedAt);

-- 3.11 runbook_run_state — stepwise runbook progress (natural key sessionId+runbookId).
CREATE TABLE IF NOT EXISTS runbook_run_state (
  id          TEXT PRIMARY KEY,
  sessionId   TEXT NOT NULL,
  runbookId     TEXT NOT NULL,
  currentStep INTEGER NOT NULL DEFAULT 0,
  stepsJson   TEXT NOT NULL DEFAULT '[]',
  status      TEXT NOT NULL DEFAULT 'active',
  startedAt   INTEGER NOT NULL,
  updatedAt   INTEGER NOT NULL,
  completedAt INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_runbook_run_state_key ON runbook_run_state(sessionId, runbookId);

-- 3.12 memories + FTS5 external-content recall index.
CREATE TABLE IF NOT EXISTS memories (
  id        TEXT PRIMARY KEY,
  content   TEXT NOT NULL,
  category  TEXT,
  source    TEXT NOT NULL DEFAULT 'assistant',
  -- TTL + provenance. expiresAt is an optional epoch-ms deadline (list/recall hide
  -- rows past it; NULL = never). runId records which turn created the row. kind is
  -- semantic (durable fact, default) vs episodic (session-scoped). sessionId
  -- namespaces episodic rows; NULL for semantic.
  expiresAt INTEGER,
  runId     TEXT,
  kind      TEXT NOT NULL DEFAULT 'semantic',
  sessionId TEXT,
  pinnedAt  INTEGER,
  deletedAt INTEGER,
  createdAt INTEGER NOT NULL,
  updatedAt INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_category ON memories(category, deletedAt);
CREATE INDEX IF NOT EXISTS idx_memories_pinned   ON memories(pinnedAt)
  WHERE pinnedAt IS NOT NULL AND deletedAt IS NULL;
-- Partial index on the TTL deadline so the list/recall expiry predicate doesn't
-- full-scan once the store grows (only live, expiring rows are indexed).
CREATE INDEX IF NOT EXISTS idx_memories_expires  ON memories(expiresAt)
  WHERE expiresAt IS NOT NULL AND deletedAt IS NULL;

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
  content, content='memories', content_rowid='rowid');

-- Triggers keep memories_fts in lockstep with memories.content. Soft-deleted rows
-- STAY indexed (recall filters via the JOIN's deletedAt IS NULL); the hard-delete
-- sweep relies on memories_ad to auto-evict (old.content matches the indexed value
-- because forget never mutates content).
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
  INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;

-- 3.13 artifacts — durable tool-result overflow payloads. When a serialized tool
-- result overflows the inline cap the session stashes the full JSON envelope here;
-- the in-memory ArtifactStore is a bounded hot cache (64 entries) and THIS table is
-- the fallback, so an artifact.read resolves even after the id was evicted from the
-- cache or written by a prior (now-restarted) session. Looked up by opaque id;
-- sessionId is provenance + the retention-sweep scope key.
CREATE TABLE IF NOT EXISTS artifacts (
  id         TEXT PRIMARY KEY,
  sessionId  TEXT NOT NULL,
  content    TEXT NOT NULL,
  totalChars INTEGER NOT NULL,
  totalBytes INTEGER NOT NULL,
  createdAt  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_artifacts_session ON artifacts(sessionId, createdAt);

-- 3.14 context_checkpoints — durable structured compaction checkpoint, reloaded on
-- resume. Exactly two rows: slot 'latest' (the current checkpoint) and slot 'prev'
-- (the immediately-preceding one, rotated in on each upsert) so a corrupt 'latest'
-- (unparseable payloadJson) falls back to the last valid checkpoint. payloadJson is
-- the full structured checkpoint object verbatim (opaque here, so a richer object
-- round-trips without a schema change); the promoted columns are what the resume path
-- reads directly. lastSeq is the conversation seq at the compaction boundary (resume
-- validates the checkpoint against the replayed delta). No projectId/sessionId: one
-- DB is one project, and a stale checkpoint is harmless (the conversation rows are the
-- authoritative transcript). The 2-row PK-keyed shape needs no index.
CREATE TABLE IF NOT EXISTS context_checkpoints (
  slot            TEXT PRIMARY KEY,   -- 'latest' | 'prev'
  compactionDepth INTEGER NOT NULL,
  summaryText     TEXT NOT NULL,
  lastRunId       TEXT,
  lastSeq         INTEGER NOT NULL,
  payloadJson     TEXT NOT NULL,
  createdAt       INTEGER NOT NULL
);

-- 3.15 async_invocations — runtime-owned async tool futures (terminal.run.async /
-- terminal.await.async). The tool call returns an immediate "accepted" handle;
-- the async coordinator polls the watched terminals to completion and publishes
-- the outcome to the attention queue (an autonomous wake). PROJECT-scoped: a
-- non-terminal row survives process boundaries and is adopted by the next
-- owner's coordinator (cockpit or supervisor daemon); a terminal row with a
-- NULL queueEventId is an unconfirmed publish the adopter retries (the stable
-- group DedupeKey makes the retry idempotent at the queue).
CREATE TABLE IF NOT EXISTS async_invocations (
  id              TEXT PRIMARY KEY,   -- asy_<8hex>
  toolName        TEXT NOT NULL,
  title           TEXT NOT NULL,
  groupId         TEXT NOT NULL,      -- sibling-coalescing key: the creating turn's run_… id (or the row id)
  sessionId       TEXT NOT NULL,
  terminalIdsJson TEXT NOT NULL,      -- JSON string[]
  command         TEXT,               -- terminal.run.async only
  status          TEXT NOT NULL DEFAULT 'starting',
  outcomesJson    TEXT,               -- JSON map terminalId -> {status,exitCode?,reason?}
  lastError       TEXT,
  queueEventId    TEXT,
  endedReason     TEXT,               -- 'user_cancelled' | 'session_cleared' | 'register_failed' | NULL
  createdAt       INTEGER NOT NULL,
  startedAt       INTEGER,
  expiresAt       INTEGER NOT NULL,   -- hard deadline
  finishedAt      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_async_invocations_live  ON async_invocations(status, createdAt);
CREATE INDEX IF NOT EXISTS idx_async_invocations_group ON async_invocations(groupId, status);

-- 3.17 workflow_graphs — the workflow-intelligence execution-graph layer
-- (client-owned durable DAGs; distinct from the flat workflow_runs ledger).
-- The typed graph is an opaque JSON snapshot (serialized by
-- internal/workflowgraph); status/goal are promoted for cheap list reads.
-- revision is the optimistic-concurrency counter: every snapshot write names
-- the revision it read and bumps it by one, so a stale backend patch can never
-- silently clobber newer local state. Project-scoped like watchers: rows
-- survive process boundaries; the next owner adopts them.
CREATE TABLE IF NOT EXISTS workflow_graphs (
  id            TEXT PRIMARY KEY,   -- wfg_<8hex>
  status        TEXT NOT NULL,
  goal          TEXT NOT NULL,
  schemaVersion INTEGER NOT NULL,
  revision      INTEGER NOT NULL,
  snapshotJson  TEXT NOT NULL,
  createdAt     INTEGER NOT NULL,
  updatedAt     INTEGER NOT NULL,
  completedAt   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_workflow_graphs_status ON workflow_graphs(status, updatedAt);

-- 3.18 workflow_events — append-only projection log per graph (plan created,
-- patch applied, evidence recorded, node transitioned, async settled). The
-- snapshot's in-graph evidence ring is bounded; THIS table is the full history
-- and the reconcile task's recent-events feed.
CREATE TABLE IF NOT EXISTS workflow_events (
  id          TEXT PRIMARY KEY,     -- wge_<8hex>
  workflowId  TEXT NOT NULL,
  revision    INTEGER NOT NULL,
  kind        TEXT NOT NULL,
  nodeId      TEXT,
  summary     TEXT NOT NULL,
  payloadJson TEXT NOT NULL,
  payloadHash TEXT NOT NULL,
  createdAt   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workflow_events_wf ON workflow_events(workflowId, createdAt);

-- 3.19 workflow_resource_links — the REVERSE index from an external resource
-- (terminal, watcher, async handle, worktree, branch, PR, …) back to the graph
-- (and optionally node) that owns it. An async completion or queue event
-- carrying only its own id maps to the right node through this table, which is
-- exactly what makes a wake turn continue the ORIGINAL workflow instead of
-- treating the event as an isolated notification.
CREATE TABLE IF NOT EXISTS workflow_resource_links (
  workflowId   TEXT NOT NULL,
  resourceType TEXT NOT NULL,
  resourceRef  TEXT NOT NULL,
  nodeId       TEXT,
  label        TEXT,
  status       TEXT,
  metadataJson TEXT NOT NULL DEFAULT '{}',
  createdAt    INTEGER NOT NULL,
  updatedAt    INTEGER NOT NULL,
  PRIMARY KEY (workflowId, resourceType, resourceRef)
);
CREATE INDEX IF NOT EXISTS idx_workflow_resource_ref ON workflow_resource_links(resourceType, resourceRef);

-- 3.20 workflow_reconcile_runs — forensic record of each backend reconcile
-- call: the revision it read, whether/where its patch landed, and bounded
-- input/output hashes for debug-log correlation.
CREATE TABLE IF NOT EXISTS workflow_reconcile_runs (
  id              TEXT PRIMARY KEY, -- wrc_<8hex>
  workflowId      TEXT NOT NULL,
  baseRevision    INTEGER NOT NULL,
  appliedRevision INTEGER,
  status          TEXT NOT NULL,
  inputHash       TEXT NOT NULL,
  outputHash      TEXT,
  warning         TEXT,
  createdAt       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workflow_reconcile_wf ON workflow_reconcile_runs(workflowId, createdAt);

-- 3.16 runtime_state — tiny durable key/value pairs the runtime must hand across
-- process boundaries for the persistent supervisor: the current conversation's
-- session id (so a detached daemon continues the SAME transcript) and the
-- per-session backend state token (opaque, server-signed; replaying it keeps
-- the backend's runbook-selection cadence stable across a handover). NOT a config
-- store — config resolution stays in internal/config.
CREATE TABLE IF NOT EXISTS runtime_state (
  key       TEXT PRIMARY KEY,
  value     TEXT NOT NULL,
  updatedAt INTEGER NOT NULL
);
