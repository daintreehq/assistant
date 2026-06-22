# Daintree Assistant — Phase 0 Contract Fixtures

Language-independent freeze of the public contracts for the Go rewrite. Every
section cites the authoritative TypeScript source. This is the **acceptance
baseline**: tool names, risk classes, env vars, command names, the JSONL/exit-code
vocabulary, the host protocol, magic constants, and the SQLite table list must all
match what is captured here.

Captured from the TS source on the `fix/resize-nuclear-redraw` branch.

---

## 1. Tool inventory (87 tools)

Source: `src/tools/*.ts` (`name`, `risk`, `parameters`), `src/schemas.ts` (`RiskClass`),
`src/tools/types.ts` (`ToolDef`).

**Risk classes** (`src/schemas.ts` → `RiskClass`, ordered): `read`, `local`, `ui`,
`terminal`, `project`, `git`, `external`, `system`.

**Tier gating** (`src/safety/policy.ts`): `supervisor` = read/local/ui; `operator`
= +terminal/project/external; `system` = +git/system. Mutating classes
(`ALWAYS_CONFIRM`) confirm for the interactive `main` actor; non-interactive actors
need a scoped automation grant.

All parameter objects are JSON-Schema `type:"object"`, `additionalProperties:false`
unless noted (the forge wrappers' inner `arguments` is `additionalProperties:true`).
`schema?` (Zod) is the runtime validator; the JSON Schema below is the model-facing
`parameters`. Handlers never throw — they return the `ToolResult` envelope.

### Result envelope (`src/tools/types.ts`, `src/schemas.ts`)

```
ToolResult<T> = { ok: bool, summary: string, result?: T, error?: ToolError, auditId?: string }
ToolError     = { code: string, message: string, recoverable: bool, details?: unknown }
ok(summary, result?)            -> { ok:true,  summary, result }
fail(code, message, {recoverable=true, details}) -> { ok:false, summary:message, error:{...} }
NO_ARGS = { type:"object", properties:{}, additionalProperties:false }
ToolActor = "main" | "watcher" | "timer" | "workflow" | "system"
```

### 1a. fs / context / artifact / audit (read-mostly)

| tool | risk | required | props (sketch) |
|---|---|---|---|
| `fs.list` | read | — | `path`, `depth` |
| `fs.read` | read | `path` | `path`, `maxBytes` |
| `fs.search` | read | `query` | `query`, `glob`, `maxResults` |
| `context.snapshot` | read | — | NO_ARGS |
| `terminal.read` | read | `terminalId` | `terminalId`, `maxLines`, `tailBytes` |
| `terminal.summarize` | read | `terminalId` | `terminalId`, `purpose`, `tailBytes` |
| `artifact.read` | read | `artifactId` | `artifactId`, `offset`, `limit` |
| `audit.export` | read | `format` | `format`(json\|csv), `actor`, `toolName`, `outcome`, `tsFrom`, `tsTo`, `limit` |

### 1b. timers / watchers / queue / grants / memory (local daemon state)

| tool | risk | required | props (sketch) |
|---|---|---|---|
| `timer.schedule` | local | `title`,`payload` | `title`, `fireAt`(ISO), `delayMs`, `repeat{everyMs*,maxRuns,until}`, `payload{type:enqueue\|call_safe_tool, message, toolCall{toolName*,args}}`, `target{projectId,worktreeId,terminalId,workflowRunId}` |
| `timer.list` | read | — | NO_ARGS |
| `timer.cancel` | local | `id` | `id` |
| `watcher.terminal.create` | local | `terminalIds`,`title`,`goal` | `terminalIds[]`, `title`, `goal`, `cadenceMs`, `startAfterMs`, `stopAfterMs`, `stopWhen`(WatchCondition), `alertWhen`(WatchCondition), `modelTier` |
| `watcher.watchPR` | local | `prNumber` | `prNumber`, `cwd`, `title`, `startAfterMs`, `stopAfterMs` |
| `watcher.list` | read | — | NO_ARGS |
| `watcher.cancel` | local | `id` | `id` |
| `queue.publish` | local | `source`,`severity`,`title`,`summary` | `source`(EventSource), `severity`(Severity), `title`, `summary`, `target`, `evidence`, `recommendedActions[]{label*,toolName*,args,risk,requiresConfirmation}`, `dedupeKey`, `ttlMs` |
| `queue.digest` | read | — | `severityAtLeast`, `maxItems`, `includeResolved` |
| `queue.resolve` | local | `id` | `id` |
| `grant.create` | local | `actorId`,`actorType`,`ttlMs`,`maxUses` | `actorId`, `actorType`(watcher\|timer), `allowedRiskClasses[]`, `allowedToolNames[]`, `ttlMs`, `maxUses` |
| `grant.list` | read | — | `actorId` |
| `grant.revoke` | local | `id` | `id` |
| `memory.recall` | read | `query` | `query`, `category`, `limit`, `pinnedOnly` |
| `memory.list` | read | — | `category`, `pinnedOnly`, `limit` |
| `memory.save` | local | `content` | `content`, `category`, `source` |
| `memory.forget` | local | `id` | `id` (mem_…) |
| `memory.pin` | local | `id` | `id` |
| `memory.unpin` | local | `id` | `id` |

`WatchCondition` (`src/schemas.ts`) is a recursive union: leaves `{stateIs}`,
`{runtimeStatusIs}`, `{contains}`, `{regex}`, `{noOutputForMs}`, `{modelJudge}`;
combinators `{all:[…]}`, `{any:[…]}`, `{not:…}`.

### 1c. skills / workflow runs (local)

| tool | risk | required | props (sketch) |
|---|---|---|---|
| `skill.find` | read | `query` | `query` |
| `skill.load` | read | `skillId` | `skillId` |
| `skill.run.get` | read | `skillId` | `skillId` |
| `skill.step.advance` | local | `skillId`,`completedStep` | `skillId`, `completedStep`, `nextStep`, `status`(done\|skipped), `notes` |
| `workflow.create` | local | — | issue/branch/worktree/PR/terminal/watcher fields (free-form workflow_run patch) |
| `workflow.get` | read | `id` | `id` (wfr_…) |
| `workflow.list` | read | — | `status` |
| `workflow.update` | local | `id` | `id` + patch fields |

### 1d. terminal extraction (read / local)

| tool | risk | required | props (sketch) |
|---|---|---|---|
| `terminal.extract` | read | `terminalIds` | `terminalIds[]`, `instruction` |
| `terminal.extract.async` | local | `terminalIds`,`instruction` | `terminalIds[]`, `instruction`, `title`, `verdictInstruction`, `dedupeKey`, `ttlMs` |

### 1e. agent task (project)

| tool | risk | required | props (sketch) |
|---|---|---|---|
| `agentTask.spawnForEdits` | project | (see schema) | `worktreeId`, `agentId`, `mode`(edit\|explore), `title`, `taskPrompt`, `acceptanceCriteria`, `context`, `watcher`, `create` |

This is the ONLY path to file edits (no-file-edit invariant). Idempotent spawn saga
keyed on a deterministic `idempotencyKey`; stages: `launch_requested → agent_started
→ terminal_bound → watcher_attached → confirmed | failed | ambiguous`
(`src/schemas.ts` → `AgentLaunchStage`; durable in `agent_launches`).

### 1f. Daintree MCP wrappers (`src/tools/mcpTools.ts`)

Core / discovery:

| tool | risk | required | props |
|---|---|---|---|
| `daintree.status` | read | — | NO_ARGS |
| `daintree.listTools` | read | — | NO_ARGS |
| `tool.search` | read | `query` | `query`, `max` |
| `daintree.call` | system | `name` | `name`, `arguments`, `requestKey` (raw MCP passthrough) |

Recipes / worktrees / copyTree:

| tool | risk | required | props |
|---|---|---|---|
| `recipe.list` | read | — | `arguments`, `requestKey` |
| `recipe.run` | project | `recipeId` | `recipeId`, `arguments`, `terminalId`, `requestKey` |
| `worktree.createWithRecipe` | project | `arguments` | `arguments`, `terminalId`, `worktreeId`, `requestKey` |
| `copyTree.generate` | read | — | `worktreeId`, `terminalId`, `command`, `options` |
| `copyTree.injectToTerminal` | terminal | `terminalId` | `terminalId`, `worktreeId`, `options` |
| `copyTree.generateAndCopyFile` | system | — | `worktreeId`, `options`, `arguments` |

Terminal control / focus (ui & terminal):

| tool | risk | required | props |
|---|---|---|---|
| `terminal.focus` | ui | `terminalId` | `terminalId`, `worktreeId`, `options` |
| `terminal.sendCommand` | terminal | `terminalId`,`command` | `terminalId`, `command` |
| `terminal.arm` | terminal | `terminalId` | `terminalId` |
| `terminal.disarm` | terminal | `terminalId` | `terminalId`, `worktreeId`, `options` |
| `terminal.disarmAll` | terminal | — | NO_ARGS |
| `agent.focusNextWaiting` | ui | — | NO_ARGS |
| `agent.focusNextWorking` | ui | — | NO_ARGS |
| `agent.focusNextAgent` | ui | — | NO_ARGS |
| `agent.focusPreviousAgent` | ui | — | NO_ARGS |
| `workflow.focusNextAttention` | ui | — | NO_ARGS |

Git snapshot (git):

| tool | risk | required | props |
|---|---|---|---|
| `git.snapshotRevert` | git | `worktreeId` | `worktreeId`, `arguments` |
| `git.snapshotDelete` | git | `worktreeId` | `worktreeId`, `arguments` |

Forge reads (read) — all forward via an `arguments` passthrough object
(`additionalProperties:true`); `forge.getPR` takes `{cwd, prNumber}`:

`forge.listIssues`, `forge.getIssue`, `forge.listPRs`, `forge.getPR`.

Forge writes (all **external**, all confirm; generated by `forgeWrite(...)` —
each takes a typed body + `requestKey`):

`forge.createIssue`, `forge.closeIssue`, `forge.reopenIssue`, `forge.editIssue`,
`forge.addIssueComment`, `forge.addIssueLabel`, `forge.removeIssueLabel`,
`forge.assignIssue`, `forge.unassignIssue`, `forge.createPR`, `forge.closePR`,
`forge.reopenPR`, `forge.mergePR`, `forge.convertPRToDraft`,
`forge.markPRReadyForReview`, `forge.commentOnPR`, `forge.editPR`,
`forge.approvePR`, `forge.requestChanges`, `forge.dismissReview`,
`forge.requestReviewers`.

Workflow orchestration (external):

| tool | risk | required | props |
|---|---|---|---|
| `workflow.startWorkOnIssue` | external | `arguments` | `arguments`, `attachWatcher`, `requestKey` |
| `workflow.prepBranchForReview` | external | `arguments` | `arguments`, `requestKey` |

### Risk-class tally (acceptance count)

read 29 · local 17 · ui 6 · terminal 5 · project 3 · git 2 · external 23 · system 2 → **87 total**.
(external = 21 forge writes + 2 workflow.* orchestration; system = `daintree.call`
and `copyTree.generateAndCopyFile`; `grant.create` is **local**.)

### No-file-edit guard (`src/safety/policy.ts`)

`assertNoFileEditTools` rejects any registered name containing a forbidden fragment
(case-insensitive `includes`):
`write_file`, `writefile`, `apply_patch`, `applypatch`, `edit_file`, `editfile`,
`fs.write`, `fs.edit`, `file.write`, `file.edit`, `patch.apply`.

---

## 2. Environment variables

Source: `src/config.ts` (`loadConfig`, `DEFAULTS`). Resolution helper `firstString`
returns the first non-empty trimmed value.

**Precedence (highest → lowest):** CLI overrides → real `process.env` → project
`.env` (`<projectPath>/.env`) → assistant's own `.env` fallback → `DEFAULTS`.
`dotenv` never overrides an already-set var, so real env and the project `.env`
always win over the own-env fallback.

**Trusted-env-only** (snapshotted from `process.env` BEFORE any `.env` load; a bound
project's `.env` must NOT be able to set these — privilege escalation / exfiltration):

- `DAINTREE_ASSISTANT_TIER` (default `system`; invalid → fail-closed to `supervisor`)
- `DAINTREE_ASSISTANT_AUTO_APPROVE` (`"1"`)
- `DAINTREE_ASSISTANT_OFFLINE` (`"1"`)
- `DAINTREE_ASSISTANT_STATE_DIR`
- `DAINTREE_ASSISTANT_LOG_DIR`

**Merged-env OK** (read from `process.env` after `.env` load — cosmetic or
log-into-home-only):

- `FIREWORKS_API_KEY` (required)
- `FIREWORKS_BASE_URL` (default `https://api.fireworks.ai/inference/v1`)
- `DAINTREE_LARGE_MODEL` (default `accounts/fireworks/models/glm-5p2`)
- `DAINTREE_MEDIUM_MODEL` (default same as large)
- `DAINTREE_SMALL_MODEL` (default `accounts/fireworks/models/deepseek-v4-flash`)
- `DAINTREE_MCP_URL` (default MCP url `http://127.0.0.1:45454/mcp`)
- `DAINTREE_MCP_TOKEN`
- `DAINTREE_PROJECT_ID` (drives per-project state dir via `projectIdToDir`)
- `DAINTREE_WINDOW_ID` (read & surfaced; does not yet affect path; reservedColumns→2)
- `DAINTREE_ASSISTANT_DEBUG_LOG` (`"1"`; may come from own `.env` — safe, logs only to trusted logDir)
- `DAINTREE_ASSISTANT_NO_SPLASH` (`"1"` disables splash; on by default)
- `DAINTREE_ASSISTANT_RESERVED_COLUMNS` (int; floored at 1; default 2 embedded / 1 bare)

State root: `~/.daintree/assistant-cli/[<project-slug>-<sha8>]`, db at
`<stateDir>/state.db`. Log dir default `~/.daintree/logs` (global, not per-project).

CLI `--tier` maps to `DAINTREE_ASSISTANT_TIER`; `windowId` is never accepted as a flag.

---

## 3. Slash commands

Source: `src/commandRegistry.ts` (`COMMAND_REGISTRY` — single source of truth),
handlers in `src/cli/commands.ts` (REPL) and `src/cli/commandData.ts` (cockpit).

Canonical commands (order is the help-surface order):

`/status`, `/inbox [sev]`, `/tools [query]`, `/timers`, `/watchers`,
`/audit [n]` (default 15; `export <json|csv>`), `/explain [runId]`, `/models`,
`/permissions [tier]` (supervisor|operator|system), `/skills [sub]`
(loaded|find <query>|load <id…>|clear), `/compact`, `/clear`, `/doctor`,
`/reconnect`, `/help`, `/quit`.

**Aliases** (handled in both switch statements, not listed as user-facing entries):
- `/help` ⇔ `/?`
- `/quit` ⇔ `/exit`, `/q`

UI-only on-demand views (not slash commands; `src/ui`): `^O` / `/panel` opens the
Operations/help view, Esc returns (per CLAUDE.md UI boundary).

---

## 4. One-shot JSONL event vocabulary + exit codes

Source: `src/schemas.ts` (`JsonlEventType`, `JsonlEventSchema`, `JsonResultEnvelopeSchema`,
`ONE_SHOT_EXIT_CODE`, `JSON_OUTPUT_SCHEMA_VERSION`), emitted by `src/cli/jsonSink.ts`.

`JSON_OUTPUT_SCHEMA_VERSION = 1`.

**Event types** (`type` string on each JSONL line — reuse the durable RunEvent
vocabulary so live stream and DB replay agree):

`assistant:start`, `assistant:content`, `assistant:end`, `assistant:cancelled`,
`tool:call`, `tool:result`, `error`, `info`, `result`.

**Common fields on every line** (`JsonlEventSchema`, `passthrough`): `type`, `ts`
(number), `seq` (int ≥ 0, monotonic per run starting at 0). Per-type payload fields
ride alongside.

**Terminal `result` line** (`JsonResultEnvelopeSchema`, strict — last line of every
`--json` run):
```
{ type:"result", ts, seq, schemaVersion:1,
  status: "success"|"error"|"cancelled",
  exitCode: 0|1|2,
  content: string,                     // final assistant text ("" if none)
  error: { message:string } | null }   // present only when status="error"
```

**Exit-code mapping** (`ONE_SHOT_EXIT_CODE`):
- `0 success` — turn completed and assistant replied.
- `1 error` — model/general error (stream error, max iterations, unexpected throw); also process-wide catch-all.
- `2 cancelled` — turn cancelled mid-flight.
- `3 toolFailure` — **RESERVED**, never emitted today (failed tool calls feed back as recoverable context; a turn ending after a tool error still exits 0). Kept stable so a future loop change can adopt it without renumbering.

`JsonOutputStatus = "success" | "error" | "cancelled"`.

---

## 5. Host protocol (Electron utility-process)

Source: `src/host/protocol.ts` (hand-mirror of Daintree
`shared/types/ipc/assistantHost.ts` + `mcpServer.ts`).

`PROTOCOL_VERSION = 1` (must equal Daintree's `ASSISTANT_HOST_PROTOCOL_VERSION`;
Daintree Zod-validates and refuses unknown versions).

**SessionDescriptor** (first `parentPort` message; carries NO secret — MCP url/token/
windowId arrive via env): `{ sessionId, windowId:number, projectId, cwd, tier,
protocolVersion, resumeSessionId? }`.

**Host → Daintree events** (`HostEvent` union, discriminated by `type`):
- `host:ready` `{ sessionId, protocolVersion, resumedSessionId? }`
- `turn:start` `{ sessionId, turnId, role, startedAt }`
- `turn:token` `{ sessionId, turnId, chunk }`
- `turn:end` `{ sessionId, turnId, endedAt, outcome? }`
- `tool:started` `{ sessionId, toolCallId, toolId, argsSummary, startedAt, turnId?, danger }`
- `tool:settled` `{ sessionId, toolCallId, toolId, durationMs, result, severity, errorCode?, turnId? }`
- `approval:requested` `{ sessionId, approvalId, toolId, summary, requestedAt, turnId? }`
- `approval:decided` `{ sessionId, approvalId, decision, decidedAt }`
- `host:error` `{ sessionId, code, message }`
- `host:shutdown` `{ sessionId, reason, resumeSessionId? }`

**Daintree → host commands** (`HostCommand` union; `HostCommandType`):
- `prompt` `{ sessionId, text }`
- `approval:decide` `{ sessionId, approvalId, decision }`
- `interrupt` `{ sessionId }`
- `hibernate` `{ sessionId }`
- `shutdown` `{ sessionId }`

**Audit-aligned vocabularies** (mirror `mcpServer.ts`):
- `AuditResult` = `success | error | confirmation-pending | unauthorized | dedup | collision | rate_limited`
- `AuditSeverity` = `info | notice | warning | error | critical`
- `ConfirmationDecision` = `approved | rejected | timeout`
- `HostShutdownReason` = `hibernate | revoke | error | exit`
- `TurnRole` = `user | assistant`
- `TurnOutcomeClass` = `answered | hedged | refused | docs-empty | tier-rejected | mcp-not-ready | agent-stuck | tool-error | reasoning-loop | hibernate-resume-stale | cancelled | unknown`

`SEVERITY_BY_RESULT` (`severityForResult`): success→info, dedup→info,
confirmation-pending→notice, unauthorized→warning, rate_limited→warning,
collision→warning, error→error.

---

## 6. Magic constants / limits / cadences

### Agent loop (`src/agent/loop.ts`)
- `MAX_TOOL_ITERATIONS = 12` (per turn; over → "Reached the tool-iteration limit…", exit 1)
- `REPEAT_FAILURE_WARN = 2`, `REPEAT_FAILURE_ABORT = 3` (identical repeated tool failures → warn / abort breaker)
- `CONTROL_MESSAGE_COUNT = 3` (dynamic control messages after the cached prefix)
- `MAX_TOOL_RESULT_CHARS = 8000` (inline tool-result cap; overflow → artifact)
- `TRUNCATION_PREVIEW_CHARS = 1500`, `TRUNCATION_SUMMARY_CHARS = 500` (overflow stub sizes)
- `MAX_STORED_ARTIFACTS = 64` (session artifact store cap; LRU evict)
- `AUTO_COMPACT_TOKEN_THRESHOLD = 60_000` (estimated tokens → auto-compact before a turn)
- `CHARS_PER_TOKEN = 4` (token estimate divisor)
- `MAIN_PROMPT_CACHE_KEY = "daintree-main"` (Fireworks `prompt_cache_key`; plain, **unversioned** — groups requests onto a cache node, never a version)
- `CANCELLED_REPLY = "Turn cancelled"`
- `CLEAR_MARKER = "[conversation cleared — context reset to initial state]"`
- `SKILL_CONTEXT_MUTATING_TOOLS = {skill.find, skill.load}`

### Scheduler / watcher cadence (`src/watcherCadence.ts`, `src/daemon/scheduler.ts`)
- `SCHEDULER_TICK_MS = 3_000` (daemon tick; also the floor for any cadence)
- `SUPERVISOR_DEFAULT_CADENCE_MS = 3_000`
- `MONITOR_DEFAULT_CADENCE_MS = 120_000`
- `PR_WATCHER_CADENCE_MS = 60_000` (fixed, not user-configurable)
- `WATCHER_SPAWN_GRACE_MS = 20_000` (terminal-absent grace right after launch)
- Effective check interval = `max(cadenceMs, SCHEDULER_TICK_MS)`.

### Watcher engine (`src/daemon/watcherEngine.ts`)
- `RATE_LIMIT_COOLDOWN_MS = 60_000`
- `JUDGE_CONFIDENCE_FLOOR = 0.6`
- Deterministic-signal confidences in code: read-fail 0.4, rate-limit 0.9, exit-code paths 0.85–0.95, spawn-grace 0.5, etc.

### Splash (`src/ui/splash/frames.ts`, `src/ui/components/StartupSplash.tsx`)
- `SPLASH_WIDTH = 48`, `SPLASH_HEIGHT = 18`
- `fps = 28` (default), `lingerMs = 420` (default)
- gradient endpoints `TOP = #8FEBC4`, `BASE = #36CE94`; suppressed when `columns <= SPLASH_WIDTH`.

### DB retention defaults (`src/storage/db.ts`, `DAY_MS = 86_400_000`)
- `auditLogMaxAgeMs = 30d`, `auditLogKeepRows = 5000`
- `runEventsMaxAgeMs = 14d`, `runEventsKeepRuns = 500`
- `conversationMaxAgeMs = 90d`, `conversationKeepRows = 1000`
- `skillSelLogMaxAgeMs = 30d`, `skillSelLogKeepRows = 500`
- `eventsTerminalAgeMs = 7d`, `memoriesDeletedAgeMs = 30d`

### Debug log (`src/config.ts`, `src/debugLog.ts`)
- Default dir `~/.daintree/logs`; deletes logs older than **7 days** at boot;
  per-session file `<date>-<sessionId>.log`.

### Model tiers (`src/config.ts` DEFAULTS / `src/schemas.ts` ModelTier)
- `ModelTier = small | medium | large`; medium routes to large in v1.
- large/medium = `accounts/fireworks/models/glm-5p2`; small = `deepseek-v4-flash`.

---

## 7. SQLite tables

Source: `src/storage/db.ts` (`SCHEMA`). Runtime-adaptive driver
(`src/storage/sqliteDriver.ts`): `bun:sqlite` under Bun, `node:sqlite` under Node.
Single `CREATE TABLE IF NOT EXISTS` schema (dev policy: hard-reset, no migration chain).

Tables: `timers`, `watchers`, `events`, `audit_log`, `run_events`, `conversation`,
`skill_selection_log`, `automation_grants`, `workflow_runs`, `agent_launches`,
`skill_run_state`, `memories`.

**JSON-bearing columns** (serialized JSON in a `TEXT` column — suffix convention
`…Json`, plus `run_events.payload` and `skill_run_state.stepsJson`):

- `timers`: `payloadJson`, `targetJson`
- `watchers`: `targetsJson`, `stopWhenJson`, `alertWhenJson`, `optionsJson`
- `events`: `targetJson`, `evidenceJson`, `recommendedActionsJson`
- `audit_log`: `argsJson`, `resultJson`
- `run_events`: `payload`
- `conversation`: `toolCallsJson`
- `skill_selection_log`: `selectedSkillIdsJson`
- `automation_grants`: `allowedRiskClassesJson`, `allowedToolNamesJson`
- `workflow_runs`: `terminalIdsJson`, `watcherIdsJson`, `queueEventIdsJson`, `nextActionJson`, `notesJson`
- `skill_run_state`: `stepsJson`
- (`agent_launches`, `memories` carry no JSON columns)

Session-scoped reconciliation on `Db` construction: `cancelStaleWatchers` (watchers
non-terminal → cancelled), `cancelStaleAgentLaunches` (non-terminal `agent_launches`
→ failed). Timers persist across sessions; watchers/launches do not.

---

### Source map (one line per section)

1. tools — `src/tools/*.ts`, `src/tools/types.ts`, `src/schemas.ts`, `src/safety/policy.ts`
2. env — `src/config.ts`
3. commands — `src/commandRegistry.ts`, `src/cli/commands.ts`, `src/cli/commandData.ts`
4. JSONL/exit — `src/schemas.ts`, `src/cli/jsonSink.ts`
5. host — `src/host/protocol.ts`
6. constants — `src/agent/loop.ts`, `src/watcherCadence.ts`, `src/daemon/watcherEngine.ts`, `src/ui/splash/*`, `src/ui/components/StartupSplash.tsx`, `src/storage/db.ts`, `src/config.ts`
7. tables — `src/storage/db.ts`, `src/storage/sqliteDriver.ts`
