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

-- 3.2 watchers — session-scoped supervisors. NB: insert path supplies 'active';
-- the column DEFAULT is aligned to 'active' here (clean schema, no confusion).
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
  -- WHY a watcher reached a terminal 'cancelled' status, so a session-boundary
  -- teardown ('session_ended', stamped by cancelStaleWatchers on the next open) is
  -- distinguishable from a deliberate user cancel ('user_cancelled', stamped by
  -- watcher.cancel). NULL on active rows and on natural terminal states
  -- (condition_met/timeout/error). endedAt is the epoch-ms of that cancel.
  endedReason        TEXT,
  endedAt            INTEGER,
  -- Back-link to the durable workflow_runs ledger row a supervisor watcher drives.
  -- NULL for non-supervisor / manually-created watchers. When set, the daemon advances
  -- that row's status as the watcher reaches a terminal state.
  workflowRunId      TEXT
);

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

-- 3.6 conversation — session-fresh transcript.
CREATE TABLE IF NOT EXISTS conversation (
  id            TEXT PRIMARY KEY,
  sessionId     TEXT NOT NULL,
  seq           INTEGER NOT NULL,
  role          TEXT NOT NULL,
  content       TEXT NOT NULL,
  toolCallsJson TEXT,
  toolCallId    TEXT,
  createdAt     INTEGER NOT NULL
);
-- UNIQUE so a (sessionId, seq) collision is rejected at the storage layer, not
-- merely caught on read by the rehydrator (mirrors run_events' idx_run_events_run).
CREATE UNIQUE INDEX IF NOT EXISTS idx_conv_session   ON conversation(sessionId, seq);
CREATE INDEX IF NOT EXISTS idx_conv_createdat ON conversation(createdAt);

-- 3.7 skill_selection_log — selector diagnostics.
CREATE TABLE IF NOT EXISTS skill_selection_log (
  id                   TEXT PRIMARY KEY,
  ts                   INTEGER NOT NULL,
  sessionId            TEXT NOT NULL,
  userInput            TEXT NOT NULL,
  selectedSkillIdsJson TEXT NOT NULL,
  confidence           REAL NOT NULL,
  taskType             TEXT,
  reason               TEXT
);
CREATE INDEX IF NOT EXISTS idx_skill_sel_ts ON skill_selection_log(ts);

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

-- 3.11 skill_run_state — stepwise skill progress (natural key sessionId+skillId).
CREATE TABLE IF NOT EXISTS skill_run_state (
  id          TEXT PRIMARY KEY,
  sessionId   TEXT NOT NULL,
  skillId     TEXT NOT NULL,
  currentStep INTEGER NOT NULL DEFAULT 0,
  stepsJson   TEXT NOT NULL DEFAULT '[]',
  status      TEXT NOT NULL DEFAULT 'active',
  startedAt   INTEGER NOT NULL,
  updatedAt   INTEGER NOT NULL,
  completedAt INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_run_state_key ON skill_run_state(sessionId, skillId);

-- 3.12 memories + FTS5 external-content recall index.
CREATE TABLE IF NOT EXISTS memories (
  id        TEXT PRIMARY KEY,
  content   TEXT NOT NULL,
  category  TEXT,
  source    TEXT NOT NULL DEFAULT 'assistant',
  pinnedAt  INTEGER,
  deletedAt INTEGER,
  createdAt INTEGER NOT NULL,
  updatedAt INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_category ON memories(category, deletedAt);
CREATE INDEX IF NOT EXISTS idx_memories_pinned   ON memories(pinnedAt)
  WHERE pinnedAt IS NOT NULL AND deletedAt IS NULL;

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
